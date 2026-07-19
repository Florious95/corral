package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeV2TimelineLoader(records map[string][]TimelineEvent) v2RecordTimelineLoader {
	return func(recordID string, query timelineQuery) (timelinePage, bool, error) {
		events, ok := records[recordID]
		if !ok {
			return timelinePage{}, false, nil
		}
		if query.HasBefore {
			events = filterEventsBefore(events, query.BeforeSeq)
		} else if query.HasAfter {
			events = filterEventsAfter(events, query.AfterSeq)
		}
		page := timelinePage{Events: append([]TimelineEvent(nil), events...)}
		if len(page.Events) > query.Limit {
			if query.HasAfter {
				page.Events = page.Events[:query.Limit]
				page.HasMoreAfter = true
			} else {
				page.Events = page.Events[len(page.Events)-query.Limit:]
				page.HasMoreBefore = true
			}
		}
		if page.HasMoreBefore && len(page.Events) > 0 {
			value := page.Events[0].Seq
			page.NextBeforeSeq = &value
		}
		return page, true, nil
	}
}

func TestV2EntryTimelineNoAttachmentAndGenerationStatuses(t *testing.T) {
	store := newTerminalEntryStore("routes")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	loader := fakeV2TimelineLoader(nil)

	recorder := httptest.NewRecorder()
	serveV2EntryTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/entries/e1/timeline", nil), store, "e1", loader)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unattached status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		EntryID            string          `json:"entryId"`
		AttachmentRevision uint64          `json:"attachmentRevision"`
		Attachment         *v2Attachment   `json:"attachment"`
		Events             []TimelineEvent `json:"events"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.EntryID != "e1" || response.Attachment != nil || response.AttachmentRevision != 0 || len(response.Events) != 0 || response.Events == nil {
		t.Fatalf("unattached response=%#v", response)
	}

	store.commitWithRemovalReason(v2SnapshotInput{Host: v2Host{ID: "host"}}, "process_exit")
	assertV2RouteError(t, store, loader, "e1", http.StatusGone, "entry_gone")
	assertV2RouteError(t, store, loader, "unknown", http.StatusNotFound, "entry_not_found")
}

func TestV2EntryTimelineReturnsRetryableNotReadyBeforeInitialSnapshot(t *testing.T) {
	store := newTerminalEntryStore("cold-start")
	recorder := httptest.NewRecorder()
	serveV2EntryTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/entries/e1/timeline", nil), store, "e1", fakeV2TimelineLoader(nil))
	assertV2ErrorEnvelope(t, recorder, http.StatusServiceUnavailable, "not_ready")
	var envelope v2ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Error.Retryable {
		t.Fatalf("cold-start envelope=%#v decode=%v", envelope, err)
	}

	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	store.commitWithRemovalReason(v2SnapshotInput{Host: v2Host{ID: "host"}}, "process_exit")
	assertV2RouteError(t, store, fakeV2TimelineLoader(nil), "e1", http.StatusGone, "entry_gone")
}

func TestV2EntryAndRecordTimelinesUseCurrentAttachmentCursorDomain(t *testing.T) {
	store := newTerminalEntryStore("timeline")
	record := testV2Record("r1")
	store.commit(v2SnapshotInput{
		Host:    v2Host{ID: "host"},
		Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record, EvidenceRank: 3}},
		History: []v2HistoryRecord{record},
	})
	loader := fakeV2TimelineLoader(map[string][]TimelineEvent{
		"r1": {
			{Seq: 1001, Type: "user_message", Text: "one"},
			{Seq: 2001, Type: "assistant_message", Text: "two"},
			{Seq: 3001, Type: "user_message", Text: "three"},
		},
	})

	recorder := httptest.NewRecorder()
	serveV2EntryTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/entries/e1/timeline?afterSeq=1001&limit=1", nil), store, "e1", loader)
	var entryResponse struct {
		RecordID           string          `json:"recordId"`
		AttachmentRevision uint64          `json:"attachmentRevision"`
		Events             []TimelineEvent `json:"events"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &entryResponse) != nil {
		t.Fatalf("entry timeline status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if entryResponse.RecordID != "r1" || entryResponse.AttachmentRevision != 1 || len(entryResponse.Events) != 1 || entryResponse.Events[0].Seq != 2001 {
		t.Fatalf("entry timeline=%#v", entryResponse)
	}

	recorder = httptest.NewRecorder()
	serveV2RecordTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/records/r1/timeline?afterSeq=2001", nil), "r1", loader)
	var recordResponse struct {
		RecordID string          `json:"recordId"`
		Events   []TimelineEvent `json:"events"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &recordResponse) != nil {
		t.Fatalf("record timeline status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recordResponse.RecordID != "r1" || len(recordResponse.Events) != 1 || recordResponse.Events[0].Seq != 3001 {
		t.Fatalf("record timeline=%#v", recordResponse)
	}

	recorder = httptest.NewRecorder()
	serveV2RecordTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/records/r1/timeline?beforeSeq=3001&limit=1", nil), "r1", loader)
	var beforeResponse struct {
		Events        []TimelineEvent `json:"events"`
		HasMoreBefore bool            `json:"hasMoreBefore"`
		NextBeforeSeq *int64          `json:"nextBeforeSeq"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &beforeResponse) != nil {
		t.Fatalf("before timeline status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(beforeResponse.Events) != 1 || beforeResponse.Events[0].Seq != 2001 || !beforeResponse.HasMoreBefore || beforeResponse.NextBeforeSeq == nil || *beforeResponse.NextBeforeSeq != 2001 {
		t.Fatalf("before timeline=%#v", beforeResponse)
	}

	recorder = httptest.NewRecorder()
	serveV2RecordTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/records/r1/timeline?afterSeq=1&beforeSeq=2", nil), "r1", loader)
	assertV2ErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_query")

	recorder = httptest.NewRecorder()
	serveV2RecordTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/records/missing/timeline", nil), "missing", loader)
	assertV2ErrorEnvelope(t, recorder, http.StatusNotFound, "record_not_found")
}

func TestV2EntryStreamPublishesTimelineAttachmentAndTerminalRemovalWithoutPolling(t *testing.T) {
	store := newTerminalEntryStore("entry-stream")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	loader := fakeV2TimelineLoader(map[string][]TimelineEvent{
		"r1": {{Seq: 2001, Type: "assistant_message", Text: "increment"}},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2EntryStream(w, r, store, "e1", loader)
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "?afterSeq=1001")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	state := readV2SSEEventFromReader(t, reader, "state")
	if state.Data["entryId"] != "e1" {
		t.Fatalf("initial state=%#v", state)
	}
	record := testV2Record("r1")
	attached := store.commit(v2SnapshotInput{
		Host:    v2Host{ID: "host"},
		Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record, EvidenceRank: 3}},
		History: []v2HistoryRecord{record},
	})
	attachment := readV2SSEEventFromReader(t, reader, "attachment_changed")
	if attachment.ID != attached.Revision || attachment.Data["entryId"] != "e1" || attachment.Data["attachmentRevision"] != float64(1) {
		t.Fatalf("attachment event=%#v", attachment)
	}
	removed := store.commitWithRemovalReason(v2SnapshotInput{Host: v2Host{ID: "host"}}, "process_exit")
	terminal := readV2SSEEventFromReader(t, reader, "entry_removed")
	_ = response.Body.Close()
	if terminal.ID != removed.Revision || terminal.Data["reason"] != "process_exit" {
		t.Fatalf("terminal event=%#v", terminal)
	}
}

func TestV2EntryStreamUnrelatedStoreChangeDoesNotReloadSameTimelineVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"2026-07-18T00:00:00Z","message":{"role":"user","content":"one"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newTerminalEntryStore("entry-stream-version")
	entry := testV2Entry("e1")
	entry.runtime = &v2EntryRuntime{}
	bound := &sessionRecord{AgentSession: AgentSession{ID: "r1", SessionID: "r1-session", Kind: "claude", SessionFile: path, State: "waiting_input", Live: true, CanSend: true}}
	record := testV2Record("r1")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: entry, Record: &record, BoundRecord: bound, EvidenceRank: 3}}})
	var loads atomic.Int32
	loader := func(string, timelineQuery) (timelinePage, bool, error) {
		loads.Add(1)
		return timelinePage{Events: []TimelineEvent{{Seq: 1001, Type: "user_message", Text: "one"}}}, true, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2EntryStream(w, r, store, "e1", loader)
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	_ = readV2SSEEventFromReader(t, reader, "state")
	deadline := time.Now().Add(time.Second)
	for loads.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	record.State = "running"
	bound.State = "running"
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: entry, Record: &record, BoundRecord: bound, EvidenceRank: 3}}})
	_ = readV2SSEEventFromReader(t, reader, "state")
	time.Sleep(20 * time.Millisecond)
	_ = response.Body.Close()
	if got := loads.Load(); got != 1 {
		t.Fatalf("same file version timeline loads=%d want=1", got)
	}
}

func TestV2EntryStreamHundredSubscribersShareOnePhysicalTimelineParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"2026-07-18T00:00:00Z","message":{"role":"user","content":"one"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetTimelineCacheForTest(t)
	var parses atomic.Int32
	timelineParseHook = func(string) { parses.Add(1) }
	t.Cleanup(func() { timelineParseHook = nil })
	store := newTerminalEntryStore("entry-stream-100")
	entry := testV2Entry("e1")
	entry.runtime = &v2EntryRuntime{}
	bound := &sessionRecord{AgentSession: AgentSession{ID: "r1", SessionID: "r1-session", Kind: "claude", SessionFile: path, State: "waiting_input", Live: true, CanSend: true}}
	record := testV2Record("r1")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: entry, Record: &record, BoundRecord: bound, EvidenceRank: 3}}})
	var loads atomic.Int32
	loader := func(_ string, query timelineQuery) (timelinePage, bool, error) {
		page, err := timelinePageFor(bound, query)
		loads.Add(1)
		return page, true, err
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2EntryStream(w, r, store, "e1", loader)
	}))
	defer server.Close()
	responses := make(chan *http.Response, 100)
	errors := make(chan error, 100)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := server.Client().Get(server.URL)
			if err != nil {
				errors <- err
				return
			}
			responses <- response
		}()
	}
	wg.Wait()
	close(responses)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for loads.Load() != 100 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for response := range responses {
		_ = response.Body.Close()
	}
	if got := loads.Load(); got != 100 {
		t.Fatalf("timeline loader calls=%d want=100", got)
	}
	if got := parses.Load(); got != 1 {
		t.Fatalf("100 entry streams physical timeline parses=%d want=1", got)
	}
}

func TestV2UnknownAPIRouteCannotFallThroughToSPA(t *testing.T) {
	store := newTerminalEntryStore("mux")
	mux := http.NewServeMux()
	registerV2Routes(mux, store, fakeV2TimelineLoader(nil))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("SPA MUST NOT LEAK"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", spaHandler(dir))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/not-a-route", nil))
	assertV2ErrorEnvelope(t, recorder, http.StatusNotFound, "route_not_found")
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q body=%s", got, recorder.Body.String())
	}
}

func assertV2RouteError(t *testing.T, store *terminalEntryStore, loader v2RecordTimelineLoader, entryID string, status int, code string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	serveV2EntryTimeline(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/entries/"+entryID+"/timeline", nil), store, entryID, loader)
	assertV2ErrorEnvelope(t, recorder, status, code)
}

func assertV2ErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, status, recorder.Body.String())
	}
	var envelope v2ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != code {
		t.Fatalf("envelope=%#v decode=%v body=%s", envelope, err, recorder.Body.String())
	}
}

func TestV2EntryStreamUnknownAndGoneReturnJSONBeforeSSEHeaders(t *testing.T) {
	store := newTerminalEntryStore("stream-status")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	for _, test := range []struct {
		id     string
		status int
		code   string
	}{{"e1", http.StatusGone, "entry_gone"}, {"missing", http.StatusNotFound, "entry_not_found"}} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v2/entries/"+test.id+"/stream", nil)
		ctx, cancel := context.WithTimeout(request.Context(), 100*time.Millisecond)
		serveV2EntryStream(recorder, request.WithContext(ctx), store, test.id, fakeV2TimelineLoader(nil))
		cancel()
		assertV2ErrorEnvelope(t, recorder, test.status, test.code)
	}
}
