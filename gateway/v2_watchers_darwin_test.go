//go:build darwin && cgo

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinV2ProcessWatcherReportsExit(t *testing.T) {
	watcher, err := newV2ProcessWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	process := exec.Command("sleep", "0.05")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	pid := process.Process.Pid
	if err := watcher.Set([]int{pid}); err != nil {
		_ = process.Process.Kill()
		t.Fatal(err)
	}
	_ = process.Wait()
	select {
	case exited := <-watcher.Events():
		if exited != pid {
			t.Fatalf("exit pid=%d want=%d", exited, pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("kqueue NOTE_EXIT did not report pid %d", pid)
	}
}

func TestDarwinV2FSEventsWatcherReportsCreateAndAppend(t *testing.T) {
	parent := t.TempDir()
	roots := []string{filepath.Join(parent, "claude"), filepath.Join(parent, "codex")}
	for _, root := range roots {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	watcher, err := newV2FSEventsWatcher(roots)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	claudePath := filepath.Join(roots[0], "session.jsonl")
	if err := os.WriteFile(claudePath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForV2PathEvent(t, watcher.Events(), claudePath)

	codexPath := filepath.Join(roots[1], "rollout.jsonl")
	if err := os.WriteFile(codexPath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForV2PathEvent(t, watcher.Events(), codexPath)
	file, err := os.OpenFile(codexPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("second\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append=%v close=%v", writeErr, closeErr)
	}
	waitForV2PathEvent(t, watcher.Events(), codexPath)
}

func waitForV2PathEvent(t *testing.T, events <-chan v2PathInvalidation, path string) {
	t.Helper()
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			for _, changed := range event.Paths {
				got, _ := filepath.EvalSymlinks(changed)
				if got == want {
					return
				}
			}
		case <-timer.C:
			t.Fatalf("FSEvents did not report %s", path)
		}
	}
}
