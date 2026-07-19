//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinProcessSnapshotsDoNotUseExecFallback(t *testing.T) {
	oldScan, oldTargeted := scanProcFallback, targetedFallback
	scanProcFallback = func() *procSnapshot {
		t.Fatal("global process scan invoked exec fallback")
		return nil
	}
	targetedFallback = func(int) (string, []int, *procSnapshot, bool) {
		t.Fatal("targeted process scan invoked exec fallback")
		return "", nil, nil, false
	}
	t.Cleanup(func() { scanProcFallback, targetedFallback = oldScan, oldTargeted })

	if snap := scanProcSnapshot(); snap == nil || !snap.valid || snap.command[os.Getpid()] == "" {
		t.Fatalf("native global process snapshot is incomplete: %#v", snap)
	}
	_, _, snap, _ := targetedPaneAgent(os.Getpid())
	if snap == nil || !snap.valid || snap.command[os.Getpid()] == "" {
		t.Fatalf("native targeted process snapshot is incomplete: %#v", snap)
	}
}

func TestTargetedPaneClassificationSurvivesExecRunnerFailure(t *testing.T) {
	oldList, oldCommand, oldFallback := darwinProcessList, darwinProcessCommandFor, targetedFallback
	darwinProcessList = func() ([]unix.KinfoProc, error) {
		root := unix.KinfoProc{}
		root.Proc.P_pid, root.Eproc.Ppid = 20, 1
		agent := unix.KinfoProc{}
		agent.Proc.P_pid, agent.Eproc.Ppid = 21, 20
		return []unix.KinfoProc{root, agent}, nil
	}
	darwinProcessCommandFor = func(pid int) (string, error) {
		if pid == 21 {
			return "claude --session-id fixture", nil
		}
		return "zsh", nil
	}
	targetedFallback = func(int) (string, []int, *procSnapshot, bool) {
		t.Fatal("exec runner was called")
		return "", nil, nil, false
	}
	t.Cleanup(func() {
		darwinProcessList, darwinProcessCommandFor, targetedFallback = oldList, oldCommand, oldFallback
	})

	kind, pids, snap, ok := targetedPaneAgent(20)
	if !ok || kind != "claude" || !reflect.DeepEqual(pids, []int{21}) || snap.command[21] == "" {
		t.Fatalf("kind=%q pids=%v snapshot=%#v ok=%v", kind, pids, snap, ok)
	}
}

func TestParseDarwinProcArgsStopsBeforeEnvironment(t *testing.T) {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, 3)
	raw = append(raw, []byte("/opt/homebrew/bin/claude\x00\x00claude\x00--session-id\x00fixture-id\x00PATH=/bin\x00")...)
	want := []string{"claude", "--session-id", "fixture-id"}
	if got := parseDarwinProcArgs(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q want=%q", got, want)
	}
}
