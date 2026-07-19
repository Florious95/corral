package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testV2TerminalService(t *testing.T, entryIDs ...string) (*terminalEntryStore, *v2WriteService, *atomic.Int32) {
	t.Helper()
	store := newTerminalEntryStore("terminal")
	drafts := make([]v2EntryDraft, 0, len(entryIDs))
	for _, entryID := range entryIDs {
		drafts = append(drafts, v2EntryDraft{Entry: testV2Entry(entryID)})
	}
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: drafts})
	var verifies atomic.Int32
	service := newV2WriteService(store, v2WriteDependencies{Verify: func(entry v2TerminalEntry) (*paneBinding, *v2WriteError) {
		verifies.Add(1)
		return &paneBinding{Socket: "/private/tmp/test", TmuxID: "%1", PanePID: 42, Kind: entry.Kind}, nil
	}})
	return store, service, &verifies
}

func resetV2ScreenTestState(t *testing.T) {
	t.Helper()
	previous := v2ScreenCapturePane
	v2ScreenFailures.Lock()
	v2ScreenFailures.until = map[string]time.Time{}
	v2ScreenFailures.Unlock()
	t.Cleanup(func() {
		v2ScreenCapturePane = previous
		v2ScreenFailures.Lock()
		v2ScreenFailures.until = map[string]time.Time{}
		v2ScreenFailures.Unlock()
	})
}

func TestV2ScreenReturnsANSIPositionAndConditionalETagAfterIdentityVerification(t *testing.T) {
	resetV2ScreenTestState(t)
	_, service, verifies := testV2TerminalService(t, "e1")
	v2ScreenCapturePane = func(*paneBinding) (v2ScreenCapture, error) {
		return v2ScreenCapture{Content: "\x1b[31mred\x1b[0m\n", Cols: 120, Rows: 40, CursorX: 7, CursorY: 9}, nil
	}
	first := httptest.NewRecorder()
	serveV2EntryScreen(first, httptest.NewRequest(http.MethodGet, "/api/v2/entries/e1/screen", nil), service, "e1")
	if first.Code != http.StatusOK || first.Header().Get("ETag") == "" || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("status=%d headers=%v body=%s", first.Code, first.Header(), first.Body.String())
	}
	var response v2ScreenResponse
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil || response.EntryID != "e1" || response.Cols != 120 || response.Rows != 40 || response.CursorX != 7 || response.CursorY != 9 || response.Content != "\x1b[31mred\x1b[0m\n" || len(response.Hash) != 64 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	secondRequest := httptest.NewRequest(http.MethodGet, "/api/v2/entries/e1/screen", nil)
	secondRequest.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	serveV2EntryScreen(second, secondRequest, service, "e1")
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 || verifies.Load() != 2 {
		t.Fatalf("status=%d body=%q verifies=%d", second.Code, second.Body.String(), verifies.Load())
	}
}

func TestV2ScreenFailureBackoffIsEntryLocal(t *testing.T) {
	resetV2ScreenTestState(t)
	_, service, _ := testV2TerminalService(t, "bad", "good")
	var badCalls atomic.Int32
	v2ScreenCapturePane = func(binding *paneBinding) (v2ScreenCapture, error) {
		if binding.TmuxID == "%1" && badCalls.Add(1) == 1 {
			return v2ScreenCapture{}, errors.New("input/output error")
		}
		return v2ScreenCapture{Content: "ok", Cols: 80, Rows: 24}, nil
	}
	bad := httptest.NewRecorder()
	serveV2EntryScreen(bad, httptest.NewRequest(http.MethodGet, "/api/v2/entries/bad/screen", nil), service, "bad")
	assertV2ErrorEnvelope(t, bad, http.StatusServiceUnavailable, "screen_unavailable")
	retry := httptest.NewRecorder()
	serveV2EntryScreen(retry, httptest.NewRequest(http.MethodGet, "/api/v2/entries/bad/screen", nil), service, "bad")
	assertV2ErrorEnvelope(t, retry, http.StatusServiceUnavailable, "screen_unavailable")
	good := httptest.NewRecorder()
	serveV2EntryScreen(good, httptest.NewRequest(http.MethodGet, "/api/v2/entries/good/screen", nil), service, "good")
	if good.Code != http.StatusOK || badCalls.Load() != 2 {
		t.Fatalf("good status=%d capture calls=%d body=%s", good.Code, badCalls.Load(), good.Body.String())
	}
}

func TestV2KeysWhitelistIdempotencyAndNonceConflict(t *testing.T) {
	_, service, verifies := testV2TerminalService(t, "e1")
	var injected atomic.Int32
	service.dependencies.Keys = func(_ *paneBinding, _ string) error { injected.Add(1); return nil }
	allowed := []string{"0", "9", "a", "Z", "Enter", "Up", "Down", "Left", "Right", "Escape", "Tab", "Ctrl+C"}
	for index, key := range allowed {
		body := map[string]any{"clientNonce": fmt.Sprintf("nonce-%d", index), "key": key}
		first := httptest.NewRecorder()
		serveV2EntryWrite(first, v2JSONRequest(t, "/api/v2/entries/e1/keys", body), service, "e1", "keys")
		second := httptest.NewRecorder()
		serveV2EntryWrite(second, v2JSONRequest(t, "/api/v2/entries/e1/keys", body), service, "e1", "keys")
		firstResponse, secondResponse := decodeV2WriteResponse(t, first), decodeV2WriteResponse(t, second)
		if first.Code != http.StatusOK || second.Code != http.StatusOK || firstResponse.Key != key || firstResponse.DeliveryID == "" || secondResponse.DeliveryID != firstResponse.DeliveryID {
			t.Fatalf("key=%q first=%#v second=%#v", key, firstResponse, secondResponse)
		}
	}
	beforeInvalid := verifies.Load()
	for _, key := range []string{"", "aa", "!", "Space", "Ctrl+V"} {
		recorder := httptest.NewRecorder()
		serveV2EntryWrite(recorder, v2JSONRequest(t, "/api/v2/entries/e1/keys", map[string]any{"clientNonce": "bad-" + key, "key": key}), service, "e1", "keys")
		assertV2ErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_key")
	}
	if injected.Load() != int32(len(allowed)) || verifies.Load() != beforeInvalid {
		t.Fatalf("injected=%d verifies before=%d after=%d", injected.Load(), beforeInvalid, verifies.Load())
	}
	conflict := httptest.NewRecorder()
	serveV2EntryWrite(conflict, v2JSONRequest(t, "/api/v2/entries/e1/send", map[string]any{"clientNonce": "nonce-0", "text": "text"}), service, "e1", "send")
	assertV2ErrorEnvelope(t, conflict, http.StatusConflict, "client_nonce_conflict")
}

func TestV2KeyArgsAreOnlyLiteralOrNamedSendKeys(t *testing.T) {
	tests := map[string][]string{
		"a":      {"send-keys", "-t", "%7", "-l", "--", "a"},
		"Enter":  {"send-keys", "-t", "%7", "Enter"},
		"Escape": {"send-keys", "-t", "%7", "Escape"},
		"Ctrl+C": {"send-keys", "-t", "%7", "C-c"},
	}
	for key, want := range tests {
		if got := v2KeyArgs("%7", key); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("key=%q args=%q want=%q", key, got, want)
		}
	}
}

func TestV2ScreenTmuxArgsAreReadOnlyAndPreserveANSI(t *testing.T) {
	dimensions := []string{"display-message", "-p", "-t", "%7", "#{pane_width}\t#{pane_height}\t#{cursor_x}\t#{cursor_y}"}
	capture := []string{"capture-pane", "-e", "-p", "-t", "%7"}
	if strings.Join(v2ScreenDimensionArgs("%7"), "\x00") != strings.Join(dimensions, "\x00") || strings.Join(v2ScreenCaptureArgs("%7"), "\x00") != strings.Join(capture, "\x00") {
		t.Fatalf("dimension args=%q capture args=%q", v2ScreenDimensionArgs("%7"), v2ScreenCaptureArgs("%7"))
	}
}
