package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGzipMiddlewareCompressesJSONOnlyWhenRequestedAndNeverSSE(t *testing.T) {
	jsonHandler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"payload": strings.Repeat("compressible", 100)})
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/snapshot", nil)
	request.Header.Set("Accept-Encoding", "br, gzip")
	recorder := httptest.NewRecorder()
	jsonHandler.ServeHTTP(recorder, request)
	if recorder.Header().Get("Content-Encoding") != "gzip" || recorder.Header().Get("Vary") != "Accept-Encoding" || recorder.Header().Get("Content-Length") != "" {
		t.Fatalf("headers=%v", recorder.Header())
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || json.Valid(decoded) == false {
		t.Fatalf("decoded=%q err=%v", decoded, err)
	}

	plain := httptest.NewRecorder()
	jsonHandler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if plain.Header().Get("Content-Encoding") != "" || !json.Valid(plain.Body.Bytes()) {
		t.Fatalf("plain headers=%v body=%q", plain.Header(), plain.Body.String())
	}

	stream := httptest.NewRecorder()
	streamRequest := httptest.NewRequest(http.MethodGet, "/api/v2/stream", nil)
	streamRequest.Header.Set("Accept-Encoding", "gzip")
	gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: state\n\n")
	})).ServeHTTP(stream, streamRequest)
	if stream.Header().Get("Content-Encoding") != "" || stream.Body.String() != "event: state\n\n" {
		t.Fatalf("stream headers=%v body=%q", stream.Header(), stream.Body.String())
	}

	declined := httptest.NewRecorder()
	declinedRequest := httptest.NewRequest(http.MethodGet, "/api/v2/snapshot", nil)
	declinedRequest.Header.Set("Accept-Encoding", "gzip;q=0")
	jsonHandler.ServeHTTP(declined, declinedRequest)
	if declined.Header().Get("Content-Encoding") != "" || !json.Valid(declined.Body.Bytes()) {
		t.Fatalf("declined headers=%v body=%q", declined.Header(), declined.Body.String())
	}
}

func TestStaticCacheHeaders(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":          "index",
		"assets/index-a1.js":  "asset",
		"assets/index-b2.css": "style",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handler := spaHandler(dir)
	for path, want := range map[string]string{
		"/":                    "no-cache",
		"/index.html":          "no-cache",
		"/missing-route":       "no-cache",
		"/assets/index-a1.js":  "public, max-age=31536000, immutable",
		"/assets/index-b2.css": "public, max-age=31536000, immutable",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s Cache-Control=%q, want %q", path, got, want)
		}
	}
}

func TestV2EventEngineDisableIsExplicitOptIn(t *testing.T) {
	t.Setenv("V2_EVENT_ENGINE_DISABLE", "")
	if !v2EventEngineBackgroundEnabled() {
		t.Fatal("v2 event engine background loop must remain enabled by default")
	}
	t.Setenv("V2_EVENT_ENGINE_DISABLE", "1")
	if v2EventEngineBackgroundEnabled() {
		t.Fatal("V2_EVENT_ENGINE_DISABLE=1 must disable the background loop")
	}
}
