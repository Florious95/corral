package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func testV2Entry(id string) v2TerminalEntry {
	return v2TerminalEntry{
		EntryID: id, Kind: "claude", Cwd: "/work", State: "unknown", CanSend: true,
		LastActivityAt: "2026-07-15T00:00:00Z", Pane: v2Pane{PaneID: "%0", WindowName: "worker"},
	}
}

func testV2Record(id string) v2HistoryRecord {
	return v2HistoryRecord{
		RecordID: id, SessionID: id + "-session", Kind: "claude", Cwd: "/work",
		Title: id + " title", State: "waiting_input", Model: "claude-test",
		LastActivityAt: "2026-07-15T01:00:00Z", LastMessagePreview: id + " preview",
	}
}

func TestV2LivePaneWithoutAttachmentIsIdleNotUnknownOrGone(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	pane := paneBinding{Socket: "/tmp/live", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}
	inspection := weakTestInspection(started, pane)
	input, err := v2InputFromRecords(v2Host{ID: "host"}, nil, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, PaneID: pane.TmuxID, AgentPID: 11, StartSec: started.Unix()}, started, nil
	})
	if err != nil || len(input.Entries) != 1 {
		t.Fatalf("live placeholder input=%#v err=%v", input, err)
	}
	entry := input.Entries[0].Entry
	if entry.State != "idle" || !entry.CanSend {
		t.Fatalf("live pane without attachment must remain online: %#v", entry)
	}
}

func TestV2HistoryRecordUsesPreviewWhileEntryKeepsLastMessagePreview(t *testing.T) {
	recordJSON, err := json.Marshal(testV2Record("record"))
	if err != nil {
		t.Fatal(err)
	}
	var recordFields map[string]json.RawMessage
	if err := json.Unmarshal(recordJSON, &recordFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := recordFields["preview"]; !ok {
		t.Fatalf("HistoryRecord missing locked preview field: %s", recordJSON)
	}
	if _, ok := recordFields["lastMessagePreview"]; ok {
		t.Fatalf("HistoryRecord leaked TerminalEntry field name: %s", recordJSON)
	}
	entryJSON, err := json.Marshal(testV2Entry("entry"))
	if err != nil {
		t.Fatal(err)
	}
	var entryFields map[string]json.RawMessage
	if err := json.Unmarshal(entryJSON, &entryFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := entryFields["lastMessagePreview"]; !ok {
		t.Fatalf("TerminalEntry lost locked lastMessagePreview field: %s", entryJSON)
	}
}

func entryByID(t *testing.T, snapshot v2Snapshot, id string) v2TerminalEntry {
	t.Helper()
	for _, entry := range snapshot.Entries {
		if entry.EntryID == id {
			return entry
		}
	}
	t.Fatalf("entry %s missing from %#v", id, snapshot.Entries)
	return v2TerminalEntry{}
}

func TestV2EntryIDIsURLSafeAndUsesMicrosecondGeneration(t *testing.T) {
	identity := v2EntryIdentity{
		HostID: "host-alpha", SocketPath: "/private/tmp/tmux-501/corral-test",
		SocketDevice: 1, SocketInode: 2, PaneID: "%0", AgentPID: 42,
		StartSec: 100, StartUsec: 123456,
	}
	first := v2EntryID(identity)
	if !regexp.MustCompile(`^e1_[A-Za-z0-9_-]{22}$`).MatchString(first) {
		t.Fatalf("entryId is not the locked URL-safe shape: %q", first)
	}
	if again := v2EntryID(identity); again != first {
		t.Fatalf("same generation changed id: %q != %q", again, first)
	}
	identity.StartUsec++
	if next := v2EntryID(identity); next == first {
		t.Fatal("same-second process generations with different microseconds collided")
	}
}

func TestV2PaneIdentityUsesAgentGenerationInsteadOfLongLivedPaneShell(t *testing.T) {
	pane := paneBinding{Socket: "/private/tmp/tmux-501/qa", TmuxID: "%0", PanePID: 35278, ProcessPIDs: []int{35349}}
	identity := v2IdentityForPane(v2Host{ID: "host"}, pane, 1, 2, v2ProcessStarted{Sec: 100, Usec: 1})
	if identity.AgentPID != 35349 {
		t.Fatalf("identity agent pid=%d want=35349", identity.AgentPID)
	}
	oldID := v2EntryID(identity)
	pane.ProcessPIDs = []int{67128}
	next := v2IdentityForPane(v2Host{ID: "host"}, pane, 1, 2, v2ProcessStarted{Sec: 101, Usec: 2})
	if nextID := v2EntryID(next); nextID == oldID {
		t.Fatalf("same pane reused old entryId after agent replacement: %s", nextID)
	}
}

func TestTerminalEntryStoreAtomicallyAssignsUniqueAttachmentAndHistory(t *testing.T) {
	store := newTerminalEntryStore("testboot")
	record1, record2 := testV2Record("r1"), testV2Record("r2")
	snapshot := store.commit(v2SnapshotInput{
		Host: v2Host{ID: "host", Name: "Mac"},
		Entries: []v2EntryDraft{
			{Entry: testV2Entry("e1"), Record: &record1, EvidenceRank: 3},
			{Entry: testV2Entry("e2"), Record: &record1, EvidenceRank: 1},
			{Entry: testV2Entry("e3")},
		},
		History: []v2HistoryRecord{record1, record2},
	})
	if snapshot.Revision != "rv1_testboot_1" {
		t.Fatalf("revision=%q", snapshot.Revision)
	}
	if len(snapshot.History) != 1 || snapshot.History[0].RecordID != "r2" {
		t.Fatalf("attached record must be atomically excluded from history: %#v", snapshot.History)
	}
	winner := entryByID(t, snapshot, "e1")
	if winner.Attachment == nil || winner.Attachment.RecordID != "r1" || winner.AttachmentRevision != 1 || winner.Attachment.AttachmentRevision != 1 {
		t.Fatalf("winner attachment=%#v topRevision=%d", winner.Attachment, winner.AttachmentRevision)
	}
	if winner.LastActivityAt != record1.LastActivityAt || winner.LastMessagePreview != record1.LastMessagePreview || winner.Model != record1.Model || winner.State != record1.State {
		t.Fatalf("record display fields were not projected to entry: %#v", winner)
	}
	if loser := entryByID(t, snapshot, "e2"); loser.Attachment != nil || loser.AttachmentRevision != 0 || loser.LastMessagePreview != "" || loser.Model != "" {
		t.Fatalf("losing/unidentified entry leaked record projection: %#v", loser)
	}

	// A caller cannot mutate the committed snapshot through a returned value.
	snapshot.Entries[0].Cwd = "/mutated"
	if got := entryByID(t, store.snapshot(), "e1"); got.Cwd == "/mutated" {
		t.Fatal("snapshot commit was not isolated from caller mutation")
	}
}

func TestTerminalEntryStoreEqualRankConflictLeavesRecordInHistory(t *testing.T) {
	store := newTerminalEntryStore("testboot")
	record := testV2Record("contended")
	snapshot := store.commit(v2SnapshotInput{
		Host: v2Host{ID: "host"},
		Entries: []v2EntryDraft{
			{Entry: testV2Entry("e1"), Record: &record, EvidenceRank: 2},
			{Entry: testV2Entry("e2"), Record: &record, EvidenceRank: 2},
		},
		History: []v2HistoryRecord{record},
	})
	if entryByID(t, snapshot, "e1").Attachment != nil || entryByID(t, snapshot, "e2").Attachment != nil {
		t.Fatal("equal highest evidence must not attach the record to either entry")
	}
	if len(snapshot.History) != 1 || snapshot.History[0].RecordID != record.RecordID {
		t.Fatalf("contended record must remain in history: %#v", snapshot.History)
	}
}

func TestTerminalEntryStoreRevisionsAreStableAndAttachmentRevisionIsPerEntry(t *testing.T) {
	store := newTerminalEntryStore("testboot")
	record1, record2 := testV2Record("r1"), testV2Record("r2")
	input := v2SnapshotInput{
		Host:    v2Host{ID: "host"},
		Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record1, EvidenceRank: 3}},
		History: []v2HistoryRecord{record1, record2},
	}
	first := store.commit(input)
	if repeated := store.commit(input); repeated.Revision != first.Revision || entryByID(t, repeated, "e1").AttachmentRevision != 1 {
		t.Fatalf("identical commit changed revision: first=%#v repeated=%#v", first, repeated)
	}

	record1.LastActivityAt = "2026-07-15T02:00:00Z"
	input.Entries[0].Record = &record1
	activityOnly := store.commit(input)
	if activityOnly.Revision != "rv1_testboot_2" || entryByID(t, activityOnly, "e1").AttachmentRevision != 1 {
		t.Fatalf("display update must change snapshot but not attachment generation: %#v", activityOnly)
	}

	input.Entries[0].Record = &record2
	switched := store.commit(input)
	if switched.Revision != "rv1_testboot_3" || entryByID(t, switched, "e1").AttachmentRevision != 2 {
		t.Fatalf("record replacement must advance both revisions: %#v", switched)
	}

	input.Entries[0].Record = nil
	cleared := store.commit(input)
	entry := entryByID(t, cleared, "e1")
	if cleared.Revision != "rv1_testboot_4" || entry.Attachment != nil || entry.AttachmentRevision != 3 {
		t.Fatalf("attachment clear must retain a new top-level revision: %#v", cleared)
	}
}

func TestTerminalEntryStoreLookupDistinguishesGoneAndUnknown(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	store := newTerminalEntryStore("testboot")
	store.now = func() time.Time { return now }
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	if _, status := store.lookup("e1"); status != http.StatusOK {
		t.Fatalf("live lookup status=%d", status)
	}
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	if _, status := store.lookup("e1"); status != http.StatusGone {
		t.Fatalf("removed generation status=%d, want 410", status)
	}
	if _, status := store.lookup("never-seen"); status != http.StatusNotFound {
		t.Fatalf("unknown generation status=%d, want 404", status)
	}
	now = now.Add(v2RemovedEntryTTL + time.Second)
	if _, status := store.lookup("e1"); status != http.StatusNotFound {
		t.Fatalf("expired tombstone status=%d, want 404", status)
	}
}

func TestTerminalEntryStoreConcurrentCommitsNeverExposeAttachedRecordInHistory(t *testing.T) {
	store := newTerminalEntryStore("testboot")
	record1, record2 := testV2Record("r1"), testV2Record("r2")
	inputs := []v2SnapshotInput{
		{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record1, EvidenceRank: 3}}, History: []v2HistoryRecord{record1, record2}},
		{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e2"), Record: &record2, EvidenceRank: 3}}, History: []v2HistoryRecord{record1, record2}},
	}
	check := func(snapshot v2Snapshot) error {
		attached := map[string]bool{}
		for _, entry := range snapshot.Entries {
			if entry.Attachment != nil {
				attached[entry.Attachment.RecordID] = true
			}
		}
		for _, record := range snapshot.History {
			if attached[record.RecordID] {
				return errors.New("attached record leaked into history")
			}
		}
		return nil
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 300)
	for worker := 0; worker < 3; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for i := 0; i < 100; i++ {
				var snapshot v2Snapshot
				if worker < 2 {
					snapshot = store.commit(inputs[(worker+i)%len(inputs)])
				} else {
					snapshot = store.snapshot()
				}
				if err := check(snapshot); err != nil {
					errorsFound <- err
				}
			}
		}(worker)
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
}

func TestV2InputFromRecordsKeepsEveryPaneAndProjectsOnlyItsAttachment(t *testing.T) {
	started := time.Date(2026, 7, 15, 3, 0, 0, 123000000, time.UTC)
	attachedPane := paneBinding{Socket: "/tmp/tmux-a", TmuxID: "%0", PanePID: 10, Kind: "claude", WindowName: "leader", ProcessPIDs: []int{11}}
	unidentifiedPane := paneBinding{Socket: "/tmp/tmux-b", TmuxID: "%1", PanePID: 20, Kind: "codex", WindowName: "collector", ProcessPIDs: []int{21}}
	record := &sessionRecord{AgentSession: AgentSession{
		ID: "r1", Kind: "claude", Cwd: "/record-cwd", Title: "attached", SessionID: "session-1",
		SessionFile: "/record.jsonl", State: "waiting_input", Model: "model-1",
		LastActivityAt: "2026-07-15T04:00:00Z", LastMessagePreview: "preview-1",
	}, binding: &attachedPane, bindingEvidence: "strong"}
	historyOnly := &sessionRecord{AgentSession: AgentSession{
		ID: "r2", Kind: "claude", Cwd: "/history", Title: "history", SessionID: "session-2",
		SessionFile: "/history.jsonl", State: "gone", LastActivityAt: "2026-07-14T04:00:00Z",
	}}
	inspection := liveInspection{
		panes: []paneBinding{attachedPane, unidentifiedPane},
		files: processFiles{cwd: map[int]string{11: "/pane-cwd", 21: "/codex-cwd"}, valid: true},
		snap:  &procSnapshot{started: map[int]time.Time{11: started, 21: started}, valid: true},
		valid: true,
	}
	input, err := v2InputFromRecords(v2Host{ID: "host"}, []*sessionRecord{record, historyOnly}, inspection,
		func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
			return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, PaneID: pane.TmuxID, AgentPID: pane.ProcessPIDs[0]}, started, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Entries) != 2 || len(input.History) != 2 {
		t.Fatalf("input entries/history=%d/%d", len(input.Entries), len(input.History))
	}
	var attached, unidentified v2EntryDraft
	for _, draft := range input.Entries {
		switch draft.Entry.Pane.PaneID {
		case "%0":
			attached = draft
		case "%1":
			unidentified = draft
		}
	}
	if attached.Record == nil || attached.Record.RecordID != "r1" || attached.Entry.Cwd != "/pane-cwd" || attached.EvidenceRank != 3 {
		t.Fatalf("attached draft=%#v", attached)
	}
	if unidentified.Record != nil || unidentified.Entry.Cwd != "/codex-cwd" || !unidentified.Entry.CanSend || unidentified.Entry.LastActivityAt != started.Format(time.RFC3339Nano) {
		t.Fatalf("unidentified draft=%#v", unidentified)
	}
	snapshot := newTerminalEntryStore("testboot").commit(input)
	if len(snapshot.Entries) != 2 || len(snapshot.History) != 1 || snapshot.History[0].RecordID != "r2" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestV2InputFromRecordsRejectsPartialPaneIdentity(t *testing.T) {
	inspection := liveInspection{
		panes: []paneBinding{{Socket: "/tmp/tmux-a", TmuxID: "%0", PanePID: 10, Kind: "claude"}},
		files: processFiles{cwd: map[int]string{}, valid: true}, snap: &procSnapshot{valid: true}, valid: true,
	}
	_, err := v2InputFromRecords(v2Host{ID: "host"}, nil, inspection, func(_ v2Host, _ paneBinding) (v2EntryIdentity, time.Time, error) {
		return v2EntryIdentity{}, time.Time{}, errors.New("process exited")
	})
	if err == nil {
		t.Fatal("partial pane identity must fail the whole atomic scan")
	}
}

func TestV2PaneIdentityFailureBackoffAndRecovery(t *testing.T) {
	var logs lockedV2LogBuffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogOutput) })
	now := time.Date(2026, 7, 19, 3, 42, 0, 0, time.UTC)
	store := newTerminalEntryStore("pane-backoff")
	store.now = func() time.Time { return now }
	host := v2Host{ID: "host"}
	pane := paneBinding{Socket: "/tmp/ta-bad", TmuxID: "%29", ProcessPIDs: []int{129}}
	calls := 0
	fail := true
	identify := func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		calls++
		if fail {
			return v2EntryIdentity{}, time.Time{}, errors.New("input/output error")
		}
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, PaneID: pane.TmuxID, AgentPID: 129}, now, nil
	}
	if _, _, err := store.identifyPaneWithBackoff(host, pane, identify); err == nil || calls != 1 {
		t.Fatalf("first failure err=%v calls=%d", err, calls)
	}
	if _, _, err := store.identifyPaneWithBackoff(host, pane, identify); err == nil || calls != 1 {
		t.Fatalf("backoff did not suppress retry err=%v calls=%d", err, calls)
	}
	now = now.Add(v2PaneIdentityRetryBase)
	fail = false
	if identity, _, err := store.identifyPaneWithBackoff(host, pane, identify); err != nil || identity.PaneID != pane.TmuxID || calls != 2 {
		t.Fatalf("recovery identity=%#v err=%v calls=%d", identity, err, calls)
	}
	store.mu.Lock()
	remaining := len(store.paneIdentityFailures)
	store.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("recovered pane remains quarantined: %d", remaining)
	}
	for _, want := range []string{`socket="/tmp/ta-bad"`, "pane=%29", "result=degraded", "retry_after=2s", "input/output error", "result=recovered", "previous_failures=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("identity log missing %q: %s", want, logs.String())
		}
	}
}

func TestDegradedPanePreservesUniqueStickyAttachment(t *testing.T) {
	host := v2Host{ID: "host"}
	record := testV2Record("r1")
	bad := testV2Entry("bad")
	bad.logicalKey = "bad-logical"
	good := testV2Entry("good")
	good.logicalKey = "good-logical"
	store := newTerminalEntryStore("degraded-attachment")
	store.commit(v2SnapshotInput{Host: host, Entries: []v2EntryDraft{{Entry: bad, Record: &record, EvidenceRank: 3}}, History: []v2HistoryRecord{record}})

	snapshot := store.commit(v2SnapshotInput{
		Host:             host,
		Entries:          []v2EntryDraft{{Entry: good, Record: &record, EvidenceRank: 3}},
		History:          []v2HistoryRecord{record},
		degradedPaneKeys: map[string]struct{}{"bad-logical": {}},
	})
	if len(snapshot.Entries) != 2 || len(snapshot.History) != 0 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if entryByID(t, snapshot, "bad").Attachment == nil || entryByID(t, snapshot, "bad").Attachment.RecordID != record.RecordID {
		t.Fatalf("bad pane lost sticky attachment: %#v", snapshot.Entries)
	}
	if entryByID(t, snapshot, "good").Attachment != nil {
		t.Fatalf("degraded attachment duplicated onto good pane: %#v", snapshot.Entries)
	}
}

func TestServeV2SnapshotIsGETOnlyAndCommitsBeforeReply(t *testing.T) {
	store := newTerminalEntryStore("testboot")
	build := func() (v2SnapshotInput, error) {
		return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}}, nil
	}
	recorder := httptest.NewRecorder()
	serveV2Snapshot(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/snapshot", nil), store, build)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var snapshot v2Snapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != "rv1_testboot_1" || len(snapshot.Entries) != 1 {
		t.Fatalf("response=%#v", snapshot)
	}
	if snapshot.Entries == nil || snapshot.History == nil {
		t.Fatalf("snapshot collections must encode as arrays, not null: %#v", snapshot)
	}

	called := false
	recorder = httptest.NewRecorder()
	serveV2Snapshot(recorder, httptest.NewRequest(http.MethodPost, "/api/v2/snapshot", nil), store, func() (v2SnapshotInput, error) {
		called = true
		return v2SnapshotInput{}, errors.New("must not run")
	})
	if recorder.Code != http.StatusMethodNotAllowed || called {
		t.Fatalf("POST status=%d called=%v", recorder.Code, called)
	}
	var methodError v2ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &methodError); err != nil || methodError.Error.Code != "method_not_allowed" || methodError.Error.Retryable {
		t.Fatalf("POST error envelope=%#v decode=%v", methodError, err)
	}
}

func TestServeV2SnapshotPreservesLastAtomicSnapshotOnScanFailure(t *testing.T) {
	store := newTerminalEntryStore("testboot")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	recorder := httptest.NewRecorder()
	serveV2Snapshot(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/snapshot", nil), store, func() (v2SnapshotInput, error) {
		return v2SnapshotInput{}, errors.New("scan incomplete")
	})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope v2ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "snapshot_unavailable" || !envelope.Error.Retryable {
		t.Fatalf("scan error envelope=%#v decode=%v", envelope, err)
	}
	if snapshot := store.snapshot(); snapshot.Revision != "rv1_testboot_1" || len(snapshot.Entries) != 1 {
		t.Fatalf("failed scan replaced committed snapshot: %#v", snapshot)
	}
}
