package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveUnixSocketCandidatesRejectsStaleFiles(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "corral-sockets-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	livePath, stalePath := filepath.Join(dir, "live"), filepath.Join(dir, "stale")
	listener, err := net.Listen("unix", livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(stalePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := liveUnixSocketCandidates([]string{stalePath, livePath})
	if len(got) != 1 || got[0] != livePath {
		t.Fatalf("live sockets=%v want=[%s]", got, livePath)
	}
}

func TestDiscoveryListPanesSerializesTmuxExec(t *testing.T) {
	oldRun, oldTTL := discoveryListPanesRun, tmuxListPanesTTL
	var active, maximum atomic.Int32
	discoveryListPanesRun = func(context.Context, string) ([]byte, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return []byte("%0\t20\tclaude\tfixture\n"), nil
	}
	t.Cleanup(func() { discoveryListPanesRun, tmuxListPanesTTL = oldRun, oldTTL })

	var wait sync.WaitGroup
	for _, socket := range []string{"one", "two", "three"} {
		wait.Add(1)
		go func(socket string) {
			defer wait.Done()
			if _, err := discoveryListPanes(socket); err != nil {
				t.Errorf("list panes %s: %v", socket, err)
			}
		}(socket)
	}
	wait.Wait()
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent tmux exec=%d want=1", got)
	}
}

func TestFreshTmuxSocketCandidateBypassesAliveCache(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old")
	newPath := filepath.Join(dir, "new")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	socketMu.Lock()
	oldSockets, oldSocketsAt := sockets, socketsAt
	oldCandidates := socketCandidates
	sockets = []string{oldPath}
	socketsAt = time.Now()
	socketCandidates = []string{oldPath}
	socketMu.Unlock()
	t.Cleanup(func() {
		socketMu.Lock()
		sockets, socketsAt = oldSockets, oldSocketsAt
		socketCandidates = oldCandidates
		socketMu.Unlock()
	})

	got := findTmuxSocketsInDir(dir, true)
	for _, path := range got {
		if path == oldPath {
			t.Fatalf("fresh candidate scan reused stale alive cache: %v", got)
		}
	}
}

func TestV2PaneBirthAugmentsStaleGlobalSnapshotWithTargetedProcessTree(t *testing.T) {
	base := liveInspection{
		panes: []paneBinding{{Socket: "/tmp/existing", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}},
		files: processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true},
		snap:  &procSnapshot{command: map[int]string{11: "claude"}, children: map[int][]int{10: {11}}, started: map[int]time.Time{}, valid: true},
		hints: map[string]bool{}, observedAt: time.Now(), valid: true,
	}
	raw := []paneBinding{
		{Socket: "/tmp/existing", TmuxID: "%0", PanePID: 10},
		{Socket: "/tmp/new", TmuxID: "%1", PanePID: 20, WindowName: "DO_NOT_CLOSE", Kind: "claude"},
	}
	classifications := 0
	got := augmentV2InspectionWithNewPanes(base, raw, func(root int) (string, []int, *procSnapshot, bool) {
		classifications++
		if root != 20 {
			t.Fatalf("targeted classifier root=%d, want only new pane root 20", root)
		}
		return "claude", []int{21}, &procSnapshot{
			command:  map[int]string{21: "claude --session-id 11111111-1111-1111-1111-111111111111"},
			children: map[int][]int{20: {21}}, started: map[int]time.Time{}, valid: true,
		}, true
	}, func(pids []int) processFiles {
		if len(pids) != 1 || pids[0] != 21 {
			t.Fatalf("file inspection pids=%v", pids)
		}
		return processFiles{cwd: map[int]string{21: "/fixture"}, open: map[int][]string{}, valid: true}
	})
	if classifications != 1 || len(got.panes) != 2 || got.panes[1].Socket != "/tmp/new" || got.panes[1].Kind != "claude" || got.panes[1].ProcessPIDs[0] != 21 || got.files.cwd[21] != "/fixture" {
		t.Fatalf("classifications=%d inspection=%#v", classifications, got)
	}
	if len(base.panes) != 1 || base.files.cwd[21] != "" || base.snap.command[21] != "" {
		t.Fatal("augmentation mutated the shared cached inspection")
	}
}
