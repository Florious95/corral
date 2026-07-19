package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedV2LogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *lockedV2LogBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(value)
}

func (buffer *lockedV2LogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}

func testV2StickyVerifyFixture() (v2TerminalEntry, v2EntryVerifyDependencies) {
	want := v2EntryIdentity{
		HostID: "host", SocketPath: "/private/tmp/test-tmux", SocketDevice: 11, SocketInode: 22,
		PaneID: "%0", AgentPID: 100, StartSec: 200, StartUsec: 300,
	}
	entry := testV2Entry(v2EntryID(want))
	entry.Kind = "claude"
	entry.runtime = &v2EntryRuntime{
		Identity: want,
		Binding: paneBinding{
			Socket: want.SocketPath, TmuxID: want.PaneID, PanePID: 42, Kind: "claude", ProcessPIDs: []int{want.AgentPID},
		},
	}
	processes := map[int]v2ProcessIdentity{
		100: {Started: v2ProcessStarted{Sec: 200, Usec: 300}, ParentPID: 80},
		80:  {ParentPID: 42},
		42:  {ParentPID: 1},
	}
	dependencies := v2EntryVerifyDependencies{
		SocketIdentity: func(string) (uint64, uint64, error) { return 11, 22, nil },
		ListPanes:      func(string) ([]byte, error) { return []byte("%0\t42\twindow\n"), nil },
		ProcessAlive:   func(pid int) bool { return pid == 100 || pid == 42 },
		ProcessIdentity: func(pid int) (v2ProcessIdentity, error) {
			identity, ok := processes[pid]
			if !ok {
				return v2ProcessIdentity{}, errors.New("missing process")
			}
			return identity, nil
		},
	}
	return entry, dependencies
}

func assertV2VerifyGone(t *testing.T, entry v2TerminalEntry, dependencies v2EntryVerifyDependencies) {
	t.Helper()
	if binding, writeErr := verifyV2EntryWithDependencies(entry, dependencies); binding != nil || writeErr == nil || writeErr.Code != "entry_gone" {
		t.Fatalf("binding=%#v error=%#v", binding, writeErr)
	}
}

func TestV2VerifyStickyIdentityDoesNotRequireGlobalProcessSnapshot(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	binding, writeErr := verifyV2EntryWithDependencies(entry, dependencies)
	if writeErr != nil || binding == nil || binding.PanePID != 42 || len(binding.ProcessPIDs) != 1 || binding.ProcessPIDs[0] != 100 {
		t.Fatalf("binding=%#v error=%#v", binding, writeErr)
	}
}

func TestV2VerifyStickyIdentityRejectsDeadProcess(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.ProcessAlive = func(int) bool { return false }
	assertV2VerifyGone(t, entry, dependencies)
}

func TestV2VerifyStickyIdentityRejectsStartTimeMismatch(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.ProcessIdentity = func(pid int) (v2ProcessIdentity, error) {
		return v2ProcessIdentity{Started: v2ProcessStarted{Sec: 200, Usec: 301}, ParentPID: 42}, nil
	}
	assertV2VerifyGone(t, entry, dependencies)
}

func TestV2VerifyStickyIdentityRejectsPanePIDMismatch(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.ListPanes = func(string) ([]byte, error) { return []byte("%0\t43\twindow\n"), nil }
	assertV2VerifyGone(t, entry, dependencies)
}

func TestV2VerifyStickyIdentityRejectsSocketIdentityMismatch(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.SocketIdentity = func(string) (uint64, uint64, error) { return 11, 23, nil }
	assertV2VerifyGone(t, entry, dependencies)
}

func TestV2VerifyStickyIdentityRejectsContradictoryParentChain(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.ProcessIdentity = func(pid int) (v2ProcessIdentity, error) {
		switch pid {
		case 100:
			return v2ProcessIdentity{Started: v2ProcessStarted{Sec: 200, Usec: 300}, ParentPID: 80}, nil
		case 80:
			return v2ProcessIdentity{ParentPID: 1}, nil
		default:
			return v2ProcessIdentity{}, errors.New("missing process")
		}
	}
	assertV2VerifyGone(t, entry, dependencies)
}

func TestV2VerifyStickyIdentityAcceptsAgentAsPaneRoot(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	entry.runtime.Binding.PanePID = entry.runtime.Identity.AgentPID
	dependencies.ListPanes = func(string) ([]byte, error) { return []byte("%0\t100\twindow\n"), nil }
	dependencies.ProcessIdentity = func(int) (v2ProcessIdentity, error) {
		return v2ProcessIdentity{Started: v2ProcessStarted{Sec: 200, Usec: 300}, ParentPID: 1}, nil
	}
	if binding, writeErr := verifyV2EntryWithDependencies(entry, dependencies); writeErr != nil || binding == nil {
		t.Fatalf("direct pane agent binding=%#v error=%#v", binding, writeErr)
	}
}

func TestV2VerifyStickyIdentityFallsBackOnlyWhenTargetIsAlive(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.ProcessIdentity = func(int) (v2ProcessIdentity, error) {
		return v2ProcessIdentity{}, errors.New("temporarily unavailable")
	}
	if binding, writeErr := verifyV2EntryWithDependencies(entry, dependencies); writeErr != nil || binding == nil {
		t.Fatalf("alive sticky fallback binding=%#v error=%#v", binding, writeErr)
	}
	dependencies.ProcessAlive = func(int) bool { return false }
	assertV2VerifyGone(t, entry, dependencies)
}

func TestV2VerifyStickyIdentityRetriesPaneListingThenFallsBackWithDiagnostics(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	var calls int
	dependencies.ListPanes = func(string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("first transient output"), errors.New("first transient error")
		}
		return []byte("second transient output"), errors.New("second transient error")
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	trace := &v2DeliveryTrace{DeliveryID: "d-test", EntryID: entry.EntryID, Started: time.Now()}
	binding, writeErr := verifyV2EntryWithDependenciesTrace(entry, dependencies, trace)
	if writeErr != nil || binding == nil || binding.PanePID != 42 || calls != 2 {
		t.Fatalf("binding=%#v error=%#v calls=%d", binding, writeErr, calls)
	}
	for _, want := range []string{"delivery=d-test entry=" + entry.EntryID, "phase=verify_list_panes", "start_us=", "end_us=", "duration_ms=", "attempt=1", "first transient error", "first transient output", "attempt=2", "second transient error", "second transient output", "using sticky pane identity"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing diagnostic %q in logs:\n%s", want, logs.String())
		}
	}
}

func TestV2VerifyStickyIdentityRetryStillRejectsExplicitPaneMismatch(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	var calls int
	dependencies.ListPanes = func(string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("temporary"), errors.New("temporary failure")
		}
		return []byte("%0\t43\twindow\n"), nil
	}
	assertV2VerifyGone(t, entry, dependencies)
	if calls != 2 {
		t.Fatalf("list-panes calls=%d, want 2", calls)
	}
}

func TestV2VerifyStickyIdentityDoesNotFallBackWhenStickyPaneIsDead(t *testing.T) {
	entry, dependencies := testV2StickyVerifyFixture()
	dependencies.ListPanes = func(string) ([]byte, error) {
		return []byte("temporary"), errors.New("temporary failure")
	}
	dependencies.ProcessAlive = func(pid int) bool { return pid == entry.runtime.Identity.AgentPID }
	assertV2VerifyGone(t, entry, dependencies)
}

func testV2WriteService(t *testing.T, attached bool, reconcile bool) (*terminalEntryStore, *v2WriteService, *atomic.Int32) {
	t.Helper()
	store := newTerminalEntryStore("writes")
	entry := testV2Entry("e1")
	entry.Kind = "claude"
	draft := v2EntryDraft{Entry: entry}
	if attached {
		record := testV2Record("r1")
		draft.Record = &record
		draft.EvidenceRank = 3
	}
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{draft}})
	var sends atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{
		Verify: func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return &paneBinding{Socket: "/private/tmp/test", TmuxID: "%1", Kind: "claude", ProcessPIDs: []int{123}}, nil
		},
		Send: func(_ *paneBinding, _ string) error { sends.Add(1); return nil },
		PrepareReconcile: func(v2TerminalEntry, string) func() bool {
			return func() bool { return reconcile }
		},
	})
	return store, service, &sends
}

func v2JSONRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func decodeV2WriteResponse(t *testing.T, recorder *httptest.ResponseRecorder) v2WriteResponse {
	t.Helper()
	var response v2WriteResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
	return response
}

func TestV2SendIsIdempotentAndPublishesAcceptedThenEchoed(t *testing.T) {
	store, service, sends := testV2WriteService(t, true, true)
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()
	requestBody := map[string]any{"clientNonce": "nonce-1", "text": "hello"}

	first := httptest.NewRecorder()
	serveV2EntryWrite(first, v2JSONRequest(t, "/api/v2/entries/e1/send", requestBody), service, "e1", "send")
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	firstResponse := decodeV2WriteResponse(t, first)
	if firstResponse.Status != "accepted" || firstResponse.EntryID != "e1" || firstResponse.ClientNonce != "nonce-1" || firstResponse.DeliveryID == "" {
		t.Fatalf("first response=%#v", firstResponse)
	}
	accepted := <-deliveries
	if accepted.Status != "accepted" || accepted.DeliveryID != firstResponse.DeliveryID {
		t.Fatalf("accepted=%#v", accepted)
	}
	select {
	case echoed := <-deliveries:
		if echoed.Status != "echoed" || echoed.DeliveryID != firstResponse.DeliveryID {
			t.Fatalf("echoed=%#v", echoed)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for echoed delivery")
	}

	retry := httptest.NewRecorder()
	serveV2EntryWrite(retry, v2JSONRequest(t, "/api/v2/entries/e1/send", requestBody), service, "e1", "send")
	retryResponse := decodeV2WriteResponse(t, retry)
	if retry.Code != http.StatusOK || retryResponse.DeliveryID != firstResponse.DeliveryID || sends.Load() != 1 {
		t.Fatalf("retry status=%d response=%#v sends=%d", retry.Code, retryResponse, sends.Load())
	}
}

func TestV2SendTimingLogsOneDeliveryAcrossWriteAndEcho(t *testing.T) {
	store, service, _ := testV2WriteService(t, true, true)
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()
	var logs bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	recorder := httptest.NewRecorder()
	serveV2EntryWrite(recorder, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"clientNonce": "timing", "text": "hello"}), service, "e1", "send")
	response := decodeV2WriteResponse(t, recorder)
	if recorder.Code != http.StatusOK || response.DeliveryID == "" {
		t.Fatalf("status=%d response=%#v", recorder.Code, response)
	}
	for range 2 {
		select {
		case <-deliveries:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for accepted and echoed")
		}
	}

	for _, phase := range []string{
		"post_enter", "verify_done", "send_keys_done", "delivery_status status=accepted",
		"response_ready", "echo_probe_start", "echo_probe_done", "jsonl_confirmed",
		"delivery_status status=echoed",
	} {
		want := "delivery=" + response.DeliveryID + " entry=e1 phase=" + phase
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing timing %q in logs:\n%s", want, logs.String())
		}
	}
	for _, want := range []string{"start_us=", "end_us=", "duration_ms=", "total_ms="} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing timing field %q in logs:\n%s", want, logs.String())
		}
	}
}

func TestV2ConcurrentRetryExecutesSendOnce(t *testing.T) {
	_, service, sends := testV2WriteService(t, false, true)
	const workers = 12
	responses := make(chan v2WriteOutcome, workers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			responses <- service.send("e1", "concurrent", "hello")
		}()
	}
	close(start)
	wait.Wait()
	close(responses)
	deliveryID := ""
	for outcome := range responses {
		if outcome.Error != nil || outcome.Response.DeliveryID == "" {
			t.Fatalf("outcome=%#v", outcome)
		}
		if deliveryID == "" {
			deliveryID = outcome.Response.DeliveryID
		} else if outcome.Response.DeliveryID != deliveryID {
			t.Fatalf("delivery ids differ: %q vs %q", outcome.Response.DeliveryID, deliveryID)
		}
	}
	if sends.Load() != 1 {
		t.Fatalf("concurrent retries injected %d times", sends.Load())
	}
}

func TestV2SendUnattributedKeepsEntryWritableWithoutFullReevaluation(t *testing.T) {
	store, service, _ := testV2WriteService(t, true, false)
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()

	recorder := httptest.NewRecorder()
	serveV2EntryWrite(recorder, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"clientNonce": "nonce-u", "text": "hello"}), service, "e1", "send")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	<-deliveries
	select {
	case event := <-deliveries:
		if event.Status != "unattributed" {
			t.Fatalf("delivery=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unattributed delivery")
	}
	deadline := time.Now().Add(time.Second)
	for {
		entry, status := store.lookup("e1")
		if status == http.StatusOK && entry.CanSend && entry.Attachment != nil && entry.Attachment.Status == "suspect" && entry.Attachment.SuspectReason == "delivery_unattributed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry after unattributed=%#v status=%d", entry, status)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestV2SendDirectTailRecheckDoesNotRunFullAttachmentReevaluation(t *testing.T) {
	store, service, _ := testV2WriteService(t, true, false)
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()
	service.dependencies.PrepareReconcile = func(v2TerminalEntry, string) func() bool {
		return func() bool { return false }
	}
	service.dependencies.MatchAttached = func(entryID, expected string) bool {
		return entryID == "e1" && expected == "hello"
	}

	if outcome := service.send("e1", "direct-tail", "hello"); outcome.Error != nil {
		t.Fatalf("outcome=%#v", outcome)
	}
	if event := <-deliveries; event.Status != "accepted" {
		t.Fatalf("accepted=%#v", event)
	}
	select {
	case event := <-deliveries:
		if event.Status != "echoed" {
			t.Fatalf("delivery=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for direct tail echo")
	}
}

func TestDefaultV2PrepareReconcileUsesExactRuntimeRecordForBusyQueueSuffix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(t.TempDir(), "66666666-6666-4666-8666-666666666666.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &sessionRecord{AgentSession: AgentSession{
		ID: "s-example-runtime", SessionID: "66666666-6666-4666-8666-666666666666",
		Kind: "claude", SessionFile: path,
	}}
	entry := testV2Entry("e1")
	entry.Attachment = &v2Attachment{RecordID: record.ID, SessionID: record.SessionID}
	entry.runtime = &v2EntryRuntime{Record: record}
	expected := "[CANARY-1784396100]"
	reconcile := defaultV2PrepareReconcile(entry, expected)
	if reconcile == nil {
		t.Fatal("exact runtime record was not used to prepare reconciliation")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]any{
		"type": "queue-operation", "operation": "enqueue",
		"content": "existing prompt\nunrelated prefix " + expected,
	})
	_, writeErr := file.Write(append(row, '\n'))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append=(%v,%v)", writeErr, closeErr)
	}
	if !reconcile() {
		t.Fatal("busy composer suffix did not reconcile against the exact runtime record")
	}
}

func TestDefaultV2MatchAttachedUsesExactRuntimeRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(t.TempDir(), "exact.jsonl")
	expected := "[exact-runtime-tail]"
	row, _ := json.Marshal(map[string]any{
		"type": "queue-operation", "operation": "enqueue",
		"content": "existing prompt" + expected,
	})
	if err := os.WriteFile(path, append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	bound := &sessionRecord{AgentSession: AgentSession{
		ID: "r1", SessionID: "session-r1", Kind: "claude", SessionFile: path,
	}}
	entry := testV2Entry("e1")
	entry.runtime = &v2EntryRuntime{}
	record := testV2Record("r1")
	record.SessionID = bound.SessionID
	store := newTerminalEntryStore("exact-record")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{
		Entry: entry, Record: &record, BoundRecord: bound, EvidenceRank: 3,
	}}})
	if !defaultV2MatchAttached(store, "e1", expected) {
		t.Fatal("attached tail did not use exact runtime record")
	}
}

func TestV2WriteReturnsRetryableNotReadyBeforeInitialSnapshot(t *testing.T) {
	store := newTerminalEntryStore("cold-write")
	var sends atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{
		Send: func(*paneBinding, string) error { sends.Add(1); return nil },
	})
	recorder := httptest.NewRecorder()
	serveV2EntryWrite(recorder, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{
		"clientNonce": "cold", "text": "hello",
	}), service, "e1", "send")
	assertV2ErrorEnvelope(t, recorder, http.StatusServiceUnavailable, "not_ready")
	var envelope v2ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Error.Retryable || sends.Load() != 0 {
		t.Fatalf("cold write envelope=%#v decode=%v sends=%d", envelope, err, sends.Load())
	}
}

func TestV2UnattachedSendDirectRecheckEchoesWithoutFullReevaluation(t *testing.T) {
	store := newTerminalEntryStore("new-attachment")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	var matches atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{
		Verify: func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return &paneBinding{Socket: "/private/tmp/test", TmuxID: "%1", Kind: "claude", ProcessPIDs: []int{123}}, nil
		},
		Send:             func(*paneBinding, string) error { return nil },
		PrepareReconcile: func(v2TerminalEntry, string) func() bool { return nil },
		MatchAttached: func(entryID, expected string) bool {
			return entryID == "e1" && expected == "hello" && matches.Add(1) >= 2
		},
	})
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()
	outcome := service.send("e1", "new-attachment", "hello")
	if outcome.Error != nil {
		t.Fatalf("outcome=%#v", outcome)
	}
	<-deliveries
	select {
	case event := <-deliveries:
		if event.Status != "echoed" || matches.Load() != 2 {
			t.Fatalf("event=%#v matches=%d", event, matches.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for post-attachment echo")
	}
}

func TestV2UnattachedSendEchoesOnAttachmentEventWithoutWaitingForTimeout(t *testing.T) {
	store := newTerminalEntryStore("late-attachment")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	var sends atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{
		Verify: func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return &paneBinding{Socket: "/private/tmp/test", TmuxID: "%1", Kind: "claude", ProcessPIDs: []int{123}}, nil
		},
		Send:             func(*paneBinding, string) error { sends.Add(1); return nil },
		PrepareReconcile: func(v2TerminalEntry, string) func() bool { return nil },
		MatchAttached: func(entryID, expected string) bool {
			entry, status := store.lookup(entryID)
			return status == http.StatusOK && expected == "hello" && entry.Attachment != nil && entry.Attachment.RecordID == "r1"
		},
	})
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()

	first := service.send("e1", "late", "hello")
	if first.Error != nil {
		t.Fatalf("first=%#v", first)
	}
	if event := <-deliveries; event.Status != "accepted" || event.DeliveryID != first.Response.DeliveryID {
		t.Fatalf("accepted=%#v", event)
	}
	select {
	case event := <-deliveries:
		if event.Status != "unattributed" || event.DeliveryID != first.Response.DeliveryID {
			t.Fatalf("unattributed=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unattributed delivery")
	}

	time.Sleep(20 * time.Millisecond)
	record := testV2Record("r1")
	attachedAt := time.Now()
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record, EvidenceRank: 3}}})
	select {
	case event := <-deliveries:
		if event.Status != "echoed" || event.DeliveryID != first.Response.DeliveryID || event.ClientNonce != "late" {
			t.Fatalf("echoed=%#v", event)
		}
		if elapsed := time.Since(attachedAt); elapsed > 100*time.Millisecond {
			t.Fatalf("attachment event echo took %s", elapsed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("attachment event waited for the removed late timeout")
	}

	retry := service.send("e1", "late", "hello")
	if retry.Error != nil || retry.Response.DeliveryID != first.Response.DeliveryID || sends.Load() != 1 {
		t.Fatalf("retry=%#v sends=%d", retry, sends.Load())
	}
	select {
	case event := <-deliveries:
		t.Fatalf("duplicate delivery event=%#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestV2UnattachedSendLateReconcileTimeoutStaysUnattributed(t *testing.T) {
	store := newTerminalEntryStore("late-timeout")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	service := newV2WriteService(store, v2WriteDependencies{
		Verify: func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return &paneBinding{Socket: "/private/tmp/test", TmuxID: "%1", Kind: "claude", ProcessPIDs: []int{123}}, nil
		},
		Send:             func(*paneBinding, string) error { return nil },
		PrepareReconcile: func(v2TerminalEntry, string) func() bool { return nil },
		MatchAttached:    func(string, string) bool { return false },
	})
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()
	if outcome := service.send("e1", "timeout", "hello"); outcome.Error != nil {
		t.Fatalf("outcome=%#v", outcome)
	}
	if event := <-deliveries; event.Status != "accepted" {
		t.Fatalf("accepted=%#v", event)
	}
	select {
	case event := <-deliveries:
		if event.Status != "unattributed" {
			t.Fatalf("unattributed=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unattributed delivery")
	}
	select {
	case event := <-deliveries:
		t.Fatalf("unexpected post-timeout delivery=%#v", event)
	case <-time.After(100 * time.Millisecond):
	}
	store.removeEntry("e1", "killed")
}

func TestV2SendRechecksMatchBeforePublishingUnattributed(t *testing.T) {
	store := newTerminalEntryStore("pre-unattributed-recheck")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	var matchCalls atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{
		Verify: func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return &paneBinding{Socket: "/private/tmp/test", TmuxID: "%1", Kind: "claude", ProcessPIDs: []int{123}}, nil
		},
		Send:             func(*paneBinding, string) error { return nil },
		PrepareReconcile: func(v2TerminalEntry, string) func() bool { return nil },
		MatchAttached:    func(string, string) bool { return matchCalls.Add(1) >= 2 },
	})
	deliveries, cancel := store.subscribeDeliveries("e1")
	defer cancel()
	if outcome := service.send("e1", "matched-before-unattributed", "hello"); outcome.Error != nil {
		t.Fatalf("outcome=%#v", outcome)
	}
	if event := <-deliveries; event.Status != "accepted" {
		t.Fatalf("accepted=%#v", event)
	}
	select {
	case event := <-deliveries:
		if event.Status != "echoed" {
			t.Fatalf("delivery=%#v want echoed without unattributed", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for echoed delivery")
	}
}

func TestV2WriteGenerationGoneAndNonceConflictHaveNoSideEffect(t *testing.T) {
	store, service, sends := testV2WriteService(t, false, true)
	store.commitWithRemovalReason(v2SnapshotInput{Host: v2Host{ID: "host"}}, "process_exit")
	recorder := httptest.NewRecorder()
	serveV2EntryWrite(recorder, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"clientNonce": "gone", "text": "hello"}), service, "e1", "send")
	assertV2ErrorEnvelope(t, recorder, http.StatusGone, "entry_gone")
	if sends.Load() != 0 {
		t.Fatalf("stale generation injected %d times", sends.Load())
	}

	store, service, sends = testV2WriteService(t, false, true)
	first := httptest.NewRecorder()
	serveV2EntryWrite(first, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"clientNonce": "same", "text": "hello"}), service, "e1", "send")
	conflict := httptest.NewRecorder()
	serveV2EntryWrite(conflict, v2JSONRequest(t, "/api/v2/entries/e1/choose", map[string]any{"clientNonce": "same", "option": 2}), service, "e1", "choose")
	assertV2ErrorEnvelope(t, conflict, http.StatusConflict, "client_nonce_conflict")
	if sends.Load() != 1 {
		t.Fatalf("nonce conflict side effects sends=%d", sends.Load())
	}
}

func TestV2AllWritesRevalidateGenerationBeforeSideEffects(t *testing.T) {
	store := newTerminalEntryStore("revalidate")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	var sideEffects atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{
		Verify: func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		},
		Send:   func(*paneBinding, string) error { sideEffects.Add(1); return nil },
		Choose: func(*paneBinding, int) error { sideEffects.Add(1); return nil },
		Kill:   func(*paneBinding) ([]int, error) { sideEffects.Add(1); return nil, nil },
		Upload: func(string, []byte) (string, error) { sideEffects.Add(1); return "/unexpected", nil },
	})

	requests := []struct {
		action  string
		request *http.Request
	}{
		{action: "send", request: v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"clientNonce": "send-gone", "text": "hello"})},
		{action: "choose", request: v2JSONRequest(t, "/api/v2/entries/e1/choose", map[string]any{"clientNonce": "choose-gone", "option": 1})},
		{action: "kill", request: v2JSONRequest(t, "/api/v2/entries/e1/kill", map[string]any{"clientNonce": "kill-gone"})},
	}
	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	_ = writer.WriteField("clientNonce", "upload-gone")
	part, _ := writer.CreateFormFile("file", "file.png")
	_, _ = part.Write([]byte("png"))
	_ = writer.Close()
	uploadRequest := httptest.NewRequest(http.MethodPost, "/api/v2/entries/e1/upload", bytes.NewReader(uploadBody.Bytes()))
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	requests = append(requests, struct {
		action  string
		request *http.Request
	}{action: "upload", request: uploadRequest})

	for _, test := range requests {
		recorder := httptest.NewRecorder()
		serveV2EntryWrite(recorder, test.request, service, "e1", test.action)
		assertV2ErrorEnvelope(t, recorder, http.StatusGone, "entry_gone")
	}
	if sideEffects.Load() != 0 {
		t.Fatalf("stale generation side effects=%d", sideEffects.Load())
	}
}

func TestV2UploadChooseAndKillResponsesAndIdempotency(t *testing.T) {
	store, service, _ := testV2WriteService(t, true, true)
	var uploads, choices, kills atomic.Int32
	service.dependencies.Upload = func(name string, data []byte) (string, error) {
		uploads.Add(1)
		if name != "photo.png" || string(data) != "png" {
			t.Fatalf("upload name=%q data=%q", name, data)
		}
		return "/Users/test/Library/Caches/corral-uploads/fixed.png", nil
	}
	service.dependencies.Choose = func(_ *paneBinding, option int) error {
		choices.Add(1)
		if option != 3 {
			t.Fatalf("option=%d", option)
		}
		return nil
	}
	service.dependencies.Kill = func(_ *paneBinding) ([]int, error) {
		kills.Add(1)
		return []int{22, 11}, nil
	}
	service.dependencies.PersistClosed = func(string) error { return nil }

	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	_ = writer.WriteField("clientNonce", "upload-1")
	part, _ := writer.CreateFormFile("file", "photo.png")
	_, _ = part.Write([]byte("png"))
	_ = writer.Close()
	uploadRequest := httptest.NewRequest(http.MethodPost, "/api/v2/entries/e1/upload", bytes.NewReader(uploadBody.Bytes()))
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	upload := httptest.NewRecorder()
	serveV2EntryWrite(upload, uploadRequest, service, "e1", "upload")
	uploadResponse := decodeV2WriteResponse(t, upload)
	if upload.Code != http.StatusOK || uploadResponse.Path != "/Users/test/Library/Caches/corral-uploads/fixed.png" || uploads.Load() != 1 {
		t.Fatalf("upload status=%d response=%#v count=%d", upload.Code, uploadResponse, uploads.Load())
	}
	uploadRequest = httptest.NewRequest(http.MethodPost, "/api/v2/entries/e1/upload", bytes.NewReader(uploadBody.Bytes()))
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRetry := httptest.NewRecorder()
	serveV2EntryWrite(uploadRetry, uploadRequest, service, "e1", "upload")
	if uploadRetry.Code != http.StatusOK || decodeV2WriteResponse(t, uploadRetry).DeliveryID != uploadResponse.DeliveryID || uploads.Load() != 1 {
		t.Fatalf("upload retry status=%d count=%d body=%s", uploadRetry.Code, uploads.Load(), uploadRetry.Body.String())
	}

	chooseBody := map[string]any{"clientNonce": "choose-1", "option": 3}
	choose := httptest.NewRecorder()
	serveV2EntryWrite(choose, v2JSONRequest(t, "/api/v2/entries/e1/choose", chooseBody), service, "e1", "choose")
	chooseResponse := decodeV2WriteResponse(t, choose)
	retry := httptest.NewRecorder()
	serveV2EntryWrite(retry, v2JSONRequest(t, "/api/v2/entries/e1/choose", chooseBody), service, "e1", "choose")
	if choose.Code != http.StatusOK || retry.Code != http.StatusOK || chooseResponse.Option != 3 || choices.Load() != 1 || decodeV2WriteResponse(t, retry).DeliveryID != chooseResponse.DeliveryID {
		t.Fatalf("choose response=%#v choices=%d", chooseResponse, choices.Load())
	}

	changes, cancel := store.subscribe(store.snapshot().Revision)
	defer cancel()
	killBody := map[string]any{"clientNonce": "kill-1"}
	kill := httptest.NewRecorder()
	serveV2EntryWrite(kill, v2JSONRequest(t, "/api/v2/entries/e1/kill", killBody), service, "e1", "kill")
	killResponse := decodeV2WriteResponse(t, kill)
	if kill.Code != http.StatusOK || !killResponse.Killed || len(killResponse.PIDs) != 2 || kills.Load() != 1 {
		t.Fatalf("kill status=%d response=%#v kills=%d", kill.Code, killResponse, kills.Load())
	}
	change := <-changes
	removed, ok := removedV2Entry(change, "e1")
	if !ok || removed.Reason != "killed" {
		t.Fatalf("removed=%#v change=%#v", removed, change)
	}
	retry = httptest.NewRecorder()
	serveV2EntryWrite(retry, v2JSONRequest(t, "/api/v2/entries/e1/kill", killBody), service, "e1", "kill")
	if retry.Code != http.StatusOK || decodeV2WriteResponse(t, retry).DeliveryID != killResponse.DeliveryID || kills.Load() != 1 {
		t.Fatalf("kill retry status=%d kills=%d body=%s", retry.Code, kills.Load(), retry.Body.String())
	}
}

func TestV2WriteRequiresClientNonceAndJSONErrors(t *testing.T) {
	_, service, sends := testV2WriteService(t, false, true)
	recorder := httptest.NewRecorder()
	serveV2EntryWrite(recorder, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"text": "hello"}), service, "e1", "send")
	assertV2ErrorEnvelope(t, recorder, http.StatusBadRequest, "client_nonce_required")
	if sends.Load() != 0 || !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("sends=%d content-type=%q", sends.Load(), recorder.Header().Get("Content-Type"))
	}
}

func TestV2IdentityComparisonRejectsEveryGenerationComponent(t *testing.T) {
	want := v2EntryIdentity{HostID: "host", SocketPath: "/tmp/tmux", SocketDevice: 1, SocketInode: 2, PaneID: "%1", AgentPID: 10, StartSec: 20, StartUsec: 30}
	if !sameV2EntryIdentity(want, want) {
		t.Fatal("identical generation did not match")
	}
	mutations := []v2EntryIdentity{
		{HostID: "other", SocketPath: want.SocketPath, SocketDevice: 1, SocketInode: 2, PaneID: "%1", AgentPID: 10, StartSec: 20, StartUsec: 30},
		{HostID: "host", SocketPath: "/tmp/other", SocketDevice: 1, SocketInode: 2, PaneID: "%1", AgentPID: 10, StartSec: 20, StartUsec: 30},
		{HostID: "host", SocketPath: want.SocketPath, SocketDevice: 9, SocketInode: 2, PaneID: "%1", AgentPID: 10, StartSec: 20, StartUsec: 30},
		{HostID: "host", SocketPath: want.SocketPath, SocketDevice: 1, SocketInode: 9, PaneID: "%1", AgentPID: 10, StartSec: 20, StartUsec: 30},
		{HostID: "host", SocketPath: want.SocketPath, SocketDevice: 1, SocketInode: 2, PaneID: "%9", AgentPID: 10, StartSec: 20, StartUsec: 30},
		{HostID: "host", SocketPath: want.SocketPath, SocketDevice: 1, SocketInode: 2, PaneID: "%1", AgentPID: 11, StartSec: 20, StartUsec: 30},
		{HostID: "host", SocketPath: want.SocketPath, SocketDevice: 1, SocketInode: 2, PaneID: "%1", AgentPID: 10, StartSec: 21, StartUsec: 30},
		{HostID: "host", SocketPath: want.SocketPath, SocketDevice: 1, SocketInode: 2, PaneID: "%1", AgentPID: 10, StartSec: 20, StartUsec: 31},
	}
	for index, got := range mutations {
		if sameV2EntryIdentity(want, got) {
			t.Fatalf("mutation %d matched: %#v", index, got)
		}
	}
}

func TestV2ChooseUsesOneNumericSendKeysCommandWithoutEnter(t *testing.T) {
	args := v2ChoiceArgs("%7", 3)
	want := []string{"send-keys", "-t", "%7", "3"}
	if len(args) != len(want) {
		t.Fatalf("args=%q", args)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("args=%q", args)
		}
	}
	for _, arg := range args {
		if arg == "Enter" || arg == "Down" {
			t.Fatalf("unexpected fallback key in args=%q", args)
		}
	}
}

func TestV2DeliverySuspectSurvivesUnchangedSnapshotAndEchoClearsIt(t *testing.T) {
	store := newTerminalEntryStore("suspect")
	record := testV2Record("r1")
	input := v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record, EvidenceRank: 3}}}
	store.commit(input)
	if !store.markAttachmentSuspect("e1", "delivery_unattributed") {
		t.Fatal("failed to mark suspect")
	}
	store.commit(input)
	entry, _ := store.lookup("e1")
	if entry.Attachment == nil || entry.Attachment.Status != "suspect" || entry.Attachment.SuspectReason != "delivery_unattributed" {
		t.Fatalf("suspect did not survive refresh: %#v", entry.Attachment)
	}
	if !store.clearAttachmentSuspect("e1") {
		t.Fatal("failed to clear suspect")
	}
	entry, _ = store.lookup("e1")
	if entry.Attachment == nil || entry.Attachment.Status != "attached" || entry.Attachment.SuspectReason != "" {
		t.Fatalf("echo did not clear suspect: %#v", entry.Attachment)
	}
}

func TestV2EntryStreamCarriesDeliveryEvents(t *testing.T) {
	store := newTerminalEntryStore("delivery-stream")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2EntryStream(w, r, store, "e1", fakeV2TimelineLoader(nil))
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	_ = readV2SSEEventFromReader(t, reader, "state")
	store.publishDelivery(v2DeliveryEvent{Type: "delivery", EntryID: "e1", ClientNonce: "nonce", DeliveryID: "delivery", Status: "accepted"})
	event := readV2SSEEventFromReader(t, reader, "delivery")
	if event.Data["entryId"] != "e1" || event.Data["clientNonce"] != "nonce" || event.Data["deliveryId"] != "delivery" || event.Data["status"] != "accepted" {
		t.Fatalf("delivery=%#v", event)
	}
}

func TestV2EntryStreamLogsDeliveryProducedToWrittenWithoutChangingWireShape(t *testing.T) {
	store := newTerminalEntryStore("delivery-stream-timing")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	var logs lockedV2LogBuffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2EntryStream(w, r, store, "e1", fakeV2TimelineLoader(nil))
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	_ = readV2SSEEventFromReader(t, reader, "state")
	store.publishDelivery(v2DeliveryEvent{Type: "delivery", EntryID: "e1", ClientNonce: "nonce", DeliveryID: "delivery", Status: "accepted"})
	event := readV2SSEEventFromReader(t, reader, "delivery")
	for _, field := range []string{"producedAt", "traceStarted"} {
		if _, leaked := event.Data[field]; leaked {
			t.Fatalf("internal timing field %q leaked into SSE: %#v", field, event.Data)
		}
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "v2 sse timing: delivery=delivery") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for _, want := range []string{"v2 sse timing: delivery=delivery entry=e1 status=accepted", "produced_us=", "written_us=", "latency_ms="} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing SSE timing %q in logs:\n%s", want, logs.String())
		}
	}
}

func TestAppendedCodexUserMessageMatchesOnlyExactResponseItem(t *testing.T) {
	expected := "first\nsecond"
	data := []byte(strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"first\nsecond"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first\nsecond"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first\nsecond"}]}}`,
	}, "\n"))
	if !appendedCodexUserMessageMatches(data, expected) {
		t.Fatal("exact Codex response_item user message did not match")
	}
	if appendedCodexUserMessageMatches(data, "different") {
		t.Fatal("non-exact Codex message matched")
	}
}
