//go:build !darwin

package main

type noopV2ProcessWatcher struct{ events chan int }

func newV2ProcessWatcher() (v2ProcessWatcher, error) {
	return &noopV2ProcessWatcher{events: make(chan int)}, nil
}

func (watcher *noopV2ProcessWatcher) Events() <-chan int { return watcher.events }
func (watcher *noopV2ProcessWatcher) Set([]int) error    { return nil }
func (watcher *noopV2ProcessWatcher) Close() error       { return nil }
