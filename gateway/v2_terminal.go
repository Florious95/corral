package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type v2ScreenCapture struct {
	Content string
	Cols    int
	Rows    int
	CursorX int
	CursorY int
}

type v2ScreenResponse struct {
	EntryID string `json:"entryId"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
	Content string `json:"content"`
	Hash    string `json:"hash"`
	CursorX int    `json:"cursorX"`
	CursorY int    `json:"cursorY"`
}

var v2ScreenCapturePane = captureV2Screen

var v2ScreenFailures = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

func captureV2Screen(binding *paneBinding) (v2ScreenCapture, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	log.Printf("v2 screen: socket=%q pane=%s operation=display-metadata", binding.Socket, binding.TmuxID)
	dimensions, err := tmuxCommandContext(ctx, binding.Socket, v2ScreenDimensionArgs(binding.TmuxID)...).Output()
	if err != nil {
		return v2ScreenCapture{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(dimensions)), "\t")
	if len(fields) != 4 {
		return v2ScreenCapture{}, fmt.Errorf("unexpected pane dimensions %q", dimensions)
	}
	cols, colsErr := strconv.Atoi(fields[0])
	rows, rowsErr := strconv.Atoi(fields[1])
	cursorX, cursorXErr := strconv.Atoi(fields[2])
	cursorY, cursorYErr := strconv.Atoi(fields[3])
	if colsErr != nil || rowsErr != nil || cursorXErr != nil || cursorYErr != nil || cols <= 0 || rows <= 0 || cursorX < 0 || cursorY < 0 {
		return v2ScreenCapture{}, fmt.Errorf("invalid pane dimensions %q", dimensions)
	}
	log.Printf("v2 screen: socket=%q pane=%s operation=capture-pane cols=%d rows=%d cursor_x=%d cursor_y=%d", binding.Socket, binding.TmuxID, cols, rows, cursorX, cursorY)
	content, err := tmuxCommandContext(ctx, binding.Socket, v2ScreenCaptureArgs(binding.TmuxID)...).Output()
	if err != nil {
		return v2ScreenCapture{}, err
	}
	return v2ScreenCapture{Content: string(content), Cols: cols, Rows: rows, CursorX: cursorX, CursorY: cursorY}, nil
}

func v2ScreenDimensionArgs(paneID string) []string {
	return []string{"display-message", "-p", "-t", paneID, "#{pane_width}\t#{pane_height}\t#{cursor_x}\t#{cursor_y}"}
}

func v2ScreenCaptureArgs(paneID string) []string {
	return []string{"capture-pane", "-e", "-p", "-t", paneID}
}

func serveV2EntryScreen(w http.ResponseWriter, r *http.Request, service *v2WriteService, entryID string) {
	if r.Method != http.MethodGet {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required", false)
		return
	}
	_, binding, writeErr := service.verifiedEntry(entryID)
	if writeErr != nil {
		writeV2WriteError(w, writeErr)
		return
	}
	v2ScreenFailures.Lock()
	blocked := time.Now().Before(v2ScreenFailures.until[entryID])
	v2ScreenFailures.Unlock()
	if blocked {
		writeV2Error(w, http.StatusServiceUnavailable, "screen_unavailable", "terminal screen is temporarily unavailable", true)
		return
	}
	screen, err := v2ScreenCapturePane(binding)
	if err != nil {
		log.Printf("v2 screen: entry=%s socket=%q pane=%s error=%v", entryID, binding.Socket, binding.TmuxID, err)
		v2ScreenFailures.Lock()
		v2ScreenFailures.until[entryID] = time.Now().Add(time.Second)
		v2ScreenFailures.Unlock()
		writeV2Error(w, http.StatusServiceUnavailable, "screen_unavailable", "terminal screen capture failed", true)
		return
	}
	v2ScreenFailures.Lock()
	delete(v2ScreenFailures.until, entryID)
	v2ScreenFailures.Unlock()
	canonical := fmt.Sprintf("%d\x00%d\x00%d\x00%d\x00%s", screen.Cols, screen.Rows, screen.CursorX, screen.CursorY, screen.Content)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	etag := `"screen-` + hash + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, v2ScreenResponse{EntryID: entryID, Cols: screen.Cols, Rows: screen.Rows, Content: screen.Content, Hash: hash, CursorX: screen.CursorX, CursorY: screen.CursorY})
}
