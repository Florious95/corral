package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type v2RecordTimelineLoader func(recordID string, query timelineQuery) (timelinePage, bool, error)

func loadV2RecordTimeline(recordID string, query timelineQuery) (timelinePage, bool, error) {
	record, _, _ := v2Entries.lookupV1WriteRecord(recordID)
	if record == nil || record.SessionFile == "" {
		record = findSession(recordID, false)
	}
	if record == nil {
		return timelinePage{}, false, nil
	}
	page, err := timelinePageFor(record, query)
	return page, true, err
}

func v2RecordTimelineVersion(store *terminalEntryStore, recordID string) (timelineFileVersion, bool) {
	record, _, _ := store.lookupV1WriteRecord(recordID)
	if record == nil || record.SessionFile == "" {
		return timelineFileVersion{}, false
	}
	version, err := timelineVersionForPath(record.SessionFile)
	return version, err == nil
}

func registerV2Routes(mux *http.ServeMux, store *terminalEntryStore, load v2RecordTimelineLoader) {
	writes := newV2WriteService(store, v2WriteDependencies{})
	mux.HandleFunc("/api/v2/snapshot", withCORS(func(w http.ResponseWriter, r *http.Request) {
		serveV2SnapshotCurrent(w, r, store)
	}))
	mux.HandleFunc("/api/v2/stream", withCORS(func(w http.ResponseWriter, r *http.Request) {
		serveV2HostStream(w, r, store)
	}))
	mux.HandleFunc("/api/v2/entries/", withCORS(func(w http.ResponseWriter, r *http.Request) {
		serveV2EntryRoute(w, r, store, load, writes)
	}))
	mux.HandleFunc("/api/v2/records/", withCORS(func(w http.ResponseWriter, r *http.Request) {
		serveV2RecordRoute(w, r, load)
	}))
	notFound := withCORS(func(w http.ResponseWriter, _ *http.Request) {
		writeV2Error(w, http.StatusNotFound, "route_not_found", "v2 API route not found", false)
	})
	mux.HandleFunc("/api/v2", notFound)
	mux.HandleFunc("/api/v2/", notFound)
}

func serveV2EntryRoute(w http.ResponseWriter, r *http.Request, store *terminalEntryStore, load v2RecordTimelineLoader, writes *v2WriteService) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v2/entries/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		writeV2Error(w, http.StatusNotFound, "route_not_found", "v2 API route not found", false)
		return
	}
	switch parts[1] {
	case "timeline":
		serveV2EntryTimeline(w, r, store, parts[0], load)
	case "stream":
		serveV2EntryStream(w, r, store, parts[0], load)
	case "screen":
		serveV2EntryScreen(w, r, writes, parts[0])
	case "send", "upload", "choose", "kill", "keys":
		serveV2EntryWrite(w, r, writes, parts[0], parts[1])
	default:
		writeV2Error(w, http.StatusNotFound, "route_not_found", "v2 API route not found", false)
	}
}

func serveV2RecordRoute(w http.ResponseWriter, r *http.Request, load v2RecordTimelineLoader) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v2/records/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "timeline" {
		writeV2Error(w, http.StatusNotFound, "route_not_found", "v2 API route not found", false)
		return
	}
	serveV2RecordTimeline(w, r, parts[0], load)
}

func serveV2EntryTimeline(w http.ResponseWriter, r *http.Request, store *terminalEntryStore, entryID string, load v2RecordTimelineLoader) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	entry, status := store.lookup(entryID)
	if status != http.StatusOK {
		writeV2EntryLookupError(w, status)
		return
	}
	query, err := parseTimelineQuery(r)
	if err != nil {
		writeV2Error(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	response := struct {
		EntryID            string          `json:"entryId"`
		RecordID           string          `json:"recordId,omitempty"`
		AttachmentRevision uint64          `json:"attachmentRevision"`
		Attachment         *v2Attachment   `json:"attachment"`
		Events             []TimelineEvent `json:"events"`
		HasMoreBefore      bool            `json:"hasMoreBefore"`
		NextBeforeSeq      *int64          `json:"nextBeforeSeq"`
	}{EntryID: entry.EntryID, AttachmentRevision: entry.AttachmentRevision, Attachment: cloneV2Attachment(entry.Attachment), Events: []TimelineEvent{}}
	if entry.Attachment != nil {
		response.RecordID = entry.Attachment.RecordID
		page, exists, loadErr := load(entry.Attachment.RecordID, query)
		if loadErr != nil {
			writeV2Error(w, http.StatusServiceUnavailable, "timeline_unavailable", loadErr.Error(), true)
			return
		}
		if !exists {
			writeV2Error(w, http.StatusNotFound, "record_not_found", "attached record not found", false)
			return
		}
		response.Events = page.Events
		response.HasMoreBefore = page.HasMoreBefore
		response.NextBeforeSeq = page.NextBeforeSeq
		if response.Events == nil {
			response.Events = []TimelineEvent{}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func serveV2RecordTimeline(w http.ResponseWriter, r *http.Request, recordID string, load v2RecordTimelineLoader) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	query, err := parseTimelineQuery(r)
	if err != nil {
		writeV2Error(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	page, exists, loadErr := load(recordID, query)
	if loadErr != nil {
		writeV2Error(w, http.StatusServiceUnavailable, "timeline_unavailable", loadErr.Error(), true)
		return
	}
	if !exists {
		writeV2Error(w, http.StatusNotFound, "record_not_found", "record not found", false)
		return
	}
	if page.Events == nil {
		page.Events = []TimelineEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recordId": recordID, "events": page.Events,
		"hasMoreBefore": page.HasMoreBefore, "nextBeforeSeq": page.NextBeforeSeq,
	})
}

func serveV2EntryStream(w http.ResponseWriter, r *http.Request, store *terminalEntryStore, entryID string, load v2RecordTimelineLoader) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	query, err := parseTimelineQuery(r)
	if err != nil {
		writeV2Error(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	if query.HasBefore {
		writeV2Error(w, http.StatusBadRequest, "invalid_query", "beforeSeq is not valid for a stream", false)
		return
	}
	after := query.AfterSeq
	entry, status, revision := store.lookupCurrent(entryID)
	if status != http.StatusOK {
		writeV2EntryLookupError(w, status)
		return
	}
	resumeRevision := r.URL.Query().Get("afterRevision")
	if resumeRevision == "" {
		resumeRevision = r.Header.Get("Last-Event-ID")
	}
	if resumeRevision != "" && resumeRevision != revision {
		payload := map[string]any{"type": "snapshot_required", "previousRevision": resumeRevision, "revision": revision}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_ = writeV2SSEEvent(w, "snapshot_required", revision, payload)
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
	changes, cancel := store.subscribe(revision)
	defer cancel()
	deliveries, cancelDeliveries := store.subscribeDeliveries(entryID)
	defer cancelDeliveries()
	if writeV2EntryState(w, revision, entry) != nil {
		return
	}
	// Establish the SSE response before parsing a potentially large attached
	// record. Clients can render the entry state while timeline catch-up runs.
	flusher.Flush()
	recordID := ""
	var recordVersion timelineFileVersion
	hasRecordVersion := false
	if entry.Attachment != nil {
		recordID = entry.Attachment.RecordID
		if query.HasAfter {
			if !writeV2TimelineAfterPages(w, revision, recordID, load, &after) {
				return
			}
		} else if page, exists, loadErr := load(recordID, timelineQuery{Limit: 1}); loadErr == nil && exists && len(page.Events) > 0 {
			after = page.Events[len(page.Events)-1].Seq
		}
		recordVersion, hasRecordVersion = v2RecordTimelineVersion(store, recordID)
	}
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case delivery := <-deliveries:
			_, _, currentRevision := store.lookupCurrent(entryID)
			if err := writeV2SSEEvent(w, "delivery", currentRevision, delivery); err != nil {
				logV2DeliverySSE(delivery, time.Now(), "error")
				return
			}
			flusher.Flush()
			logV2DeliverySSE(delivery, time.Now(), "ok")
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case change := <-changes:
			if change.SnapshotRequired {
				payload := map[string]any{"type": "snapshot_required", "previousRevision": change.PreviousRevision, "revision": change.Revision}
				_ = writeV2SSEEvent(w, "snapshot_required", change.Revision, payload)
				flusher.Flush()
				return
			}
			if removed, ok := removedV2Entry(change, entryID); ok {
				payload := map[string]any{"type": "entry_removed", "previousRevision": change.PreviousRevision, "revision": change.Revision, "entryId": entryID, "reason": removed.Reason}
				_ = writeV2SSEEvent(w, "entry_removed", change.Revision, payload)
				flusher.Flush()
				return
			}
			current, currentStatus, currentRevision := store.lookupCurrent(entryID)
			if currentStatus != http.StatusOK {
				continue
			}
			if attachment, ok := changedV2Attachment(change, entryID); ok {
				payload := map[string]any{"type": "attachment_changed", "previousRevision": change.PreviousRevision, "revision": change.Revision, "entryId": entryID, "previousRecordId": attachment.PreviousRecordID, "attachmentRevision": attachment.AttachmentRevision, "attachment": attachment.Attachment}
				if writeV2SSEEvent(w, "attachment_changed", change.Revision, payload) != nil {
					return
				}
				recordID = ""
				hasRecordVersion = false
				if current.Attachment != nil {
					recordID = current.Attachment.RecordID
					if page, exists, loadErr := load(recordID, timelineQuery{Limit: 1}); loadErr == nil && exists && len(page.Events) > 0 {
						after = page.Events[len(page.Events)-1].Seq
					} else {
						after = 0
					}
					recordVersion, hasRecordVersion = v2RecordTimelineVersion(store, recordID)
				} else {
					after = 0
				}
			}
			if current.State != entry.State || current.CanSend != entry.CanSend {
				if writeV2EntryState(w, currentRevision, current) != nil {
					return
				}
			}
			if recordID != "" && current.Attachment != nil && current.Attachment.RecordID == recordID {
				currentVersion, versionOK := v2RecordTimelineVersion(store, recordID)
				if versionOK && (!hasRecordVersion || currentVersion != recordVersion) {
					if !writeV2TimelineAfterPages(w, currentRevision, recordID, load, &after) {
						return
					} else {
						recordVersion, hasRecordVersion = currentVersion, true
					}
				}
			}
			entry = current
			flusher.Flush()
		}
	}
}

func writeV2TimelineAfterPages(w http.ResponseWriter, revision, recordID string, load v2RecordTimelineLoader, after *int64) bool {
	for {
		page, exists, err := load(recordID, timelineQuery{Limit: 1000, HasAfter: true, AfterSeq: *after})
		if err != nil || !exists {
			return true
		}
		if !writeV2TimelineAfter(w, revision, page.Events, after) {
			return false
		}
		if !page.HasMoreAfter || len(page.Events) == 0 {
			return true
		}
	}
}

func writeV2EntryLookupError(w http.ResponseWriter, status int) {
	if status == http.StatusServiceUnavailable {
		writeV2Error(w, status, "not_ready", "initial entry index is not ready", true)
		return
	}
	if status == http.StatusGone {
		writeV2Error(w, status, "entry_gone", "entry generation has exited", false)
		return
	}
	writeV2Error(w, http.StatusNotFound, "entry_not_found", "entry not found", false)
}

func writeV2EntryState(w http.ResponseWriter, revision string, entry v2TerminalEntry) error {
	payload := map[string]any{"type": "state", "revision": revision, "entryId": entry.EntryID, "state": entry.State, "canSend": entry.CanSend}
	return writeV2SSEEvent(w, "state", revision, payload)
}

func writeV2TimelineAfter(w http.ResponseWriter, revision string, events []TimelineEvent, after *int64) bool {
	for _, event := range events {
		if event.Seq <= *after {
			continue
		}
		if writeV2SSEEvent(w, "timeline", revision, event) != nil {
			return false
		}
		*after = event.Seq
	}
	return true
}

func changedV2Attachment(change v2ChangeSet, entryID string) (v2AttachmentChanged, bool) {
	for _, attachment := range change.Attachments {
		if attachment.EntryID == entryID {
			return attachment, true
		}
	}
	return v2AttachmentChanged{}, false
}

func removedV2Entry(change v2ChangeSet, entryID string) (v2EntryRemoved, bool) {
	for _, removed := range change.Removed {
		if removed.EntryID == entryID {
			return removed, true
		}
	}
	return v2EntryRemoved{}, false
}
