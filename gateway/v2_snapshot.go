package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"
)

const v2RemovedEntryTTL = 10 * time.Minute

type v2Host struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type v2Pane struct {
	PaneID     string `json:"paneId"`
	WindowName string `json:"windowName,omitempty"`
}

type v2Attachment struct {
	RecordID           string `json:"recordId"`
	SessionID          string `json:"sessionId"`
	Title              string `json:"title"`
	Status             string `json:"status"`
	AttachmentRevision uint64 `json:"attachmentRevision"`
	SuspectReason      string `json:"suspectReason,omitempty"`
}

type v2TerminalEntry struct {
	logicalKey         string          `json:"-"`
	runtime            *v2EntryRuntime `json:"-"`
	EntryID            string          `json:"entryId"`
	Kind               string          `json:"kind"`
	Cwd                string          `json:"cwd"`
	State              string          `json:"state"`
	CanSend            bool            `json:"canSend"`
	LastActivityAt     string          `json:"lastActivityAt"`
	LastMessagePreview string          `json:"lastMessagePreview"`
	Model              string          `json:"model"`
	Pane               v2Pane          `json:"pane"`
	AttachmentRevision uint64          `json:"attachmentRevision"`
	Attachment         *v2Attachment   `json:"attachment"`
}

type v2EntryRuntime struct {
	Identity v2EntryIdentity
	Binding  paneBinding
	Record   *sessionRecord
}

type v2HistoryRecord struct {
	RecordID           string `json:"recordId"`
	SessionID          string `json:"sessionId"`
	Kind               string `json:"kind"`
	Cwd                string `json:"cwd"`
	Title              string `json:"title"`
	State              string `json:"state"`
	Model              string `json:"model"`
	LastActivityAt     string `json:"lastActivityAt"`
	LastMessagePreview string `json:"preview"`
}

type v2Snapshot struct {
	Revision string            `json:"revision"`
	Host     v2Host            `json:"host"`
	Entries  []v2TerminalEntry `json:"entries"`
	History  []v2HistoryRecord `json:"history"`
}

type v2EntryDraft struct {
	Entry         v2TerminalEntry
	Record        *v2HistoryRecord
	BoundRecord   *sessionRecord
	EvidenceRank  int
	Status        string
	SuspectReason string
}

type v2SnapshotInput struct {
	Host             v2Host
	Entries          []v2EntryDraft
	History          []v2HistoryRecord
	visibleRecords   []*sessionRecord
	degradedPaneKeys map[string]struct{}
}

type v2EntryIdentity struct {
	HostID       string
	SocketPath   string
	SocketDevice uint64
	SocketInode  uint64
	PaneID       string
	AgentPID     int
	StartSec     int64
	StartUsec    int64
}

type v2ProcessStarted struct {
	Sec  int64
	Usec int64
}

type terminalEntryStore struct {
	mu                   sync.Mutex
	rebuildMu            sync.Mutex
	ready                bool
	bootID               string
	counter              uint64
	current              v2Snapshot
	entries              map[string]v2TerminalEntry
	removed              map[string]time.Time
	pendingRemoval       map[string]string
	deliverySuspects     map[string]string
	subscribers          map[chan v2ChangeSet]struct{}
	deliverySubscribers  map[string]map[chan v2DeliveryEvent]struct{}
	paneIdentityFailures map[string]v2PaneIdentityFailure
	now                  func() time.Time
}

type v2PaneIdentityFailure struct {
	Failures  int
	RetryAt   time.Time
	LastError string
}

const (
	v2PaneIdentityRetryBase = 2 * time.Second
	v2PaneIdentityRetryMax  = 30 * time.Second
)

var v2Entries = newTerminalEntryStore(newV2BootID())

func newV2BootID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err == nil {
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func newTerminalEntryStore(bootID string) *terminalEntryStore {
	return &terminalEntryStore{
		bootID: bootID, entries: map[string]v2TerminalEntry{}, removed: map[string]time.Time{},
		pendingRemoval: map[string]string{}, subscribers: map[chan v2ChangeSet]struct{}{},
		deliverySuspects:    map[string]string{},
		deliverySubscribers: map[string]map[chan v2DeliveryEvent]struct{}{}, now: time.Now,
		paneIdentityFailures: map[string]v2PaneIdentityFailure{},
	}
}

func writeV2IdentityString(buffer *bytes.Buffer, value string) {
	_ = binary.Write(buffer, binary.BigEndian, uint32(len(value)))
	_, _ = buffer.WriteString(value)
}

func v2EntryID(identity v2EntryIdentity) string {
	var canonical bytes.Buffer
	writeV2IdentityString(&canonical, "v1")
	writeV2IdentityString(&canonical, identity.HostID)
	writeV2IdentityString(&canonical, filepath.Clean(identity.SocketPath))
	_ = binary.Write(&canonical, binary.BigEndian, identity.SocketDevice)
	_ = binary.Write(&canonical, binary.BigEndian, identity.SocketInode)
	writeV2IdentityString(&canonical, identity.PaneID)
	_ = binary.Write(&canonical, binary.BigEndian, int64(identity.AgentPID))
	_ = binary.Write(&canonical, binary.BigEndian, identity.StartSec)
	_ = binary.Write(&canonical, binary.BigEndian, identity.StartUsec)
	sum := sha256.Sum256(canonical.Bytes())
	return "e1_" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func cloneV2Attachment(value *v2Attachment) *v2Attachment {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneV2Entry(value v2TerminalEntry) v2TerminalEntry {
	value.Attachment = cloneV2Attachment(value.Attachment)
	if value.runtime != nil {
		runtime := *value.runtime
		runtime.Binding.ProcessPIDs = append([]int(nil), value.runtime.Binding.ProcessPIDs...)
		if value.runtime.Record != nil {
			runtime.Record = cloneSessionRecordForResponse(value.runtime.Record)
		}
		value.runtime = &runtime
	}
	return value
}

func cloneV2Snapshot(value v2Snapshot) v2Snapshot {
	copy := value
	copy.Entries = make([]v2TerminalEntry, len(value.Entries))
	for i := range value.Entries {
		copy.Entries[i] = cloneV2Entry(value.Entries[i])
	}
	copy.History = append(make([]v2HistoryRecord, 0, len(value.History)), value.History...)
	return copy
}

func sameV2Attachment(left, right *v2Attachment) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.RecordID == right.RecordID && left.SessionID == right.SessionID &&
		left.Title == right.Title && left.Status == right.Status && left.SuspectReason == right.SuspectReason
}

func resolveV2AttachmentCandidates(drafts []v2EntryDraft) {
	groups := map[string][]int{}
	for index := range drafts {
		if drafts[index].Record != nil && drafts[index].Record.RecordID != "" {
			groups[drafts[index].Record.RecordID] = append(groups[drafts[index].Record.RecordID], index)
		}
	}
	for _, indexes := range groups {
		if len(indexes) < 2 {
			continue
		}
		maxRank, winners := drafts[indexes[0]].EvidenceRank, []int{indexes[0]}
		for _, index := range indexes[1:] {
			rank := drafts[index].EvidenceRank
			switch {
			case rank > maxRank:
				maxRank, winners = rank, []int{index}
			case rank == maxRank:
				winners = append(winners, index)
			}
		}
		winner := -1
		if len(winners) == 1 {
			winner = winners[0]
		}
		for _, index := range indexes {
			if index != winner {
				drafts[index].Record = nil
				drafts[index].BoundRecord = nil
			}
		}
	}
}

func (store *terminalEntryStore) commit(input v2SnapshotInput) v2Snapshot {
	return store.commitWithRemovalReason(input, "pane_gone")
}

func (store *terminalEntryStore) commitWithRemovalReason(input v2SnapshotInput, removalReason string) v2Snapshot {
	store.mu.Lock()
	defer store.mu.Unlock()

	drafts := append([]v2EntryDraft(nil), input.Entries...)
	degradedRecordOwners := map[string]string{}
	for _, previousEntry := range store.entries {
		if _, degraded := input.degradedPaneKeys[previousEntry.logicalKey]; degraded && previousEntry.Attachment != nil {
			degradedRecordOwners[previousEntry.Attachment.RecordID] = previousEntry.logicalKey
		}
	}
	for index := range drafts {
		if drafts[index].Record == nil {
			continue
		}
		owner := degradedRecordOwners[drafts[index].Record.RecordID]
		if owner != "" && owner != drafts[index].Entry.logicalKey {
			drafts[index].Record = nil
			drafts[index].BoundRecord = nil
		}
	}
	resolveV2AttachmentCandidates(drafts)
	previous := store.entries
	attached := map[string]bool{}
	entries := make([]v2TerminalEntry, 0, len(drafts))
	for _, draft := range drafts {
		entry := cloneV2Entry(draft.Entry)
		entry.Attachment = nil
		if draft.Record != nil {
			if entry.runtime != nil && draft.BoundRecord != nil {
				entry.runtime.Record = cloneSessionRecordForResponse(draft.BoundRecord)
			}
			status := draft.Status
			if status == "" {
				status = "attached"
			}
			entry.Attachment = &v2Attachment{
				RecordID: draft.Record.RecordID, SessionID: draft.Record.SessionID, Title: draft.Record.Title,
				Status: status, SuspectReason: draft.SuspectReason,
			}
			entry.State = draft.Record.State
			entry.LastActivityAt = draft.Record.LastActivityAt
			entry.LastMessagePreview = draft.Record.LastMessagePreview
			entry.Model = draft.Record.Model
			attached[draft.Record.RecordID] = true
		}
		if entry.Attachment != nil {
			if suspectRecord := store.deliverySuspects[entry.EntryID]; suspectRecord == entry.Attachment.RecordID {
				entry.Attachment.Status = "suspect"
				entry.Attachment.SuspectReason = "delivery_unattributed"
			} else if suspectRecord != "" {
				delete(store.deliverySuspects, entry.EntryID)
			}
		} else {
			delete(store.deliverySuspects, entry.EntryID)
		}
		if old, ok := previous[entry.EntryID]; ok {
			entry.AttachmentRevision = old.AttachmentRevision
			if !sameV2Attachment(old.Attachment, entry.Attachment) {
				entry.AttachmentRevision++
			}
		} else if entry.Attachment != nil {
			entry.AttachmentRevision = 1
		} else {
			entry.AttachmentRevision = 0
		}
		if entry.Attachment != nil {
			entry.Attachment.AttachmentRevision = entry.AttachmentRevision
		}
		entries = append(entries, entry)
	}
	presentLogicalKeys := make(map[string]bool, len(entries))
	for _, entry := range entries {
		presentLogicalKeys[entry.logicalKey] = true
	}
	for _, previousEntry := range previous {
		if _, degraded := input.degradedPaneKeys[previousEntry.logicalKey]; !degraded || presentLogicalKeys[previousEntry.logicalKey] {
			continue
		}
		preserved := cloneV2Entry(previousEntry)
		entries = append(entries, preserved)
		presentLogicalKeys[preserved.logicalKey] = true
		if preserved.Attachment != nil {
			attached[preserved.Attachment.RecordID] = true
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntryID < entries[j].EntryID })

	history := make([]v2HistoryRecord, 0, len(input.History))
	seenHistory := map[string]bool{}
	for _, record := range input.History {
		if record.RecordID == "" || attached[record.RecordID] || seenHistory[record.RecordID] {
			continue
		}
		seenHistory[record.RecordID] = true
		history = append(history, record)
	}
	sort.Slice(history, func(i, j int) bool {
		if history[i].LastActivityAt == history[j].LastActivityAt {
			return history[i].RecordID < history[j].RecordID
		}
		return history[i].LastActivityAt > history[j].LastActivityAt
	})
	store.ready = true

	if reflect.DeepEqual(store.current.Host, input.Host) && reflect.DeepEqual(store.current.Entries, entries) && reflect.DeepEqual(store.current.History, history) {
		return cloneV2Snapshot(store.current)
	}
	now := store.now()
	nextEntries := make(map[string]v2TerminalEntry, len(entries))
	for _, entry := range entries {
		nextEntries[entry.EntryID] = cloneV2Entry(entry)
		delete(store.removed, entry.EntryID)
	}
	for entryID := range previous {
		if _, ok := nextEntries[entryID]; !ok {
			store.removed[entryID] = now
		}
	}
	for entryID, removedAt := range store.removed {
		if now.Sub(removedAt) > v2RemovedEntryTTL {
			delete(store.removed, entryID)
		}
	}
	previousSnapshot := cloneV2Snapshot(store.current)
	store.counter++
	store.entries = nextEntries
	store.current = v2Snapshot{
		Revision: fmt.Sprintf("rv1_%s_%d", store.bootID, store.counter),
		Host:     input.Host, Entries: entries, History: history,
	}
	change := v2ChangeSetForSnapshots(previousSnapshot, store.current, removalReason)
	for index := range change.Removed {
		if reason := store.pendingRemoval[change.Removed[index].EntryID]; reason != "" {
			change.Removed[index].Reason = reason
			delete(store.pendingRemoval, change.Removed[index].EntryID)
		}
	}
	store.publishLocked(change)
	return cloneV2Snapshot(store.current)
}

func (store *terminalEntryStore) snapshot() v2Snapshot {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneV2Snapshot(store.current)
}

func (store *terminalEntryStore) lookup(entryID string) (v2TerminalEntry, int) {
	entry, status, _ := store.lookupCurrent(entryID)
	return entry, status
}

func (store *terminalEntryStore) lookupCurrent(entryID string) (v2TerminalEntry, int, string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if entry, ok := store.entries[entryID]; ok {
		return cloneV2Entry(entry), http.StatusOK, store.current.Revision
	}
	if !store.ready {
		return v2TerminalEntry{}, http.StatusServiceUnavailable, store.current.Revision
	}
	if removedAt, ok := store.removed[entryID]; ok {
		if store.now().Sub(removedAt) <= v2RemovedEntryTTL {
			return v2TerminalEntry{}, http.StatusGone, store.current.Revision
		}
		delete(store.removed, entryID)
	}
	return v2TerminalEntry{}, http.StatusNotFound, store.current.Revision
}

func (store *terminalEntryStore) lookupV1WriteRecord(recordID string) (*sessionRecord, v2TerminalEntry, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, entry := range store.entries {
		if entry.Attachment == nil || entry.Attachment.RecordID != recordID {
			continue
		}
		if entry.runtime != nil && entry.runtime.Record != nil {
			return cloneSessionRecordForResponse(entry.runtime.Record), cloneV2Entry(entry), true
		}
		return &sessionRecord{AgentSession: AgentSession{
			ID: recordID, SessionID: entry.Attachment.SessionID, Kind: entry.Kind, Cwd: entry.Cwd,
			Title: entry.Attachment.Title, State: entry.State, Live: true, CanSend: false,
			BindingReason: "binding_unavailable",
		}}, cloneV2Entry(entry), true
	}
	for _, record := range store.current.History {
		if record.RecordID == recordID {
			return &sessionRecord{AgentSession: AgentSession{
				ID: record.RecordID, SessionID: record.SessionID, Kind: record.Kind, Cwd: record.Cwd,
				Title: record.Title, State: record.State, Model: record.Model, Live: false, CanSend: false,
				LastActivityAt: record.LastActivityAt, LastMessagePreview: record.LastMessagePreview,
				BindingReason: "no_pane_candidate",
			}}, v2TerminalEntry{}, false
		}
	}
	return nil, v2TerminalEntry{}, false
}

func v2RecordFromSession(record *sessionRecord) v2HistoryRecord {
	return v2HistoryRecord{
		RecordID: record.ID, SessionID: record.SessionID, Kind: record.Kind, Cwd: record.Cwd,
		Title: record.Title, State: record.State, Model: record.Model,
		LastActivityAt: record.LastActivityAt, LastMessagePreview: record.LastMessagePreview,
	}
}

func v2EvidenceRank(evidence string) int {
	switch evidence {
	case "strong":
		return 3
	case "active_writer":
		return 2
	case "history":
		return 1
	default:
		return 0
	}
}

type v2PaneIdentifier func(v2Host, paneBinding) (v2EntryIdentity, time.Time, error)

func (store *terminalEntryStore) identifyPaneWithBackoff(host v2Host, pane paneBinding, identify v2PaneIdentifier) (v2EntryIdentity, time.Time, error) {
	key := v2PaneLogicalKey(host, pane)
	store.mu.Lock()
	now := store.now()
	failure, failed := store.paneIdentityFailures[key]
	if failed && now.Before(failure.RetryAt) {
		store.mu.Unlock()
		return v2EntryIdentity{}, time.Time{}, fmt.Errorf("identity retry deferred until %s after %s", failure.RetryAt.UTC().Format(time.RFC3339Nano), failure.LastError)
	}
	store.mu.Unlock()

	identity, started, err := identify(host, pane)
	if err == nil {
		store.mu.Lock()
		previous, recovered := store.paneIdentityFailures[key]
		delete(store.paneIdentityFailures, key)
		store.mu.Unlock()
		if recovered {
			log.Printf("v2 pane identity: socket=%q pane=%s result=recovered previous_failures=%d", pane.Socket, pane.TmuxID, previous.Failures)
		}
		return identity, started, nil
	}

	store.mu.Lock()
	failure = store.paneIdentityFailures[key]
	failure.Failures++
	delay := v2PaneIdentityRetryBase
	for attempt := 1; attempt < failure.Failures && delay < v2PaneIdentityRetryMax; attempt++ {
		delay *= 2
	}
	if delay > v2PaneIdentityRetryMax {
		delay = v2PaneIdentityRetryMax
	}
	failure.RetryAt = now.Add(delay)
	failure.LastError = err.Error()
	store.paneIdentityFailures[key] = failure
	store.mu.Unlock()
	log.Printf("v2 pane identity: socket=%q pane=%s result=degraded failures=%d retry_after=%s error=%v", pane.Socket, pane.TmuxID, failure.Failures, delay, err)
	return v2EntryIdentity{}, time.Time{}, err
}

func defaultV2PaneIdentifier(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
	info, err := os.Stat(pane.Socket)
	if err != nil {
		return v2EntryIdentity{}, time.Time{}, err
	}
	device, inode := physicalIdentity(info)
	agentPID := v2AgentRootPID(pane)
	if agentPID <= 0 {
		return v2EntryIdentity{}, time.Time{}, fmt.Errorf("pane %s has no real agent process", pane.TmuxID)
	}
	started, err := v2ProcessStart(agentPID)
	if err != nil {
		return v2EntryIdentity{}, time.Time{}, err
	}
	identity := v2IdentityForPane(host, pane, device, inode, started)
	return identity, time.Unix(started.Sec, started.Usec*1000), nil
}

func v2IdentityForPane(host v2Host, pane paneBinding, device, inode uint64, started v2ProcessStarted) v2EntryIdentity {
	return v2EntryIdentity{
		HostID: host.ID, SocketPath: pane.Socket, SocketDevice: device, SocketInode: inode,
		PaneID: pane.TmuxID, AgentPID: v2AgentRootPID(pane), StartSec: started.Sec, StartUsec: started.Usec,
	}
}

func v2InputFromRecords(host v2Host, records []*sessionRecord, inspection liveInspection, identify v2PaneIdentifier) (v2SnapshotInput, error) {
	bound := map[string]*sessionRecord{}
	history := make([]v2HistoryRecord, 0, len(records))
	for _, record := range records {
		if record.SessionFile == "" || record.ID == "" {
			continue
		}
		history = append(history, v2RecordFromSession(record))
		if record.binding != nil {
			key := paneIdentityKey(record.binding)
			if current := bound[key]; current == nil || v2EvidenceRank(record.bindingEvidence) > v2EvidenceRank(current.bindingEvidence) {
				bound[key] = record
			}
		}
	}

	drafts := make([]v2EntryDraft, 0, len(inspection.panes))
	degradedPaneKeys := map[string]struct{}{}
	for _, pane := range inspection.panes {
		identity, started, err := identify(host, pane)
		if err != nil {
			degradedPaneKeys[v2PaneLogicalKey(host, pane)] = struct{}{}
			continue
		}
		entryID := v2EntryID(identity)
		cwd, _ := fallbackPaneIdentity(&pane, inspection.files, inspection.snap)
		if cwd == "" {
			cwd = "未知目录"
		}
		draft := v2EntryDraft{Entry: v2TerminalEntry{
			logicalKey: v2PaneLogicalKey(host, pane),
			runtime:    &v2EntryRuntime{Identity: identity, Binding: pane},
			EntryID:    entryID, Kind: pane.Kind, Cwd: cwd, State: "idle", CanSend: true,
			LastActivityAt: started.UTC().Format(time.RFC3339Nano),
			Pane:           v2Pane{PaneID: pane.TmuxID, WindowName: pane.WindowName},
		}}
		if record := bound[paneIdentityKey(&pane)]; record != nil {
			copy := v2RecordFromSession(record)
			draft.Record = &copy
			draft.BoundRecord = record
			draft.EvidenceRank = v2EvidenceRank(record.bindingEvidence)
			if record.BindingReason == "delivery_reconciliation_failed" {
				draft.Status = "suspect"
				draft.SuspectReason = "delivery_unattributed"
			}
		}
		drafts = append(drafts, draft)
	}
	if len(drafts) == 0 && len(inspection.panes) > 0 {
		return v2SnapshotInput{}, fmt.Errorf("identify all %d panes failed", len(inspection.panes))
	}
	return v2SnapshotInput{Host: host, Entries: drafts, History: history, degradedPaneKeys: degradedPaneKeys}, nil
}

func v2PaneLogicalKey(host v2Host, pane paneBinding) string {
	return host.ID + "\x00" + filepath.Clean(pane.Socket) + "\x00" + pane.TmuxID
}

func currentV2Host() v2Host {
	hostname := hostnameOrEmpty()
	if os.Getenv("TSNET_DISABLE") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
		if err == nil {
			var status struct {
				BackendState string
				Self         struct {
					HostName     string
					TailscaleIPs []string
				}
			}
			if json.Unmarshal(out, &status) == nil && status.BackendState == "Running" && len(status.Self.TailscaleIPs) > 0 {
				if status.Self.HostName != "" {
					hostname = status.Self.HostName
				}
				return v2Host{ID: status.Self.TailscaleIPs[0], Name: hostname}
			}
		}
	}
	sum := sha256.Sum256([]byte(hostname))
	return v2Host{ID: "local_" + base64.RawURLEncoding.EncodeToString(sum[:9]), Name: hostname}
}

func buildV2SnapshotInput() (v2SnapshotInput, error) {
	records, valid := collectAllRecords(true)
	if !valid {
		return v2SnapshotInput{}, errors.New("live session scan unavailable")
	}
	inspection := inspectLiveStateMaxAge(maxWriteBindingAge)
	if !inspection.valid || inspection.observedAt.IsZero() || time.Since(inspection.observedAt) > maxWriteBindingAge {
		return v2SnapshotInput{}, errors.New("live pane scan unavailable")
	}
	visible := filterVisibleRecords(records, time.Now().Add(-7*24*time.Hour), closedSessions)
	host := currentV2Host()
	input, err := v2InputFromRecords(host, visible, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return v2Entries.identifyPaneWithBackoff(host, pane, defaultV2PaneIdentifier)
	})
	if err == nil {
		input.visibleRecords = visible
	}
	return input, err
}

type v2ErrorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func writeV2Error(w http.ResponseWriter, status int, code, message string, retryable bool) {
	var envelope v2ErrorEnvelope
	envelope.Error.Code = code
	envelope.Error.Message = message
	envelope.Error.Retryable = retryable
	writeJSON(w, status, envelope)
}

func serveV2Snapshot(w http.ResponseWriter, r *http.Request, store *terminalEntryStore, build func() (v2SnapshotInput, error)) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	store.rebuildMu.Lock()
	input, err := build()
	if err != nil {
		store.rebuildMu.Unlock()
		log.Printf("v2 snapshot scan failed: %v", err)
		writeV2Error(w, http.StatusServiceUnavailable, "snapshot_unavailable", err.Error(), true)
		return
	}
	snapshot := store.commit(input)
	if input.visibleRecords != nil {
		publishVisibleSessions(input.visibleRecords)
	}
	store.rebuildMu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}

func handleV2Snapshot(w http.ResponseWriter, r *http.Request) {
	serveV2SnapshotCurrent(w, r, v2Entries)
}

func serveV2SnapshotCurrent(w http.ResponseWriter, r *http.Request, store *terminalEntryStore) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	snapshot := store.snapshot()
	if snapshot.Revision == "" {
		writeV2Error(w, http.StatusServiceUnavailable, "snapshot_unavailable", "initial snapshot is not ready", true)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
