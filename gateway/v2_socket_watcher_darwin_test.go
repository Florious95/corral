//go:build darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinV2SocketWatcherReportsNewLiveSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "v2-socket-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	watcher, err := newV2SocketWatcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	path := filepath.Join(dir, "new-tmux")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	select {
	case got := <-watcher.Events():
		if got != path {
			t.Fatalf("socket=%q want=%q", got, path)
		}
	case <-time.After(time.Second):
		t.Fatal("kqueue directory watcher did not report new live socket")
	}
}

func TestDarwinV2SocketWatcherRetriesEarlyEventUntilSocketIsLive(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "v2-socket-watch-delayed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "delayed-tmux")
	originalDialLive := v2SocketDialLive
	attempts := 0
	v2SocketDialLive = func(candidate string) bool {
		if candidate != path {
			return originalDialLive(candidate)
		}
		attempts++
		return attempts >= 4
	}
	watcher, err := newV2SocketWatcher(dir)
	if err != nil {
		v2SocketDialLive = originalDialLive
		t.Fatal(err)
	}
	defer func() {
		_ = watcher.Close()
		v2SocketDialLive = originalDialLive
	}()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-watcher.Events():
		if got != path {
			t.Fatalf("socket=%q want=%q", got, path)
		}
		if attempts != 4 {
			t.Fatalf("dial attempts=%d want=4", attempts)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not retry the early directory event until the socket became live")
	}
}

func TestDarwinV2SocketWatcherIgnoresTmuxLockFiles(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "v2-socket-watch-lock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	originalDialLive := v2SocketDialLive
	v2SocketDialLive = func(path string) bool {
		if filepath.Ext(path) == ".lock" {
			t.Fatalf("dialed tmux lock file %q", path)
		}
		return originalDialLive(path)
	}
	watcher, err := newV2SocketWatcher(dir)
	if err != nil {
		v2SocketDialLive = originalDialLive
		t.Fatal(err)
	}
	defer func() {
		_ = watcher.Close()
		v2SocketDialLive = originalDialLive
	}()
	if err := os.WriteFile(filepath.Join(dir, "new-tmux.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "new-tmux")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	select {
	case got := <-watcher.Events():
		if got != path {
			t.Fatalf("socket=%q want=%q", got, path)
		}
	case <-time.After(time.Second):
		t.Fatal("lock file blocked the live socket event")
	}
}

func TestDarwinV2SocketWatcherContinuesAfterInterruptedKqueueWait(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "v2-socket-watch-eintr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var readCalls atomic.Int32
	kevent := func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		if len(changes) == 0 {
			if readCalls.Add(1) == 1 {
				return 0, unix.EINTR
			}
		}
		return unix.Kevent(kq, changes, events, timeout)
	}
	watcher, err := newDarwinV2SocketWatcher(dir, kevent)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	path := filepath.Join(dir, "new-tmux")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	select {
	case got := <-watcher.Events():
		if got != path {
			t.Fatalf("socket=%q want=%q", got, path)
		}
		if readCalls.Load() < 2 {
			t.Fatalf("kqueue read calls=%d want at least 2", readCalls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("interrupted kqueue wait permanently stopped socket discovery")
	}
}

func TestDarwinV2SocketWatcherRestartsAfterFatalKqueueError(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "v2-socket-watch-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var readCalls atomic.Int32
	kevent := func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		if len(changes) == 0 && readCalls.Add(1) == 1 {
			return 0, unix.EIO
		}
		return unix.Kevent(kq, changes, events, timeout)
	}
	watcher, err := newDarwinV2SocketWatcher(dir, kevent)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	path := filepath.Join(dir, "new-tmux")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	select {
	case got := <-watcher.Events():
		if got != path {
			t.Fatalf("socket=%q want=%q", got, path)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal kqueue error was not self-healed")
	}
}
