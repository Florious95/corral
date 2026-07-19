//go:build darwin

package main

import (
	"errors"
	"log"
	"sync"

	"golang.org/x/sys/unix"
)

type darwinV2ProcessWatcher struct {
	fd        int
	events    chan int
	mu        sync.Mutex
	watched   map[int]bool
	closeOnce sync.Once
}

func newV2ProcessWatcher() (v2ProcessWatcher, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	watcher := &darwinV2ProcessWatcher{fd: fd, events: make(chan int, 64), watched: map[int]bool{}}
	go watcher.read()
	return watcher, nil
}

func (watcher *darwinV2ProcessWatcher) Events() <-chan int { return watcher.events }

func (watcher *darwinV2ProcessWatcher) Set(pids []int) error {
	wanted := map[int]bool{}
	for _, pid := range pids {
		if pid > 0 {
			wanted[pid] = true
		}
	}
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	for pid := range watcher.watched {
		if wanted[pid] {
			continue
		}
		log.Printf("v2 process watcher: EV_DELETE pid=%d", pid)
		change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_DELETE}
		if _, err := unix.Kevent(watcher.fd, []unix.Kevent_t{change}, nil, nil); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ESRCH) {
			return err
		}
		delete(watcher.watched, pid)
	}
	for pid := range wanted {
		if watcher.watched[pid] {
			continue
		}
		log.Printf("v2 process watcher: EV_ADD filter=EVFILT_PROC fflags=NOTE_EXIT pid=%d", pid)
		change := unix.Kevent_t{
			Ident: uint64(pid), Filter: unix.EVFILT_PROC,
			Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
			Fflags: unix.NOTE_EXIT,
		}
		if _, err := unix.Kevent(watcher.fd, []unix.Kevent_t{change}, nil, nil); err != nil {
			if errors.Is(err, unix.ESRCH) {
				log.Printf("v2 process watcher: pid=%d exited before NOTE_EXIT registration", pid)
				select {
				case watcher.events <- pid:
				default:
				}
				continue
			}
			return err
		}
		watcher.watched[pid] = true
	}
	return nil
}

func (watcher *darwinV2ProcessWatcher) read() {
	events := make([]unix.Kevent_t, 16)
	for {
		count, err := unix.Kevent(watcher.fd, nil, events, nil)
		if err != nil {
			return
		}
		for _, event := range events[:count] {
			pid := int(event.Ident)
			log.Printf("v2 process watcher: NOTE_EXIT pid=%d", pid)
			watcher.mu.Lock()
			delete(watcher.watched, pid)
			watcher.mu.Unlock()
			watcher.events <- pid
		}
	}
}

func (watcher *darwinV2ProcessWatcher) Close() error {
	var err error
	watcher.closeOnce.Do(func() { err = unix.Close(watcher.fd) })
	return err
}
