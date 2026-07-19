//go:build darwin

package main

import (
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type darwinV2SocketWatcher struct {
	dir       string
	dirFD     int
	kqueueFD  int
	events    chan string
	done      chan struct{}
	known     map[string]bool
	kevent    func(int, []unix.Kevent_t, []unix.Kevent_t, *unix.Timespec) (int, error)
	fdMu      sync.Mutex
	closeOnce sync.Once
}

var v2SocketDialLive = func(path string) bool {
	connection, err := net.DialTimeout("unix", path, 25*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func newV2SocketWatcher(dir string) (v2SocketWatcher, error) {
	return newDarwinV2SocketWatcher(dir, unix.Kevent)
}

func newDarwinV2SocketWatcher(dir string, kevent func(int, []unix.Kevent_t, []unix.Kevent_t, *unix.Timespec) (int, error)) (v2SocketWatcher, error) {
	dirFD, err := unix.Open(dir, unix.O_EVTONLY, 0)
	if err != nil {
		return nil, err
	}
	kqueueFD, err := unix.Kqueue()
	if err != nil {
		_ = unix.Close(dirFD)
		return nil, err
	}
	if err := registerV2SocketKqueue(kqueueFD, dirFD, kevent); err != nil {
		_ = unix.Close(kqueueFD)
		_ = unix.Close(dirFD)
		return nil, err
	}
	watcher := &darwinV2SocketWatcher{
		dir: dir, dirFD: dirFD, kqueueFD: kqueueFD, events: make(chan string, 64), done: make(chan struct{}), known: map[string]bool{}, kevent: kevent,
	}
	if candidates, ok := listTmuxSocketCandidates(dir); ok {
		for _, path := range candidates {
			if strings.HasSuffix(path, ".lock") {
				continue
			}
			watcher.known[path] = true
		}
	}
	go watcher.read()
	return watcher, nil
}

func registerV2SocketKqueue(kqueueFD, dirFD int, kevent func(int, []unix.Kevent_t, []unix.Kevent_t, *unix.Timespec) (int, error)) error {
	change := unix.Kevent_t{
		Ident: uint64(dirFD), Filter: unix.EVFILT_VNODE, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
		Fflags: unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_RENAME | unix.NOTE_DELETE,
	}
	_, err := kevent(kqueueFD, []unix.Kevent_t{change}, nil, nil)
	return err
}

func (watcher *darwinV2SocketWatcher) Events() <-chan string { return watcher.events }

func (watcher *darwinV2SocketWatcher) Close() error {
	var closeErr error
	watcher.closeOnce.Do(func() {
		close(watcher.done)
		watcher.fdMu.Lock()
		defer watcher.fdMu.Unlock()
		if err := unix.Close(watcher.kqueueFD); err != nil {
			closeErr = err
		}
		if err := unix.Close(watcher.dirFD); err != nil && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

func (watcher *darwinV2SocketWatcher) read() {
	events := make([]unix.Kevent_t, 1)
	for {
		watcher.fdMu.Lock()
		kqueueFD := watcher.kqueueFD
		watcher.fdMu.Unlock()
		if _, err := watcher.kevent(kqueueFD, nil, events, nil); err != nil {
			select {
			case <-watcher.done:
				return
			default:
				if err == unix.EINTR || err == unix.EAGAIN {
					log.Printf("v2 socket watcher: kqueue read transient error=%q; retrying", err.Error())
					continue
				}
				log.Printf("BINDING INCIDENT: type=socket_watcher_exit error=%q action=restarting", err.Error())
				if restartErr := watcher.restartKqueue(); restartErr != nil {
					log.Printf("BINDING INCIDENT: type=socket_watcher_exit error=%q action=restart_failed retry_after=250ms", restartErr.Error())
					time.Sleep(250 * time.Millisecond)
				} else {
					log.Printf("BINDING INCIDENT: type=socket_watcher_exit action=restarted")
					watcher.scanUntilSettled()
				}
				continue
			}
		}
		watcher.scanUntilSettled()
	}
}

func (watcher *darwinV2SocketWatcher) scanUntilSettled() {
	var pending []string
	for attempt := 0; attempt < 100; attempt++ {
		pending = watcher.scan()
		if len(pending) == 0 {
			break
		}
		if attempt == 0 {
			log.Printf("v2 socket watcher: new socket not live yet paths=%q", pending)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pending) > 0 {
		log.Printf("v2 socket watcher: new socket still not live after retry paths=%q", pending)
	}
}

func (watcher *darwinV2SocketWatcher) restartKqueue() error {
	kqueueFD, err := unix.Kqueue()
	if err != nil {
		return err
	}
	watcher.fdMu.Lock()
	dirFD := watcher.dirFD
	watcher.fdMu.Unlock()
	if err := registerV2SocketKqueue(kqueueFD, dirFD, watcher.kevent); err != nil {
		_ = unix.Close(kqueueFD)
		return err
	}
	select {
	case <-watcher.done:
		_ = unix.Close(kqueueFD)
		return unix.EBADF
	default:
	}
	watcher.fdMu.Lock()
	oldFD := watcher.kqueueFD
	watcher.kqueueFD = kqueueFD
	watcher.fdMu.Unlock()
	_ = unix.Close(oldFD)
	return nil
}

func (watcher *darwinV2SocketWatcher) scan() []string {
	candidates, ok := listTmuxSocketCandidates(watcher.dir)
	if !ok {
		return nil
	}
	present := make(map[string]bool, len(candidates))
	var pending []string
	for _, path := range candidates {
		if strings.HasSuffix(path, ".lock") {
			continue
		}
		present[path] = true
		if watcher.known[path] {
			continue
		}
		if !v2SocketDialLive(path) {
			pending = append(pending, path)
			continue
		}
		watcher.known[path] = true
		select {
		case watcher.events <- path:
		case <-watcher.done:
			return nil
		default:
			log.Printf("v2 socket watcher: event queue full path=%q", path)
		}
	}
	for path := range watcher.known {
		if !present[path] {
			delete(watcher.known, path)
		}
	}
	return pending
}
