//go:build darwin

package main

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	darwinProcessList       = func() ([]unix.KinfoProc, error) { return unix.SysctlKinfoProcSlice("kern.proc.all") }
	darwinProcessCommandFor = darwinProcessCommand
)

func nativeProcSnapshotAll() (*procSnapshot, error, bool) {
	processes, err := darwinProcessList()
	if err != nil {
		return nil, err, true
	}
	return nativeProcSnapshot(processes, nil), nil, true
}

func nativeProcSnapshotTree(root int) (*procSnapshot, error, bool) {
	processes, err := darwinProcessList()
	if err != nil {
		return nil, err, true
	}
	children := make(map[int][]int, len(processes))
	byPID := make(map[int]unix.KinfoProc, len(processes))
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		byPID[pid] = process
		children[int(process.Eproc.Ppid)] = append(children[int(process.Eproc.Ppid)], pid)
	}
	wanted := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 && len(wanted) < 64 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if !wanted[child] {
				wanted[child] = true
				queue = append(queue, child)
			}
		}
	}
	selected := make([]unix.KinfoProc, 0, len(wanted))
	for pid := range wanted {
		if process, ok := byPID[pid]; ok {
			selected = append(selected, process)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("process %d is unavailable", root), true
	}
	return nativeProcSnapshot(selected, wanted), nil, true
}

func nativeProcSnapshot(processes []unix.KinfoProc, wanted map[int]bool) *procSnapshot {
	snap := &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true}
	for _, process := range processes {
		pid, parent := int(process.Proc.P_pid), int(process.Eproc.Ppid)
		if wanted != nil && !wanted[pid] {
			continue
		}
		command, err := darwinProcessCommandFor(pid)
		if err != nil {
			command = darwinProcName(process.Proc.P_comm[:])
		}
		snap.command[pid] = strings.ToLower(command)
		snap.children[parent] = append(snap.children[parent], pid)
		snap.started[pid] = time.Unix(process.Proc.P_starttime.Sec, int64(process.Proc.P_starttime.Usec)*1000)
	}
	return snap
}

func darwinProcessCommand(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	args, _ := parseDarwinProcArgsAndEnv(raw)
	if len(args) == 0 {
		return "", fmt.Errorf("process %d has no argv", pid)
	}
	return strings.Join(args, " "), nil
}

func parseDarwinProcArgs(raw []byte) []string {
	args, _ := parseDarwinProcArgsAndEnv(raw)
	return args
}

func parseDarwinProcArgsAndEnv(raw []byte) ([]string, []string) {
	if len(raw) < 4 {
		return nil, nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	if argc <= 0 {
		return nil, nil
	}
	data := raw[4:]
	if end := strings.IndexByte(string(data), 0); end >= 0 {
		data = data[end+1:]
	} else {
		return nil, nil
	}
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}
	args := make([]string, 0, argc)
	for len(data) > 0 && len(args) < argc {
		end := strings.IndexByte(string(data), 0)
		if end < 0 {
			end = len(data)
		}
		if end > 0 {
			args = append(args, string(data[:end]))
		}
		if end == len(data) {
			break
		}
		data = data[end+1:]
	}
	var environment []string
	for len(data) > 0 {
		for len(data) > 0 && data[0] == 0 {
			data = data[1:]
		}
		if len(data) == 0 {
			break
		}
		end := strings.IndexByte(string(data), 0)
		if end < 0 {
			end = len(data)
		}
		environment = append(environment, string(data[:end]))
		if end == len(data) {
			break
		}
		data = data[end+1:]
	}
	return args, environment
}

func nativeProcessEnvironmentValue(pid int, key string) (string, bool) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", true
	}
	_, environment := parseDarwinProcArgsAndEnv(raw)
	prefix := key + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix), true
		}
	}
	return "", true
}

func darwinProcName(raw []byte) string {
	if end := strings.IndexByte(string(raw), 0); end >= 0 {
		raw = raw[:end]
	}
	return string(raw)
}
