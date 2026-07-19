package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type claudeHistoryEntry struct {
	SessionID string
	At        time.Time
	Ambiguous bool
}

type claudeHistoryState struct {
	path    string
	device  uint64
	inode   uint64
	offset  int64
	pending []byte
	entries map[string]claudeHistoryEntry
}

var (
	claudeHistoryMu    sync.Mutex
	claudeHistoryCache claudeHistoryState
)

func loadClaudeHistoryIndex() map[string]claudeHistoryEntry {
	path := filepath.Join(homeDir(), ".claude", "history.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return map[string]claudeHistoryEntry{}
	}
	device, inode := physicalIdentity(info)

	claudeHistoryMu.Lock()
	defer claudeHistoryMu.Unlock()
	cache := &claudeHistoryCache
	if cache.path != path || cache.device != device || cache.inode != inode || info.Size() < cache.offset {
		*cache = claudeHistoryState{path: path, device: device, inode: inode, entries: map[string]claudeHistoryEntry{}}
	}
	if cache.entries == nil {
		cache.entries = map[string]claudeHistoryEntry{}
	}
	if info.Size() > cache.offset {
		file, openErr := os.Open(path)
		if openErr == nil {
			_, seekErr := file.Seek(cache.offset, io.SeekStart)
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if seekErr == nil && readErr == nil {
				cache.offset = info.Size()
				data = append(append([]byte(nil), cache.pending...), data...)
				lines := bytes.Split(data, []byte{'\n'})
				cache.pending = append(cache.pending[:0], lines[len(lines)-1]...)
				for _, line := range lines[:len(lines)-1] {
					observeClaudeHistoryLine(cache.entries, line)
				}
			}
		}
	}
	result := make(map[string]claudeHistoryEntry, len(cache.entries))
	for cwd, entry := range cache.entries {
		result[cwd] = entry
	}
	return result
}

func observeClaudeHistoryLine(entries map[string]claudeHistoryEntry, line []byte) {
	var row struct {
		Project   string `json:"project"`
		SessionID string `json:"sessionId"`
		Timestamp int64  `json:"timestamp"`
	}
	if json.Unmarshal(line, &row) != nil || row.Project == "" || !uuidPattern.MatchString(row.SessionID) || row.Timestamp <= 0 {
		return
	}
	candidate := claudeHistoryEntry{SessionID: strings.ToLower(row.SessionID), At: time.UnixMilli(row.Timestamp)}
	current, ok := entries[row.Project]
	if !ok || candidate.At.After(current.At) {
		entries[row.Project] = candidate
		return
	}
	if candidate.At.Equal(current.At) && candidate.SessionID != current.SessionID {
		current.Ambiguous = true
		entries[row.Project] = current
	}
}

func invalidateClaudeHistoryCache() {
	claudeHistoryMu.Lock()
	claudeHistoryCache = claudeHistoryState{}
	claudeHistoryMu.Unlock()
}
