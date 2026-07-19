//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework CoreServices -framework CoreFoundation
#include <stdlib.h>
#include "v2_fsevents_darwin.h"
*/
import "C"

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"
)

type darwinV2FSEventsWatcher struct {
	stream    *C.RCV2FSEvents
	token     uintptr
	events    chan v2PathInvalidation
	overflow  chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var (
	v2FSEventsToken atomic.Uint64
	v2FSEventsMu    sync.RWMutex
	v2FSEventsByID  = map[uintptr]*darwinV2FSEventsWatcher{}
)

func newV2FSEventsWatcher(roots []string) (v2PathWatcher, error) {
	existing := make([]string, 0, 2)
	for _, root := range roots {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			existing = append(existing, root)
		}
		if len(existing) == 2 {
			break
		}
	}
	if len(existing) == 0 {
		return &noopV2PathWatcher{events: make(chan v2PathInvalidation)}, nil
	}
	token := uintptr(v2FSEventsToken.Add(1))
	watcher := &darwinV2FSEventsWatcher{
		token: token, events: make(chan v2PathInvalidation, 512),
		overflow: make(chan struct{}, 1), done: make(chan struct{}),
	}
	v2FSEventsMu.Lock()
	v2FSEventsByID[token] = watcher
	v2FSEventsMu.Unlock()
	path1 := C.CString(existing[0])
	defer C.free(unsafe.Pointer(path1))
	var path2 *C.char
	if len(existing) > 1 {
		path2 = C.CString(existing[1])
		defer C.free(unsafe.Pointer(path2))
	}
	watcher.stream = C.rc_v2_fsevents_start(C.uintptr_t(token), path1, path2)
	if watcher.stream == nil {
		v2FSEventsMu.Lock()
		delete(v2FSEventsByID, token)
		v2FSEventsMu.Unlock()
		return nil, errors.New("FSEventStreamStart failed")
	}
	go watcher.forwardOverflow()
	return watcher, nil
}

//export goV2FSEventsEmit
func goV2FSEventsEmit(token C.uintptr_t, path *C.char, flags C.uint32_t) {
	v2FSEventsMu.RLock()
	watcher := v2FSEventsByID[uintptr(token)]
	v2FSEventsMu.RUnlock()
	if watcher == nil {
		return
	}
	mask := uint32(C.rc_v2_fsevents_full_scan_flags())
	event := v2PathInvalidation{
		Paths: []string{C.GoString(path)}, FullScan: uint32(flags)&mask != 0,
	}
	select {
	case watcher.events <- event:
	default:
		select {
		case watcher.overflow <- struct{}{}:
		default:
		}
	}
}

func (watcher *darwinV2FSEventsWatcher) Events() <-chan v2PathInvalidation { return watcher.events }

func (watcher *darwinV2FSEventsWatcher) forwardOverflow() {
	for {
		select {
		case <-watcher.done:
			return
		case <-watcher.overflow:
			select {
			case watcher.events <- v2PathInvalidation{FullScan: true}:
			case <-watcher.done:
				return
			}
		}
	}
}

func (watcher *darwinV2FSEventsWatcher) Close() error {
	watcher.closeOnce.Do(func() {
		v2FSEventsMu.Lock()
		delete(v2FSEventsByID, watcher.token)
		v2FSEventsMu.Unlock()
		close(watcher.done)
		C.rc_v2_fsevents_stop(watcher.stream)
	})
	return nil
}
