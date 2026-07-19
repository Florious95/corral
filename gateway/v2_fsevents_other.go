//go:build !darwin || !cgo

package main

func newV2FSEventsWatcher([]string) (v2PathWatcher, error) {
	return &noopV2PathWatcher{events: make(chan v2PathInvalidation)}, nil
}
