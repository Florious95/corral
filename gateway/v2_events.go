package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"
)

type v2AttachmentChanged struct {
	EntryID            string        `json:"entryId"`
	PreviousRecordID   string        `json:"previousRecordId,omitempty"`
	AttachmentRevision uint64        `json:"attachmentRevision"`
	Attachment         *v2Attachment `json:"attachment"`
}

type v2EntryRemoved struct {
	EntryID string `json:"entryId"`
	Reason  string `json:"reason"`
}

type v2DeliveryEvent struct {
	Type         string    `json:"type"`
	EntryID      string    `json:"entryId"`
	ClientNonce  string    `json:"clientNonce"`
	DeliveryID   string    `json:"deliveryId"`
	Status       string    `json:"status"`
	ProducedAt   time.Time `json:"-"`
	TraceStarted time.Time `json:"-"`
}

type v2ChangeSet struct {
	PreviousRevision string
	Revision         string
	SnapshotRequired bool
	Attachments      []v2AttachmentChanged
	Removed          []v2EntryRemoved
}

func v2ChangeSetForSnapshots(previous, current v2Snapshot, removalReason string) v2ChangeSet {
	change := v2ChangeSet{PreviousRevision: previous.Revision, Revision: current.Revision}
	previousEntries := make(map[string]v2TerminalEntry, len(previous.Entries))
	for _, entry := range previous.Entries {
		previousEntries[entry.EntryID] = entry
	}
	currentEntries := make(map[string]v2TerminalEntry, len(current.Entries))
	currentLogicalKeys := make(map[string]bool, len(current.Entries))
	for _, entry := range current.Entries {
		currentEntries[entry.EntryID] = entry
		if entry.logicalKey != "" {
			currentLogicalKeys[entry.logicalKey] = true
		}
		old, existed := previousEntries[entry.EntryID]
		if existed && sameV2Attachment(old.Attachment, entry.Attachment) {
			continue
		}
		if !existed && entry.Attachment == nil {
			continue
		}
		previousRecordID := ""
		if old.Attachment != nil {
			previousRecordID = old.Attachment.RecordID
		}
		change.Attachments = append(change.Attachments, v2AttachmentChanged{
			EntryID: entry.EntryID, PreviousRecordID: previousRecordID,
			AttachmentRevision: entry.AttachmentRevision, Attachment: cloneV2Attachment(entry.Attachment),
		})
	}
	if removalReason == "" {
		removalReason = "pane_gone"
	}
	for _, entry := range previous.Entries {
		if _, exists := currentEntries[entry.EntryID]; !exists {
			reason := removalReason
			if entry.logicalKey != "" && currentLogicalKeys[entry.logicalKey] {
				reason = "generation_replaced"
			}
			change.Removed = append(change.Removed, v2EntryRemoved{EntryID: entry.EntryID, Reason: reason})
		}
	}
	sort.Slice(change.Attachments, func(i, j int) bool { return change.Attachments[i].EntryID < change.Attachments[j].EntryID })
	sort.Slice(change.Removed, func(i, j int) bool { return change.Removed[i].EntryID < change.Removed[j].EntryID })
	return change
}

func (store *terminalEntryStore) publishLocked(change v2ChangeSet) {
	for subscriber := range store.subscribers {
		select {
		case subscriber <- change:
		default:
			select {
			case <-subscriber:
			default:
			}
			subscriber <- v2ChangeSet{
				PreviousRevision: change.PreviousRevision,
				Revision:         change.Revision,
				SnapshotRequired: true,
			}
		}
	}
}

func (store *terminalEntryStore) subscribe(afterRevision string) (<-chan v2ChangeSet, func()) {
	store.mu.Lock()
	defer store.mu.Unlock()
	changes := make(chan v2ChangeSet, 1)
	store.subscribers[changes] = struct{}{}
	if afterRevision != "" && afterRevision != store.current.Revision {
		changes <- v2ChangeSet{
			PreviousRevision: afterRevision,
			Revision:         store.current.Revision,
			SnapshotRequired: true,
		}
	}
	return changes, func() {
		store.mu.Lock()
		delete(store.subscribers, changes)
		store.mu.Unlock()
	}
}

func (store *terminalEntryStore) subscribeDeliveries(entryID string) (<-chan v2DeliveryEvent, func()) {
	store.mu.Lock()
	defer store.mu.Unlock()
	events := make(chan v2DeliveryEvent, 8)
	if store.deliverySubscribers[entryID] == nil {
		store.deliverySubscribers[entryID] = map[chan v2DeliveryEvent]struct{}{}
	}
	store.deliverySubscribers[entryID][events] = struct{}{}
	return events, func() {
		store.mu.Lock()
		delete(store.deliverySubscribers[entryID], events)
		if len(store.deliverySubscribers[entryID]) == 0 {
			delete(store.deliverySubscribers, entryID)
		}
		store.mu.Unlock()
	}
}

func (store *terminalEntryStore) publishDelivery(event v2DeliveryEvent) {
	if event.ProducedAt.IsZero() {
		event.ProducedAt = time.Now()
	}
	if event.TraceStarted.IsZero() {
		event.TraceStarted = event.ProducedAt
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for subscriber := range store.deliverySubscribers[event.EntryID] {
		select {
		case subscriber <- event:
		default:
			// Delivery events are advisory UI acknowledgements. A slow client can
			// recover from the HTTP response and the attached timeline.
		}
	}
}

func logV2DeliverySSE(event v2DeliveryEvent, writtenAt time.Time, result string) {
	producedAt := event.ProducedAt
	if producedAt.IsZero() {
		producedAt = writtenAt
	}
	traceStarted := event.TraceStarted
	if traceStarted.IsZero() {
		traceStarted = producedAt
	}
	log.Printf("v2 sse timing: delivery=%s entry=%s status=%s produced_us=%d written_us=%d latency_ms=%.3f total_ms=%.3f result=%s",
		event.DeliveryID, event.EntryID, event.Status, producedAt.UnixMicro(), writtenAt.UnixMicro(),
		float64(writtenAt.Sub(producedAt))/float64(time.Millisecond), float64(writtenAt.Sub(traceStarted))/float64(time.Millisecond), result)
}

func (store *terminalEntryStore) markPendingRemoval(entryID, reason string) {
	store.mu.Lock()
	if _, ok := store.entries[entryID]; ok {
		store.pendingRemoval[entryID] = reason
	}
	store.mu.Unlock()
}

func (store *terminalEntryStore) clearPendingRemoval(entryID string) {
	store.mu.Lock()
	delete(store.pendingRemoval, entryID)
	store.mu.Unlock()
}

func (store *terminalEntryStore) removeEntry(entryID, reason string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.entries[entryID]; !ok {
		delete(store.pendingRemoval, entryID)
		return false
	}
	previous := cloneV2Snapshot(store.current)
	entries := make([]v2TerminalEntry, 0, len(store.current.Entries)-1)
	for _, entry := range store.current.Entries {
		if entry.EntryID != entryID {
			entries = append(entries, cloneV2Entry(entry))
		}
	}
	delete(store.entries, entryID)
	delete(store.deliverySuspects, entryID)
	store.removed[entryID] = store.now()
	if pending := store.pendingRemoval[entryID]; pending != "" {
		reason = pending
	}
	delete(store.pendingRemoval, entryID)
	store.counter++
	store.current.Entries = entries
	store.current.Revision = fmt.Sprintf("rv1_%s_%d", store.bootID, store.counter)
	store.publishLocked(v2ChangeSetForSnapshots(previous, store.current, reason))
	return true
}

func (store *terminalEntryStore) markAttachmentSuspect(entryID, reason string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[entryID]
	if !ok || entry.Attachment == nil {
		return false
	}
	if entry.Attachment.Status == "suspect" && entry.Attachment.SuspectReason == reason {
		return true
	}
	previous := cloneV2Snapshot(store.current)
	entry = cloneV2Entry(entry)
	store.deliverySuspects[entryID] = entry.Attachment.RecordID
	entry.AttachmentRevision++
	entry.Attachment.Status = "suspect"
	entry.Attachment.SuspectReason = reason
	entry.Attachment.AttachmentRevision = entry.AttachmentRevision
	store.entries[entryID] = entry
	for index := range store.current.Entries {
		if store.current.Entries[index].EntryID == entryID {
			store.current.Entries[index] = cloneV2Entry(entry)
			break
		}
	}
	store.counter++
	store.current.Revision = fmt.Sprintf("rv1_%s_%d", store.bootID, store.counter)
	store.publishLocked(v2ChangeSetForSnapshots(previous, store.current, ""))
	return true
}

func (store *terminalEntryStore) clearAttachmentSuspect(entryID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.deliverySuspects, entryID)
	entry, ok := store.entries[entryID]
	if !ok || entry.Attachment == nil || entry.Attachment.SuspectReason != "delivery_unattributed" {
		return false
	}
	previous := cloneV2Snapshot(store.current)
	entry = cloneV2Entry(entry)
	entry.AttachmentRevision++
	entry.Attachment.Status = "attached"
	entry.Attachment.SuspectReason = ""
	entry.Attachment.AttachmentRevision = entry.AttachmentRevision
	store.entries[entryID] = entry
	for index := range store.current.Entries {
		if store.current.Entries[index].EntryID == entryID {
			store.current.Entries[index] = cloneV2Entry(entry)
			break
		}
	}
	store.counter++
	store.current.Revision = fmt.Sprintf("rv1_%s_%d", store.bootID, store.counter)
	store.publishLocked(v2ChangeSetForSnapshots(previous, store.current, ""))
	return true
}

func writeV2SSEEvent(w http.ResponseWriter, eventType, revision string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", revision, eventType, payload)
	return err
}

func writeV2ChangeSet(w http.ResponseWriter, change v2ChangeSet) error {
	base := map[string]any{
		"previousRevision": change.PreviousRevision,
		"revision":         change.Revision,
	}
	if change.SnapshotRequired {
		base["type"] = "snapshot_required"
		return writeV2SSEEvent(w, "snapshot_required", change.Revision, base)
	}
	// Notify the atomic snapshot first. If a client disconnects between events
	// sharing this revision, its Last-Event-ID still reflects a refresh signal.
	base["type"] = "snapshot_changed"
	if err := writeV2SSEEvent(w, "snapshot_changed", change.Revision, base); err != nil {
		return err
	}
	for _, attachment := range change.Attachments {
		payload := map[string]any{
			"type": "attachment_changed", "previousRevision": change.PreviousRevision,
			"revision": change.Revision, "entryId": attachment.EntryID,
			"previousRecordId":   attachment.PreviousRecordID,
			"attachmentRevision": attachment.AttachmentRevision, "attachment": attachment.Attachment,
		}
		if err := writeV2SSEEvent(w, "attachment_changed", change.Revision, payload); err != nil {
			return err
		}
	}
	for _, removed := range change.Removed {
		payload := map[string]any{
			"type": "entry_removed", "previousRevision": change.PreviousRevision,
			"revision": change.Revision, "entryId": removed.EntryID, "reason": removed.Reason,
		}
		if err := writeV2SSEEvent(w, "entry_removed", change.Revision, payload); err != nil {
			return err
		}
	}
	return nil
}

func serveV2HostStream(w http.ResponseWriter, r *http.Request, store *terminalEntryStore) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	if store.snapshot().Revision == "" {
		writeV2Error(w, http.StatusServiceUnavailable, "snapshot_unavailable", "initial snapshot is not ready", true)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeV2Error(w, http.StatusInternalServerError, "stream_unsupported", "streaming is unavailable", true)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	afterRevision := r.URL.Query().Get("afterRevision")
	if afterRevision == "" {
		afterRevision = r.Header.Get("Last-Event-ID")
	}
	changes, cancel := store.subscribe(afterRevision)
	defer cancel()
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case change := <-changes:
			if err := writeV2ChangeSet(w, change); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func handleV2HostStream(w http.ResponseWriter, r *http.Request) {
	serveV2HostStream(w, r, v2Entries)
}
