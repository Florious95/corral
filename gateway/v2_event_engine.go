package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const v2PaneDiscoveryInterval = 2 * time.Second
const v2PathInvalidationDebounce = 50 * time.Millisecond

var (
	v2FastPhysicalFilesForPaths = v2PhysicalFilesForPaths
	v2FastCachedMetadata        = cachedMetadata
)

type v2PathInvalidation struct {
	Paths    []string
	FullScan bool
}

type v2PathWatcher interface {
	Events() <-chan v2PathInvalidation
	Close() error
}

type noopV2PathWatcher struct{ events chan v2PathInvalidation }

func (watcher *noopV2PathWatcher) Events() <-chan v2PathInvalidation { return watcher.events }
func (watcher *noopV2PathWatcher) Close() error                      { return nil }

type v2ProcessWatcher interface {
	Events() <-chan int
	Set([]int) error
	Close() error
}

type v2SocketWatcher interface {
	Events() <-chan string
	Close() error
}

type noopV2SocketWatcher struct{ events chan string }

func (watcher *noopV2SocketWatcher) Events() <-chan string { return watcher.events }
func (watcher *noopV2SocketWatcher) Close() error          { return nil }

type v2EventEngineConfig struct {
	Store          *terminalEntryStore
	PathWatcher    v2PathWatcher
	ProcessWatcher v2ProcessWatcher
	SocketWatcher  v2SocketWatcher
	PaneInterval   time.Duration
	Inspect        func() liveInspection
	InspectSocket  func(string) []paneBinding
	ClassifyPane   targetedPaneAgentFunc
	Build          func(liveInspection) (v2SnapshotInput, error)
	FastBuild      func(liveInspection, []string) (v2SnapshotInput, bool, error)
	Invalidate     func([]string, bool)
}

type v2EventEngine struct {
	config          v2EventEngineConfig
	startOnce       sync.Once
	inspection      liveInspection
	paneGeneration  uint64
	snapshotMu      sync.Mutex
	snapshotRunning bool
	snapshotVersion uint64
	snapshotPending *v2SnapshotBuild
}

type v2SnapshotBuild struct {
	inspection    liveInspection
	removalReason string
	version       uint64
}

type v2InspectionResult struct {
	inspection liveInspection
	generation uint64
}

func newV2EventEngine(config v2EventEngineConfig) *v2EventEngine {
	if config.PaneInterval <= 0 {
		config.PaneInterval = v2PaneDiscoveryInterval
	}
	if config.InspectSocket == nil {
		config.InspectSocket = listRawPanesForSocketTargeted
	}
	if config.ClassifyPane == nil {
		config.ClassifyPane = targetedPaneAgent
	}
	return &v2EventEngine{config: config}
}

func (engine *v2EventEngine) Start(ctx context.Context) {
	engine.startOnce.Do(func() { go engine.run(ctx) })
}

func (engine *v2EventEngine) run(ctx context.Context) {
	defer engine.config.PathWatcher.Close()
	defer engine.config.ProcessWatcher.Close()
	if engine.config.SocketWatcher != nil {
		defer engine.config.SocketWatcher.Close()
	}
	engine.refreshPanes("pane_gone")
	if !v2EventEngineBackgroundEnabled() {
		log.Printf("v2 event engine: background disabled by V2_EVENT_ENGINE_DISABLE=1 after initial snapshot")
		return
	}
	ticker := time.NewTicker(engine.config.PaneInterval)
	defer ticker.Stop()
	pathEvents := engine.config.PathWatcher.Events()
	processEvents := engine.config.ProcessWatcher.Events()
	var socketEvents <-chan string
	if engine.config.SocketWatcher != nil {
		socketEvents = engine.config.SocketWatcher.Events()
	}
	inspectionResults := make(chan v2InspectionResult, 1)
	inspectionRunning := false
	var pathTimer *time.Timer
	var pathTimerC <-chan time.Time
	var pendingPaths []string
	pendingFullScan := false
	defer func() {
		if pathTimer != nil {
			pathTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-pathEvents:
			if !ok {
				pathEvents = nil
				continue
			}
			pendingPaths = append(pendingPaths, event.Paths...)
			pendingFullScan = pendingFullScan || event.FullScan
			if pathTimer == nil {
				pathTimer = time.NewTimer(v2PathInvalidationDebounce)
				pathTimerC = pathTimer.C
			}
		case <-pathTimerC:
			queued := len(pathEvents)
			for index := 0; index < queued; index++ {
				event, ok := <-pathEvents
				if !ok {
					pathEvents = nil
					break
				}
				pendingPaths = append(pendingPaths, event.Paths...)
				pendingFullScan = pendingFullScan || event.FullScan
			}
			engine.config.Invalidate(pendingPaths, pendingFullScan)
			engine.commitFastSnapshot(engine.inspection, "pane_gone", pendingPaths)
			engine.scheduleSnapshot(engine.inspection, "pane_gone")
			pendingPaths = nil
			pendingFullScan = false
			pathTimer = nil
			pathTimerC = nil
		case pid, ok := <-processEvents:
			if !ok {
				processEvents = nil
				continue
			}
			engine.removeExitedProcess(pid)
		case socket, ok := <-socketEvents:
			if !ok {
				socketEvents = nil
				continue
			}
			engine.refreshNewSocket(socket)
		case result := <-inspectionResults:
			inspectionRunning = false
			if result.generation != engine.paneGeneration {
				log.Printf("v2 event engine: discarded stale pane inspection generation=%d current=%d", result.generation, engine.paneGeneration)
				continue
			}
			engine.applyPaneInspection(result.inspection, "pane_gone")
		case <-ticker.C:
			if inspectionRunning {
				continue
			}
			inspectionRunning = true
			generation := engine.paneGeneration
			go func(generation uint64) {
				inspection := engine.config.Inspect()
				select {
				case inspectionResults <- v2InspectionResult{inspection: inspection, generation: generation}:
				case <-ctx.Done():
				}
			}(generation)
		}
	}
}

func (engine *v2EventEngine) refreshNewSocket(socket string) {
	if !engine.inspection.valid {
		return
	}
	started := time.Now()
	raw := engine.config.InspectSocket(socket)
	if len(raw) == 0 {
		log.Printf("v2 discovery: operation=socket-event socket=%q result=no_panes duration_ms=%.3f", socket, float64(time.Since(started))/float64(time.Millisecond))
		return
	}
	inspection := augmentV2InspectionWithNewPanes(engine.inspection, raw, engine.config.ClassifyPane, func([]int) processFiles {
		return processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true}
	})
	if sameV2PaneIdentitySet(engine.inspection, inspection) {
		return
	}
	engine.inspection = inspection
	engine.paneGeneration++
	if err := engine.config.ProcessWatcher.Set(v2AgentRootPIDs(inspection)); err != nil {
		log.Printf("v2 event engine: process watch update after socket event failed: %v", err)
	}
	engine.commitFastSnapshot(inspection, "pane_gone", nil)
	engine.scheduleSnapshot(inspection, "pane_gone")
	ended := time.Now()
	log.Printf("v2 discovery: operation=socket-event socket=%q result=committed duration_ms=%.3f end_us=%d", socket, float64(ended.Sub(started))/float64(time.Millisecond), ended.UnixMicro())
}

func (engine *v2EventEngine) refreshPanes(removalReason string) {
	engine.applyPaneInspection(engine.config.Inspect(), removalReason)
}

func (engine *v2EventEngine) applyPaneInspection(inspection liveInspection, removalReason string) {
	if !inspection.valid {
		log.Printf("v2 event engine: pane scan unavailable; preserving revision=%s", engine.config.Store.snapshot().Revision)
		return
	}
	if sameV2PaneIdentitySet(engine.inspection, inspection) {
		engine.inspection = inspection
		return
	}
	engine.inspection = inspection
	engine.paneGeneration++
	if err := engine.config.ProcessWatcher.Set(v2AgentRootPIDs(inspection)); err != nil {
		log.Printf("v2 event engine: process watch update failed: %v", err)
	}
	engine.commitFastSnapshot(inspection, removalReason, nil)
	engine.scheduleSnapshot(inspection, removalReason)
}

func (engine *v2EventEngine) commitFastSnapshot(inspection liveInspection, removalReason string, paths []string) {
	if engine.config.FastBuild == nil || !inspection.valid {
		return
	}
	started := time.Now()
	input, ok, err := engine.config.FastBuild(inspection, paths)
	if err != nil {
		log.Printf("v2 event engine: pane-first commit failed: %v", err)
	} else if ok {
		engine.config.Store.rebuildMu.Lock()
		engine.config.Store.commitWithRemovalReason(input, removalReason)
		engine.config.Store.rebuildMu.Unlock()
		log.Printf("v2 event engine: pane-first commit entries=%d duration_ms=%.3f", len(input.Entries), float64(time.Since(started))/float64(time.Millisecond))
	}
}

func (engine *v2EventEngine) scheduleSnapshot(inspection liveInspection, removalReason string) {
	if !inspection.valid {
		return
	}
	engine.snapshotMu.Lock()
	engine.snapshotVersion++
	engine.snapshotPending = &v2SnapshotBuild{inspection: inspection, removalReason: removalReason, version: engine.snapshotVersion}
	if engine.snapshotRunning {
		engine.snapshotMu.Unlock()
		return
	}
	engine.snapshotRunning = true
	engine.snapshotMu.Unlock()
	go engine.runSnapshotBuilds()
}

func (engine *v2EventEngine) runSnapshotBuilds() {
	for {
		engine.snapshotMu.Lock()
		request := engine.snapshotPending
		engine.snapshotPending = nil
		engine.snapshotMu.Unlock()
		if request == nil {
			engine.snapshotMu.Lock()
			if engine.snapshotPending == nil {
				engine.snapshotRunning = false
				engine.snapshotMu.Unlock()
				return
			}
			engine.snapshotMu.Unlock()
			continue
		}
		input, err := engine.config.Build(request.inspection)
		engine.snapshotMu.Lock()
		currentVersion := engine.snapshotVersion
		stale := request.version != currentVersion
		initial := engine.config.Store.snapshot().Revision == ""
		if err == nil && (!stale || initial) {
			engine.config.Store.rebuildMu.Lock()
			engine.config.Store.commitWithRemovalReason(input, request.removalReason)
			engine.config.Store.rebuildMu.Unlock()
			if input.visibleRecords != nil {
				publishVisibleSessions(input.visibleRecords)
			}
		}
		engine.snapshotMu.Unlock()
		if err != nil {
			log.Printf("v2 event engine: snapshot refresh failed: %v", err)
		} else if stale && initial {
			log.Printf("v2 event engine: committed stale initial snapshot version=%d current=%d; follow-up pending", request.version, currentVersion)
		} else if stale {
			log.Printf("v2 event engine: discarded stale snapshot build version=%d current=%d", request.version, currentVersion)
		}
	}
}

func (engine *v2EventEngine) refreshSnapshot(removalReason string) {
	if !engine.inspection.valid {
		return
	}
	engine.config.Store.rebuildMu.Lock()
	defer engine.config.Store.rebuildMu.Unlock()
	input, err := engine.config.Build(engine.inspection)
	if err != nil {
		log.Printf("v2 event engine: snapshot refresh failed: %v", err)
		return
	}
	engine.config.Store.commitWithRemovalReason(input, removalReason)
	if input.visibleRecords != nil {
		publishVisibleSessions(input.visibleRecords)
	}
}

func (engine *v2EventEngine) removeExitedProcess(pid int) {
	panes := engine.inspection.panes[:0]
	removed := false
	for _, pane := range engine.inspection.panes {
		if v2AgentRootPID(pane) == pid {
			removed = true
			continue
		}
		panes = append(panes, pane)
	}
	if !removed {
		return
	}
	engine.inspection.panes = panes
	engine.paneGeneration++
	if err := engine.config.ProcessWatcher.Set(v2AgentRootPIDs(engine.inspection)); err != nil {
		log.Printf("v2 event engine: process watch update after exit failed: %v", err)
	}
	engine.commitFastSnapshot(engine.inspection, "process_exit", nil)
	engine.scheduleSnapshot(engine.inspection, "process_exit")
}

func v2AgentRootPID(pane paneBinding) int {
	if len(pane.ProcessPIDs) == 0 {
		return 0
	}
	return pane.ProcessPIDs[0]
}

type v2PaneIdentityKey struct {
	socket   string
	paneID   string
	kind     string
	panePID  int
	agentPID int
}

func sameV2PaneIdentitySet(previous, current liveInspection) bool {
	if !previous.valid || !current.valid || len(previous.panes) != len(current.panes) {
		return false
	}
	identities := make(map[v2PaneIdentityKey]struct{}, len(previous.panes))
	for _, pane := range previous.panes {
		identities[v2PaneIdentityKey{
			socket: pane.Socket, paneID: pane.TmuxID, kind: pane.Kind,
			panePID: pane.PanePID, agentPID: v2AgentRootPID(pane),
		}] = struct{}{}
	}
	for _, pane := range current.panes {
		key := v2PaneIdentityKey{
			socket: pane.Socket, paneID: pane.TmuxID, kind: pane.Kind,
			panePID: pane.PanePID, agentPID: v2AgentRootPID(pane),
		}
		if _, ok := identities[key]; !ok {
			return false
		}
	}
	return true
}

func v2AgentRootPIDs(inspection liveInspection) []int {
	seen := map[int]bool{}
	var pids []int
	for _, pane := range inspection.panes {
		if pid := v2AgentRootPID(pane); pid > 0 && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func invalidateV2RecordPaths(paths []string, fullScan bool) {
	// FSEvents only wakes the snapshot refresh. Each cache lookup validates the
	// current device/inode/size/mtime tuple, so clearing here would turn a burst
	// or watcher overflow into a full reparse of every record.
	_, _ = paths, fullScan
}

func inspectV2LiveState() liveInspection {
	inspection := inspectLiveStateMaxAgeWithSocketCandidateRefresh(0, true)
	if !inspection.valid {
		return inspection
	}
	raw := listRawPanesWithSocketCandidateRefresh(true)
	return augmentV2InspectionWithNewPanes(inspection, raw, targetedPaneAgent, inspectProcessFiles)
}

func buildV2SnapshotInputFromInspection(store *terminalEntryStore, inspection liveInspection) (v2SnapshotInput, error) {
	records, _ := collectAllRecords(false)
	history := loadClaudeHistoryIndex()
	records = bindInspectionDetailed(records, inspection, weakBindings, history, time.Now()).records
	visible := filterVisibleRecords(records, time.Now().Add(-7*24*time.Hour), closedSessions)
	host := currentV2Host()
	input, err := v2InputFromRecords(host, visible, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return store.identifyPaneWithBackoff(host, pane, defaultV2PaneIdentifier)
	})
	if err == nil {
		input.visibleRecords = visible
	}
	return input, err
}

func buildV2PaneFirstInput(store *terminalEntryStore, inspection liveInspection, paths []string) (v2SnapshotInput, bool, error) {
	return buildV2PaneFirstInputWithIdentifier(store, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return store.identifyPaneWithBackoff(host, pane, defaultV2PaneIdentifier)
	}, paths)
}

func buildV2PaneFirstInputWithIdentifier(store *terminalEntryStore, inspection liveInspection, identify v2PaneIdentifier, paths []string) (v2SnapshotInput, bool, error) {
	current := store.snapshot()
	if current.Revision == "" {
		return v2SnapshotInput{}, false, nil
	}
	previous := make(map[string]v2TerminalEntry, len(current.Entries))
	for _, entry := range current.Entries {
		previous[entry.logicalKey] = entry
	}
	records := v2FastAttachmentRecords(inspection, paths)
	if len(records) > 0 {
		records = bindInspectionDetailed(records, inspection, weakBindings, nil, time.Now()).records
	}
	input, err := v2InputFromRecords(current.Host, records, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		if old, ok := previous[v2PaneLogicalKey(host, pane)]; ok && sameV2PaneRuntime(old, pane) {
			identity := old.runtime.Identity
			return identity, time.Unix(identity.StartSec, identity.StartUsec*1000), nil
		}
		return identify(host, pane)
	})
	if err != nil {
		return v2SnapshotInput{}, false, err
	}
	fastHistory := input.History
	input.History = append([]v2HistoryRecord(nil), current.History...)
	historyIDs := make(map[string]bool, len(input.History))
	for _, record := range input.History {
		historyIDs[record.RecordID] = true
	}
	for _, record := range fastHistory {
		if !historyIDs[record.RecordID] {
			input.History = append(input.History, record)
			historyIDs[record.RecordID] = true
		}
	}
	for _, entry := range current.Entries {
		if entry.Attachment != nil {
			input.History = append(input.History, v2HistoryRecordFromEntry(entry))
		}
	}
	for index := range input.Entries {
		old, ok := previous[input.Entries[index].Entry.logicalKey]
		if !ok || old.Attachment == nil {
			continue
		}
		record := v2HistoryRecordFromEntry(old)
		input.Entries[index].Record = &record
		input.Entries[index].EvidenceRank = 4
		input.Entries[index].Status = old.Attachment.Status
		input.Entries[index].SuspectReason = old.Attachment.SuspectReason
		if old.runtime != nil && old.runtime.Record != nil {
			input.Entries[index].BoundRecord = cloneSessionRecordForResponse(old.runtime.Record)
		}
	}
	return input, true, nil
}

func sameV2PaneRuntime(entry v2TerminalEntry, pane paneBinding) bool {
	if entry.runtime == nil {
		return false
	}
	binding := entry.runtime.Binding
	return binding.Socket == pane.Socket && binding.TmuxID == pane.TmuxID && binding.Kind == pane.Kind &&
		binding.PanePID == pane.PanePID && v2AgentRootPID(binding) == v2AgentRootPID(pane)
}

func v2FastAttachmentRecords(inspection liveInspection, paths []string) []*sessionRecord {
	if len(paths) == 0 {
		return nil
	}
	wanted := map[string]bool{}
	for _, pane := range inspection.panes {
		for _, pid := range pane.ProcessPIDs {
			if pane.Kind == "claude" {
				sessionID, source := claudeSessionHintEvidence(pid, inspection.snap.command[pid])
				if source != "argv" {
					if registryID := validatedClaudeSessionRegistryHint(pid, inspection.files.cwd[pid], inspection.snap.started[pid]); registryID != "" {
						sessionID, source = registryID, "registry"
					}
				}
				if source == "argv" || source == "registry" {
					wanted["claude\x00"+sessionID] = true
				}
			}
			if pane.Kind == "codex" {
				for _, path := range inspection.files.open[pid] {
					if sessionID := sessionIDFromPath(path); sessionID != "" {
						wanted["codex\x00"+sessionID] = true
					}
				}
			}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	physical := v2FastPhysicalFilesForPaths(paths)
	records := make([]*sessionRecord, 0, len(wanted))
	for key := range wanted {
		if file, ok := physical[key]; ok {
			if record := v2FastCachedMetadata(file, nil); record != nil {
				records = append(records, record)
			}
		}
	}
	return records
}

func v2PhysicalFilesForPaths(paths []string) map[string]physicalFile {
	files := map[string]physicalFile{}
	home := homeDir()
	claudeRoot := filepath.Join(home, ".claude", "projects")
	codexRoots := []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")}
	for _, path := range paths {
		clean := filepath.Clean(path)
		var kind, sessionID string
		if v2PathWithin(clean, claudeRoot) && !hasPathSegment(clean, "subagents") {
			name := filepath.Base(clean)
			sessionID = strings.TrimSuffix(name, ".jsonl")
			if !strings.HasSuffix(name, ".jsonl") || !uuidPattern.MatchString(sessionID) {
				continue
			}
			kind = "claude"
		} else {
			for _, root := range codexRoots {
				if v2PathWithin(clean, root) {
					kind, sessionID = "codex", sessionIDFromPath(clean)
					break
				}
			}
			if sessionID == "" {
				continue
			}
		}
		info, err := os.Stat(clean)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		device, inode := physicalIdentity(info)
		file := physicalFile{kind: kind, sessionID: strings.ToLower(sessionID), path: clean, mtime: info.ModTime(), size: info.Size(), device: device, inode: inode}
		key := kind + "\x00" + file.sessionID
		files[key] = preferPhysical(files[key], file)
	}
	return files
}

func v2PathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func v2HistoryRecordFromEntry(entry v2TerminalEntry) v2HistoryRecord {
	return v2HistoryRecord{
		RecordID: entry.Attachment.RecordID, SessionID: entry.Attachment.SessionID, Kind: entry.Kind, Cwd: entry.Cwd,
		Title: entry.Attachment.Title, State: entry.State, Model: entry.Model, LastActivityAt: entry.LastActivityAt,
		LastMessagePreview: entry.LastMessagePreview,
	}
}

func v2WatchRoots() []string {
	home := homeDir()
	return []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".codex", "sessions"),
	}
}

func startV2EventEngine(ctx context.Context, store *terminalEntryStore) (*v2EventEngine, error) {
	paths, err := newV2FSEventsWatcher(v2WatchRoots())
	if err != nil {
		return nil, err
	}
	processes, err := newV2ProcessWatcher()
	if err != nil {
		_ = paths.Close()
		return nil, err
	}
	socketDir := fmt.Sprintf("/private/tmp/tmux-%d", os.Getuid())
	sockets, err := newV2SocketWatcher(socketDir)
	if err != nil {
		_ = processes.Close()
		_ = paths.Close()
		return nil, err
	}
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, SocketWatcher: sockets,
		PaneInterval: v2PaneDiscoveryInterval, Inspect: inspectV2LiveState,
		Build: func(inspection liveInspection) (v2SnapshotInput, error) {
			return buildV2SnapshotInputFromInspection(store, inspection)
		}, FastBuild: func(inspection liveInspection, paths []string) (v2SnapshotInput, bool, error) {
			return buildV2PaneFirstInput(store, inspection, paths)
		}, Invalidate: invalidateV2RecordPaths,
	})
	engine.Start(ctx)
	return engine, nil
}
