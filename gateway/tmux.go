package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const claudeProcessStartLayout = "Mon Jan _2 15:04:05 2006"

func validatedClaudeSessionRegistryHint(pid int, cwd string, started time.Time) string {
	if pid <= 0 || cwd == "" || started.IsZero() {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(homeDir(), ".claude", "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return ""
	}
	var entry struct {
		PID       int    `json:"pid"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		ProcStart string `json:"procStart"`
	}
	if json.Unmarshal(data, &entry) != nil || entry.PID != pid || !uuidPattern.MatchString(entry.SessionID) ||
		filepath.Clean(entry.Cwd) != filepath.Clean(cwd) {
		return ""
	}
	registeredStart, err := time.ParseInLocation(claudeProcessStartLayout, entry.ProcStart, time.UTC)
	if err != nil || registeredStart.Unix() != started.Unix() {
		return ""
	}
	return strings.ToLower(entry.SessionID)
}

type paneBinding struct {
	Socket      string
	TmuxID      string
	WindowName  string
	PanePID     int
	Kind        string
	ProcessPIDs []int
}

type procSnapshot struct {
	command  map[int]string
	children map[int][]int
	started  map[int]time.Time
	valid    bool
}

var (
	procMu                sync.Mutex
	procValue             *procSnapshot
	procAt                time.Time
	socketMu              sync.Mutex
	sockets               []string
	socketsAt             time.Time
	socketCandidates      []string
	liveMu                sync.Mutex
	liveValue             *liveInspection
	liveAt                time.Time
	tmuxListPanesMu       sync.Mutex
	tmuxListPanesTTL      = 3 * time.Second
	scanProcFallback      = scanProcSnapshotExec
	targetedFallback      = targetedPaneAgentExec
	discoveryListPanesRun = func(ctx context.Context, socket string) ([]byte, error) {
		return tmuxCommandContext(ctx, socket, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_pid}\t#{pane_current_command}\t#{window_name}").CombinedOutput()
	}
)

type liveInspection struct {
	panes      []paneBinding
	files      processFiles
	snap       *procSnapshot
	hints      map[string]bool
	observedAt time.Time
	valid      bool
}

func loadProcSnapshot() *procSnapshot {
	procMu.Lock()
	if procValue != nil && time.Since(procAt) < 2*time.Second {
		v := procValue
		procMu.Unlock()
		return v
	}
	procMu.Unlock()

	snap := scanProcSnapshot()
	procMu.Lock()
	procValue, procAt = snap, time.Now()
	procMu.Unlock()
	return snap
}

func scanProcSnapshot() *procSnapshot {
	if snap, err, supported := nativeProcSnapshotAll(); supported {
		if err != nil {
			log.Printf("process scan failed: sysctl: %v", err)
			return &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: false}
		}
		return snap
	}
	return scanProcFallback()
}

func scanProcSnapshotExec() *procSnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snapshotAt := time.Now().Truncate(time.Second)
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,lstart=,etime=,command=").Output()
	snap := &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: err == nil}
	if err != nil {
		log.Printf("process scan failed: %v", err)
		return snap
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		parts := strings.Fields(line)
		if len(parts) < 9 {
			continue
		}
		pid, err1 := strconv.Atoi(parts[0])
		ppid, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}
		command := strings.Join(parts[8:], " ")
		snap.command[pid] = strings.ToLower(command)
		snap.children[ppid] = append(snap.children[ppid], pid)
		if started := parseProcessStarted(parts, snapshotAt); !started.IsZero() {
			snap.started[pid] = started
		}
	}
	return snap
}

func parseProcessStarted(fields []string, snapshotAt time.Time) time.Time {
	if len(fields) < 8 {
		return time.Time{}
	}
	if started, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(fields[2:7], " "), time.Local); err == nil {
		return started
	}
	if elapsed, err := parseElapsed(fields[7]); err == nil {
		return snapshotAt.Add(-elapsed)
	}
	return time.Time{}
}

func parseElapsed(value string) (time.Duration, error) {
	days := 0
	if parts := strings.SplitN(value, "-", 2); len(parts) == 2 {
		var err error
		days, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		value = parts[1]
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid elapsed time %q", value)
	}
	values := make([]int, len(parts))
	for i := range parts {
		parsed, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, err
		}
		values[i] = parsed
	}
	hours, minutes, seconds := 0, values[0], values[1]
	if len(values) == 3 {
		hours, minutes, seconds = values[0], values[1], values[2]
	}
	return time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second, nil
}

func matchAgentKind(argv string) string {
	argv = strings.ToLower(argv)
	// Provider prompts can mention the other CLI. Only inspect the command prefix,
	// where the executable and wrapper command live.
	if len(argv) > 1024 {
		argv = argv[:1024]
	}
	claudeAt := firstIndex(argv, "@anthropic-ai", "/claude ", "claude ", "claude-code", "claude.exe")
	codexAt := firstIndex(argv, "@openai/codex", "/codex ", "codex ", "codex-code-mode")
	if claudeAt >= 0 && (codexAt < 0 || claudeAt < codexAt) {
		return "claude"
	}
	if codexAt >= 0 {
		return "codex"
	}
	if strings.HasSuffix(argv, "/claude") || argv == "claude" {
		return "claude"
	}
	if strings.HasSuffix(argv, "/codex") || argv == "codex" {
		return "codex"
	}
	return ""
}

func firstIndex(text string, needles ...string) int {
	result := -1
	for _, needle := range needles {
		if index := strings.Index(text, needle); index >= 0 && (result < 0 || index < result) {
			result = index
		}
	}
	return result
}

func isShellLauncherCommand(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return false
	}
	shell := strings.TrimPrefix(fields[0], "-")
	if index := strings.LastIndex(shell, "/"); index >= 0 {
		shell = shell[index+1:]
	}
	if shell != "sh" && shell != "bash" && shell != "zsh" {
		return false
	}
	return fields[1] == "-c" || fields[1] == "-lc"
}

func classifyPaneAgent(snap *procSnapshot, root int) (string, []int) {
	type processDepth struct {
		pid   int
		depth int
	}
	queue := []processDepth{{pid: root}}
	seen := map[int]bool{}
	var processes []processDepth
	minimumDepth := -1
	kinds := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current.pid] {
			continue
		}
		seen[current.pid] = true
		processes = append(processes, current)
		command := snap.command[current.pid]
		if kind := matchAgentKind(command); kind != "" && !isShellLauncherCommand(command) {
			if minimumDepth < 0 {
				minimumDepth = current.depth
			}
			if current.depth == minimumDepth {
				kinds[kind] = true
			}
		}
		for _, child := range snap.children[current.pid] {
			queue = append(queue, processDepth{pid: child, depth: current.depth + 1})
		}
	}
	if len(kinds) != 1 {
		return "", nil
	}
	kind := ""
	for value := range kinds {
		kind = value
	}
	var agentPIDs []int
	for _, process := range processes {
		command := snap.command[process.pid]
		if !isShellLauncherCommand(command) && matchAgentKind(command) == kind {
			agentPIDs = append(agentPIDs, process.pid)
		}
	}
	return kind, agentPIDs
}

func tmuxCommand(socket string, args ...string) *exec.Cmd {
	return tmuxCommandContext(context.Background(), socket, args...)
}

func tmuxCommandContext(ctx context.Context, socket string, args ...string) *exec.Cmd {
	if socket == "" {
		return exec.CommandContext(ctx, "tmux", args...)
	}
	return exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...)
}

func findTmuxSockets() []string {
	dir := fmt.Sprintf("/private/tmp/tmux-%d", os.Getuid())
	return findTmuxSocketsInDir(dir, false)
}

func findTmuxSocketsWithFreshCandidates() []string {
	dir := fmt.Sprintf("/private/tmp/tmux-%d", os.Getuid())
	return findTmuxSocketsInDir(dir, true)
}

func findTmuxSocketsInDir(dir string, refreshCandidates bool) []string {
	var candidates []string
	if refreshCandidates {
		var ok bool
		candidates, ok = listTmuxSocketCandidates(dir)
		if !ok {
			return nil
		}
	}
	socketMu.Lock()
	cacheFresh := sockets != nil && time.Since(socketsAt) < 8*time.Second
	if cacheFresh && (!refreshCandidates || equalStringSlices(candidates, socketCandidates)) {
		v := append([]string(nil), sockets...)
		socketMu.Unlock()
		return v
	}
	socketMu.Unlock()

	if !refreshCandidates {
		var ok bool
		candidates, ok = listTmuxSocketCandidates(dir)
		if !ok {
			return nil
		}
	}
	observedCandidates := append([]string(nil), candidates...)

	// macOS can retain hundreds of dead socket files. lsof identifies the few
	// sockets that still have an owning process without touching tmux state.
	lsofCtx, lsofCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer lsofCancel()
	if out, err := exec.CommandContext(lsofCtx, "lsof", "-U", "-Fn").Output(); err == nil {
		open := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "n"+dir+"/") {
				open[line[1:]] = true
			}
		}
		if len(open) > 0 {
			filtered := candidates[:0]
			for _, path := range candidates {
				if open[path] {
					filtered = append(filtered, path)
				}
			}
			candidates = filtered
		}
	}
	type result struct {
		path  string
		alive bool
	}
	results := make(chan result, len(candidates))
	sem := make(chan struct{}, 8)
	for _, path := range candidates {
		sem <- struct{}{}
		go func(path string) {
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "tmux", "-S", path, "list-panes", "-a", "-F", "#{pane_id}")
			results <- result{path: path, alive: cmd.Run() == nil}
		}(path)
	}
	var alive []string
	for range candidates {
		if r := <-results; r.alive {
			alive = append(alive, r.path)
		}
	}
	sort.Strings(alive)
	log.Printf("tmux discovery: %d candidates, %d alive", len(candidates), len(alive))
	socketMu.Lock()
	sockets, socketsAt = append([]string(nil), alive...), time.Now()
	socketCandidates = observedCandidates
	socketMu.Unlock()
	return alive
}

func listTmuxSocketCandidates(dir string) ([]string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var candidates []string
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && !info.IsDir() {
			candidates = append(candidates, dir+"/"+entry.Name())
		}
	}
	return candidates, true
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func listAgentPanes() ([]paneBinding, bool) {
	return listAgentPanesWithSocketCandidateRefresh(false)
}

func listAgentPanesWithSocketCandidateRefresh(refreshCandidates bool) ([]paneBinding, bool) {
	snap := loadProcSnapshot()
	if !snap.valid {
		return nil, false
	}
	socketPaths := paneDiscoverySocketPaths(refreshCandidates)
	var panes []paneBinding
	valid := true
	for _, socket := range socketPaths {
		out, err := discoveryListPanes(socket)
		if err != nil {
			log.Printf("v2 discovery: operation=list-panes socket=%q result=error error=%q output=%q", socket, err.Error(), string(out))
			valid = false
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.SplitN(line, "\t", 4)
			if len(fields) != 4 {
				continue
			}
			panePID, _ := strconv.Atoi(fields[1])
			kind, agentPIDs := classifyPaneAgent(snap, panePID)
			if kind != "" {
				panes = append(panes, paneBinding{Socket: socket, TmuxID: fields[0], WindowName: fields[3], PanePID: panePID, Kind: kind, ProcessPIDs: agentPIDs})
			}
		}
	}
	return panes, valid
}

func listRawPanesWithSocketCandidateRefresh(refreshCandidates bool) []paneBinding {
	socketPaths := paneDiscoverySocketPaths(refreshCandidates)
	var panes []paneBinding
	for _, socket := range socketPaths {
		out, err := discoveryListPanes(socket)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.SplitN(line, "\t", 4)
			if len(fields) != 4 {
				continue
			}
			panePID, _ := strconv.Atoi(fields[1])
			if panePID > 0 {
				panes = append(panes, paneBinding{Socket: socket, TmuxID: fields[0], WindowName: fields[3], PanePID: panePID, Kind: matchAgentKind(fields[2])})
			}
		}
	}
	return panes
}

func listRawPanesForSocketTargeted(socket string) []paneBinding {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := discoveryListPanesRun(ctx, socket)
	if err != nil {
		log.Printf("v2 discovery: operation=list-panes-targeted socket=%q result=error error=%q output=%q", socket, err.Error(), string(out))
		return nil
	}
	var panes []paneBinding
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) != 4 {
			continue
		}
		panePID, _ := strconv.Atoi(fields[1])
		if panePID > 0 {
			panes = append(panes, paneBinding{Socket: socket, TmuxID: fields[0], WindowName: fields[3], PanePID: panePID, Kind: matchAgentKind(fields[2])})
		}
	}
	return panes
}

func paneDiscoverySocketPaths(refreshCandidates bool) []string {
	if !refreshCandidates {
		return findTmuxSockets()
	}
	dir := fmt.Sprintf("/private/tmp/tmux-%d", os.Getuid())
	candidates, ok := listTmuxSocketCandidates(dir)
	if !ok {
		return findTmuxSockets()
	}
	candidates = liveUnixSocketCandidates(candidates)
	socketMu.Lock()
	known := append([]string(nil), sockets...)
	previous := make(map[string]bool, len(socketCandidates))
	for _, path := range socketCandidates {
		previous[path] = true
	}
	sockets = append([]string(nil), candidates...)
	socketsAt = time.Now()
	socketCandidates = append([]string(nil), candidates...)
	socketMu.Unlock()
	seen := map[string]bool{}
	paths := make([]string, 0, len(candidates)+len(known))
	// A newly-created socket is the most likely birth source and must not wait
	// behind every existing tmux server under host load.
	for _, path := range candidates {
		if !previous[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	for _, path := range known {
		if !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	return paths
}

func liveUnixSocketCandidates(candidates []string) []string {
	type result struct {
		path string
		live bool
	}
	results := make(chan result, len(candidates))
	sem := make(chan struct{}, 64)
	for _, path := range candidates {
		go func(path string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			connection, err := net.DialTimeout("unix", path, 25*time.Millisecond)
			if err == nil {
				_ = connection.Close()
			}
			results <- result{path: path, live: err == nil}
		}(path)
	}
	live := make([]string, 0, len(candidates))
	for range candidates {
		if result := <-results; result.live {
			live = append(live, result.path)
		}
	}
	sort.Strings(live)
	return live
}

func discoveryListPanes(socket string) ([]byte, error) {
	tmuxListPanesMu.Lock()
	defer tmuxListPanesMu.Unlock()
	timeout := tmuxListPanesTTL
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	started := time.Now()
	out, err := discoveryListPanesRun(ctx, socket)
	cancel()
	elapsed := time.Since(started)
	if err == nil {
		adapted := 2*elapsed + time.Second
		if adapted < 3*time.Second {
			adapted = 3 * time.Second
		}
		if adapted > 10*time.Second {
			adapted = 10 * time.Second
		}
		tmuxListPanesTTL = adapted
	} else if timeout < 10*time.Second {
		tmuxListPanesTTL = 2 * timeout
		if tmuxListPanesTTL > 10*time.Second {
			tmuxListPanesTTL = 10 * time.Second
		}
	}
	return out, err
}

type targetedPaneAgentFunc func(int) (string, []int, *procSnapshot, bool)
type inspectProcessFilesFunc func([]int) processFiles

func augmentV2InspectionWithNewPanes(inspection liveInspection, raw []paneBinding, classify targetedPaneAgentFunc, inspectFiles inspectProcessFilesFunc) liveInspection {
	inspection.panes = append([]paneBinding(nil), inspection.panes...)
	inspection.snap = cloneProcSnapshot(inspection.snap)
	inspection.files = cloneProcessFiles(inspection.files)
	inspection.hints = cloneBoolMap(inspection.hints)
	known := make(map[v2PaneIdentityKey]bool, len(inspection.panes))
	for _, pane := range inspection.panes {
		known[v2PaneIdentityKey{socket: pane.Socket, paneID: pane.TmuxID, panePID: pane.PanePID}] = true
	}
	if inspection.snap == nil {
		inspection.snap = &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true}
	}
	if inspection.files.cwd == nil {
		inspection.files = processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true}
	}
	if inspection.hints == nil {
		inspection.hints = map[string]bool{}
	}
	var added []paneBinding
	for _, pane := range raw {
		key := v2PaneIdentityKey{socket: pane.Socket, paneID: pane.TmuxID, panePID: pane.PanePID}
		if known[key] {
			continue
		}
		if pane.Kind == "" {
			continue
		}
		started := time.Now()
		kind, pids, partial, ok := classify(pane.PanePID)
		if !ok || kind == "" || len(pids) == 0 {
			log.Printf("v2 discovery: operation=targeted-classify socket=%q pane=%s pane_pid=%d result=unidentified duration_ms=%.3f", pane.Socket, pane.TmuxID, pane.PanePID, float64(time.Since(started))/float64(time.Millisecond))
			continue
		}
		log.Printf("v2 discovery: operation=targeted-classify socket=%q pane=%s pane_pid=%d agent_pid=%d kind=%s result=ok duration_ms=%.3f", pane.Socket, pane.TmuxID, pane.PanePID, pids[0], kind, float64(time.Since(started))/float64(time.Millisecond))
		pane.Kind, pane.ProcessPIDs = kind, pids
		inspection.panes = append(inspection.panes, pane)
		added = append(added, pane)
		mergeProcSnapshot(inspection.snap, partial)
	}
	var addedPIDs []int
	for _, pane := range added {
		addedPIDs = append(addedPIDs, pane.ProcessPIDs...)
	}
	files := inspectFiles(addedPIDs)
	if files.valid {
		for pid, cwd := range files.cwd {
			inspection.files.cwd[pid] = cwd
		}
		for pid, paths := range files.open {
			inspection.files.open[pid] = append([]string(nil), paths...)
		}
	}
	for _, pane := range added {
		for _, pid := range pane.ProcessPIDs {
			for _, path := range inspection.files.open[pid] {
				if pane.Kind == "codex" && strings.Contains(path, "/.codex/sessions/") {
					if id := sessionIDFromPath(path); id != "" {
						inspection.hints["codex\x00"+id] = true
					}
				}
			}
			if pane.Kind == "claude" {
				if id := claudeSessionHint(pid, inspection.snap.command[pid]); id != "" {
					inspection.hints["claude\x00"+id] = true
				}
			}
		}
	}
	if len(added) > 0 {
		inspection.observedAt = time.Now()
	}
	return inspection
}

func cloneProcSnapshot(source *procSnapshot) *procSnapshot {
	if source == nil {
		return nil
	}
	target := &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: source.valid}
	for pid, command := range source.command {
		target.command[pid] = command
	}
	for parent, children := range source.children {
		target.children[parent] = append([]int(nil), children...)
	}
	for pid, started := range source.started {
		target.started[pid] = started
	}
	return target
}

func cloneProcessFiles(source processFiles) processFiles {
	target := processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: source.valid}
	for pid, cwd := range source.cwd {
		target.cwd[pid] = cwd
	}
	for pid, paths := range source.open {
		target.open[pid] = append([]string(nil), paths...)
	}
	return target
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	target := make(map[string]bool, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}

func targetedPaneAgent(root int) (string, []int, *procSnapshot, bool) {
	if snap, err, supported := nativeProcSnapshotTree(root); supported {
		if err != nil {
			log.Printf("v2 discovery: operation=sysctl-target root_pid=%d result=error error=%q", root, err.Error())
			return "", nil, nil, false
		}
		kind, agents := classifyPaneAgent(snap, root)
		return kind, agents, snap, kind != "" && len(agents) > 0
	}
	return targetedFallback(root)
}

func targetedPaneAgentExec(root int) (string, []int, *procSnapshot, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	pids := []int{root}
	seen := map[int]bool{root: true}
	for index := 0; index < len(pids) && len(pids) < 64; index++ {
		out, err := exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(pids[index])).Output()
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				log.Printf("v2 discovery: operation=pgrep root_pid=%d parent_pid=%d result=error error=%q", root, pids[index], err.Error())
				return "", nil, nil, false
			}
		}
		for _, field := range strings.Fields(string(out)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr == nil && pid > 1 && !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
	}
	values := make([]string, len(pids))
	for index, pid := range pids {
		values[index] = strconv.Itoa(pid)
	}
	out, err := exec.CommandContext(ctx, "ps", "-p", strings.Join(values, ","), "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		log.Printf("v2 discovery: operation=ps-target root_pid=%d pids=%q result=error error=%q", root, strings.Join(values, ","), err.Error())
		return "", nil, nil, false
	}
	snap := &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil {
			continue
		}
		snap.command[pid] = strings.ToLower(strings.Join(fields[2:], " "))
		snap.children[parent] = append(snap.children[parent], pid)
	}
	kind, agents := classifyPaneAgent(snap, root)
	return kind, agents, snap, kind != "" && len(agents) > 0
}

func mergeProcSnapshot(target, source *procSnapshot) {
	if target == nil || source == nil {
		return
	}
	for pid, command := range source.command {
		target.command[pid] = command
	}
	for parent, children := range source.children {
		target.children[parent] = append([]int(nil), children...)
	}
}

type processFiles struct {
	cwd   map[int]string
	open  map[int][]string
	valid bool
}

func inspectProcessFiles(pids []int) processFiles {
	result := processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true}
	if len(pids) == 0 {
		return result
	}
	seen := map[int]bool{}
	var values []string
	for _, pid := range pids {
		if pid > 0 && !seen[pid] {
			seen[pid] = true
			values = append(values, strconv.Itoa(pid))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-F", "pfn", "-p", strings.Join(values, ",")).Output()
	if err != nil && len(out) == 0 {
		result.valid = false
		log.Printf("process file scan failed: pids=%d error=%v", len(values), err)
		return result
	}
	pid, fd := 0, ""
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'f':
			fd = line[1:]
		case 'n':
			path := line[1:]
			if fd == "cwd" {
				result.cwd[pid] = path
			}
			if strings.HasSuffix(path, ".jsonl") &&
				(strings.Contains(path, "/.claude/projects/") || strings.Contains(path, "/.codex/sessions/")) {
				result.open[pid] = append(result.open[pid], path)
			}
		}
	}
	return result
}

func liveSessionHintKeys() (map[string]bool, bool) {
	inspection := inspectLiveState()
	hints := make(map[string]bool, len(inspection.hints))
	for key := range inspection.hints {
		hints[key] = true
	}
	return hints, inspection.valid
}

func inspectLiveState() liveInspection {
	return inspectLiveStateMaxAge(10 * time.Second)
}

func inspectLiveStateMaxAge(maxAge time.Duration) liveInspection {
	return inspectLiveStateMaxAgeWithSocketCandidateRefresh(maxAge, false)
}

func inspectLiveStateMaxAgeWithSocketCandidateRefresh(maxAge time.Duration, refreshCandidates bool) liveInspection {
	liveMu.Lock()
	defer liveMu.Unlock()
	if liveValue != nil && time.Since(liveAt) < maxAge {
		return *liveValue
	}
	hints := map[string]bool{}
	panes, panesValid := listAgentPanesWithSocketCandidateRefresh(refreshCandidates)
	if livePaneScanIncomplete(panes, panesValid) {
		log.Printf("live scan incomplete: panes=%d valid=%v; preserving last successful result", len(panes), panesValid)
		if liveValue != nil {
			return *liveValue
		}
		return liveInspection{hints: hints}
	}
	var allPIDs []int
	for _, pane := range panes {
		allPIDs = append(allPIDs, pane.ProcessPIDs...)
	}
	files := inspectProcessFiles(allPIDs)
	snap := loadProcSnapshot()
	if !files.valid || !snap.valid {
		log.Printf("live scan incomplete: process-files=%v process-tree=%v; preserving last successful result", files.valid, snap.valid)
		if liveValue != nil {
			return *liveValue
		}
		return liveInspection{hints: hints}
	}
	for _, pane := range panes {
		for _, pid := range pane.ProcessPIDs {
			for _, path := range files.open[pid] {
				if pane.Kind == "codex" && strings.Contains(path, "/.codex/sessions/") {
					if id := sessionIDFromPath(path); id != "" {
						hints[pane.Kind+"\x00"+id] = true
					}
				}
			}
			if pane.Kind == "claude" {
				if id := claudeSessionHint(pid, snap.command[pid]); id != "" {
					hints["claude\x00"+id] = true
				}
			}
		}
	}
	value := liveInspection{panes: panes, files: files, snap: snap, hints: hints, observedAt: time.Now(), valid: true}
	liveValue = &value
	liveAt = time.Now()
	return value
}

func livePaneScanIncomplete(_ []paneBinding, panesValid bool) bool {
	return !panesValid
}

func claudeSessionHint(pid int, command string) string {
	id, _ := claudeSessionHintEvidence(pid, command)
	return id
}

func claudeSessionHintEvidence(pid int, command string) (string, string) {
	fields := strings.Fields(command)
	for i, field := range fields {
		if (field == "--resume" || field == "--session-id") && i+1 < len(fields) && uuidPattern.MatchString(fields[i+1]) {
			return strings.ToLower(fields[i+1]), "argv"
		}
		for _, prefix := range []string{"--resume=", "--session-id="} {
			value := strings.TrimPrefix(field, prefix)
			if value != field && uuidPattern.MatchString(value) {
				return strings.ToLower(value), "argv"
			}
		}
	}
	if value, supported := nativeProcessEnvironmentValue(pid, "CLAUDE_CODE_SESSION_ID"); supported {
		if uuidPattern.MatchString(value) {
			return strings.ToLower(value), "env"
		}
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "eww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", ""
	}
	const key = "CLAUDE_CODE_SESSION_ID="
	text := string(out)
	for start := 0; ; {
		i := strings.Index(text[start:], key)
		if i < 0 {
			return "", ""
		}
		i += start + len(key)
		end := i
		for end < len(text) && text[end] != ' ' && text[end] != '\n' && text[end] != '\t' {
			end++
		}
		value := strings.Trim(text[i:end], "'\"")
		if uuidPattern.MatchString(value) {
			return strings.ToLower(value), "env"
		}
		start = end
	}
}

func sendToPane(binding *paneBinding, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	log.Printf("send: socket=%q pane=%s bytes=%d", binding.Socket, binding.TmuxID, len(text))
	if err := tmuxCommandContext(ctx, binding.Socket, "send-keys", "-t", binding.TmuxID, "-l", "--", text).Run(); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond)
	enterCtx, enterCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer enterCancel()
	return tmuxCommandContext(enterCtx, binding.Socket, "send-keys", "-t", binding.TmuxID, "Enter").Run()
}

func processTreePIDs(snap *procSnapshot, roots []int) []int {
	seen := map[int]bool{}
	var pids []int
	var visit func(int)
	visit = func(pid int) {
		if pid <= 1 || seen[pid] {
			return
		}
		seen[pid] = true
		for _, child := range snap.children[pid] {
			visit(child)
		}
		pids = append(pids, pid)
	}
	for _, root := range roots {
		visit(root)
	}
	return pids
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalProcesses(pids []int, signal syscall.Signal) error {
	for _, pid := range pids {
		if pid == os.Getpid() {
			return fmt.Errorf("refusing to signal gateway pid %d", pid)
		}
		if err := syscall.Kill(pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal %d pid %d: %w", signal, pid, err)
		}
	}
	return nil
}

func waitProcessesGone(pids []int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			if processAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || !time.Now().Before(deadline) {
			return alive
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func terminatePane(binding *paneBinding) ([]int, error) {
	procMu.Lock()
	procValue, procAt = nil, time.Time{}
	procMu.Unlock()
	snap := loadProcSnapshot()
	if !snap.valid {
		return nil, fmt.Errorf("process scan unavailable")
	}
	pids := processTreePIDs(snap, binding.ProcessPIDs)
	var alive []int
	for _, pid := range pids {
		if processAlive(pid) {
			alive = append(alive, pid)
		}
	}
	if len(alive) == 0 {
		return nil, fmt.Errorf("bound CLI process is no longer alive")
	}
	log.Printf("kill: socket=%q pane=%s signal=TERM pids=%v", binding.Socket, binding.TmuxID, alive)
	if err := signalProcesses(alive, syscall.SIGTERM); err != nil {
		return nil, err
	}
	remaining := waitProcessesGone(alive, 3*time.Second)
	if len(remaining) > 0 {
		log.Printf("kill: socket=%q pane=%s signal=KILL pids=%v", binding.Socket, binding.TmuxID, remaining)
		if err := signalProcesses(remaining, syscall.SIGKILL); err != nil {
			return nil, err
		}
		if remaining = waitProcessesGone(remaining, 3*time.Second); len(remaining) > 0 {
			return nil, fmt.Errorf("processes still alive after SIGKILL: %v", remaining)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	log.Printf("kill: socket=%q pane=%s tmux=kill-pane", binding.Socket, binding.TmuxID)
	if output, err := tmuxCommandContext(ctx, binding.Socket, "kill-pane", "-t", binding.TmuxID).CombinedOutput(); err != nil {
		exists, confirmed := paneExists(binding)
		if !confirmed || exists {
			return nil, fmt.Errorf("tmux kill-pane: %w: %s", err, strings.TrimSpace(string(output)))
		}
		log.Printf("kill: socket=%q pane=%s already gone after CLI exit", binding.Socket, binding.TmuxID)
	}
	sort.Ints(alive)
	return alive, nil
}

func paneExists(binding *paneBinding) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := tmuxCommandContext(ctx, binding.Socket, "list-panes", "-a", "-F", "#{pane_id}").CombinedOutput()
	if err == nil {
		for _, paneID := range strings.Fields(string(output)) {
			if paneID == binding.TmuxID {
				return true, true
			}
		}
		return false, true
	}
	if binding.Socket != "" {
		if _, statErr := os.Stat(binding.Socket); errors.Is(statErr, os.ErrNotExist) {
			return false, true
		}
	}
	message := strings.ToLower(string(output))
	if strings.Contains(message, "no server running") || strings.Contains(message, "no sessions") ||
		strings.Contains(message, "server exited") || strings.Contains(message, "error connecting") {
		return false, true
	}
	return false, false
}
