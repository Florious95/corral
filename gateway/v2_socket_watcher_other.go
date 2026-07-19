//go:build !darwin

package main

func newV2SocketWatcher(string) (v2SocketWatcher, error) {
	return &noopV2SocketWatcher{events: make(chan string)}, nil
}
