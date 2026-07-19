package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var uuidAtEndPattern = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)
var uploadExtPattern = regexp.MustCompile(`(?i)^\.[a-z0-9]{1,16}$`)
var commandNamePattern = regexp.MustCompile(`(?s)<command-name>\s*/?([^<\s]+)\s*</command-name>`)
var commandMessagePattern = regexp.MustCompile(`(?s)<command-message>.*</command-message>`)
var commandArgsPattern = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
var localCommandCaveatPattern = regexp.MustCompile(`(?s)^\s*<local-command-caveat>.*</local-command-caveat>\s*$`)
var localCommandStdoutPattern = regexp.MustCompile(`(?s)^\s*<local-command-stdout>(.*)</local-command-stdout>\s*$`)
var ansiCSIPattern = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")

const maxUploadBytes = 20 * 1024 * 1024

var errUploadTooLarge = errors.New("attachment exceeds 20MB")

type AgentSession struct {
	ID                 string `json:"id"`
	Kind               string `json:"kind"`
	Cwd                string `json:"cwd"`
	Title              string `json:"title"`
	SessionID          string `json:"sessionId"`
	SessionFile        string `json:"sessionFile"`
	State              string `json:"state"`
	CanSend            bool   `json:"canSend"`
	Model              string `json:"model"`
	LastActivityAt     string `json:"lastActivityAt"`
	LastMessagePreview string `json:"lastMessagePreview"`
	Live               bool   `json:"live"`
	BindingReason      string `json:"bindingReason,omitempty"`
}

type sessionRecord struct {
	AgentSession
	mtime              time.Time
	size               int64
	device             uint64
	inode              uint64
	cwdHistory         map[string]bool
	lastType           string
	codexIndexedTitle  bool
	binding            *paneBinding
	bindingEvidence    string
	candidatePaneCount int
	claudeCustomTitle  string
	claudeAITitle      string
	claudeFirstUser    string
	timelineToolNames  map[string]string
	timelineQueued     map[string][]time.Time
}

type physicalFile struct {
	kind      string
	sessionID string
	path      string
	mtime     time.Time
	size      int64
	device    uint64
	inode     uint64
}

type metadataCacheEntry struct {
	mtime      time.Time
	size       int64
	device     uint64
	inode      uint64
	titleToken string
	record     *sessionRecord
	offset     int64
	tail       []byte
}

type metadataLoadCall struct {
	done chan struct{}
}

type timelineFileVersion struct {
	path      string
	device    uint64
	inode     uint64
	size      int64
	mtimeNano int64
}

type timelineCacheEntry struct {
	version timelineFileVersion
	events  []TimelineEvent
}

type timelineLoadCall struct {
	done   chan struct{}
	events []TimelineEvent
	err    error
}

var (
	metadataCacheMu   sync.Mutex
	metadataCache     = map[string]metadataCacheEntry{}
	metadataLoadMu    sync.Mutex
	metadataLoads     = map[string]*metadataLoadCall{}
	metadataParseHook func(string, int64)
	timelineCacheMu   sync.Mutex
	timelineCache     = map[string]timelineCacheEntry{}
	timelineLoadMu    sync.Mutex
	timelineLoads     = map[timelineFileVersion]*timelineLoadCall{}
	timelineParseHook func(string)
	visibleCacheMu    sync.Mutex
	visibleCacheAt    time.Time
	visibleCache      []*sessionRecord
	visibleV2Cache    []*sessionRecord
	writeBindingMu    sync.Mutex
	bindingStateMu    sync.RWMutex
	bindingStateAt    time.Time
	bindingDecisionMu sync.Mutex
	bindingDecisions  = map[string]string{}
	bindingIncidents  = map[string]string{}
	bindingClaudeHint = claudeSessionHintEvidence
	killMu            sync.Mutex
	weakBindings      = newWeakBindingTracker(gatewayStateFile("bindings.json", filepath.Join(homeDir(), "Library", "Caches", "corral-gateway", "bindings.json")))
	closedSessions    = newClosedSessionStore(gatewayStateFile("closed-sessions.json", filepath.Join(homeDir(), "Library", "Application Support", "Corral", "closed-sessions.json")))
)

type TimelineEvent struct {
	Seq       int64  `json:"seq"`
	TS        string `json:"ts"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Queued    bool   `json:"queued,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Skill     string `json:"skill,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Input     string `json:"input,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return os.Getenv("HOME")
}

func gatewayStateFile(name, fallback string) string {
	if dir := os.Getenv("RC_GATEWAY_STATE_DIR"); dir != "" {
		return filepath.Join(dir, name)
	}
	return fallback
}

func stableSessionID(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(sessionID)))
	return "s-" + hex.EncodeToString(sum[:6])
}

func sessionIDFromPath(path string) string {
	match := uuidAtEndPattern.FindStringSubmatch(filepath.Base(path))
	if len(match) != 2 {
		return ""
	}
	return strings.ToLower(match[1])
}

func hasPathSegment(path, segment string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == segment {
			return true
		}
	}
	return false
}

func preferPhysical(current, candidate physicalFile) physicalFile {
	if current.path == "" || candidate.mtime.After(current.mtime) ||
		(candidate.mtime.Equal(current.mtime) && candidate.path < current.path) {
		return candidate
	}
	return current
}

func physicalIdentity(info fs.FileInfo) (uint64, uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), uint64(stat.Ino)
	}
	return 0, 0
}

func discoverPhysicalFiles() map[string]physicalFile {
	home := homeDir()
	winners := map[string]physicalFile{}
	claudeRoot := filepath.Join(home, ".claude", "projects")
	_ = filepath.WalkDir(claudeRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "" || hasPathSegment(path, "subagents") {
			return nil
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !strings.HasSuffix(entry.Name(), ".jsonl") || !uuidPattern.MatchString(sessionID) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		device, inode := physicalIdentity(info)
		candidate := physicalFile{kind: "claude", sessionID: strings.ToLower(sessionID), path: path, mtime: info.ModTime(), size: info.Size(), device: device, inode: inode}
		key := candidate.kind + "\x00" + candidate.sessionID
		winners[key] = preferPhysical(winners[key], candidate)
		return nil
	})
	for _, root := range []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")} {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if entry.Name() == "subagents" {
					return filepath.SkipDir
				}
				return nil
			}
			sessionID := sessionIDFromPath(path)
			if sessionID == "" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			device, inode := physicalIdentity(info)
			candidate := physicalFile{kind: "codex", sessionID: sessionID, path: path, mtime: info.ModTime(), size: info.Size(), device: device, inode: inode}
			key := candidate.kind + "\x00" + candidate.sessionID
			winners[key] = preferPhysical(winners[key], candidate)
			return nil
		})
	}
	return winners
}

func parseContentText(raw json.RawMessage, accepted ...string) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		return direct
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	allowed := map[string]bool{}
	for _, itemType := range accepted {
		allowed[itemType] = true
	}
	var texts []string
	for _, item := range items {
		if allowed[item.Type] && strings.TrimSpace(item.Text) != "" {
			texts = append(texts, item.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func truncateRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

type claudeMetadataRow struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	Cwd         string `json:"cwd"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	Subtype     string `json:"subtype"`
	Operation   string `json:"operation"`
	Content     string `json:"content"`
	Message     struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func newClaudeMetadataRecord(file physicalFile) *sessionRecord {
	return &sessionRecord{
		AgentSession: AgentSession{Kind: "claude", SessionID: file.sessionID, SessionFile: file.path},
		mtime:        file.mtime, size: file.size, device: file.device, inode: file.inode,
		cwdHistory:        map[string]bool{},
		timelineToolNames: map[string]string{},
		timelineQueued:    map[string][]time.Time{},
	}
}

func applyClaudeMetadataLine(record *sessionRecord, line []byte) bool {
	var row claudeMetadataRow
	if json.Unmarshal(line, &row) != nil {
		return false
	}
	if row.Cwd != "" {
		if record.Cwd == "" {
			record.Cwd = row.Cwd
		}
		record.cwdHistory[row.Cwd] = true
	}
	if row.CustomTitle != "" {
		record.claudeCustomTitle = row.CustomTitle
	}
	if row.AITitle != "" {
		record.claudeAITitle = row.AITitle
	}
	if row.Message.Model != "" {
		record.Model = row.Message.Model
	}
	if row.Type == "queue-operation" && row.Operation == "enqueue" && row.Content != "" {
		key := normalizeSendEcho(row.Content)
		if queuedAt, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil && key != "" {
			record.timelineQueued[key] = append(record.timelineQueued[key], queuedAt)
		}
	}
	if row.Type == "user" {
		text := parseContentText(row.Message.Content, "text")
		if text != "" {
			if record.claudeFirstUser == "" {
				record.claudeFirstUser = text
			}
			record.LastMessagePreview = truncateRunes(text, 160)
			record.lastType = "user_message"
		}
		var items []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(row.Message.Content, &items) == nil {
			for _, item := range items {
				if item.Type == "tool_result" {
					record.lastType = "tool_result"
				}
			}
		}
	}
	if row.Type == "assistant" {
		text := parseContentText(row.Message.Content, "text")
		if text != "" {
			record.LastMessagePreview = truncateRunes(text, 160)
			record.lastType = "assistant_message"
		}
		var items []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(row.Message.Content, &items) == nil {
			for _, item := range items {
				if item.Type == "tool_use" {
					record.lastType = "tool_use"
					if item.ID != "" && item.Name != "" {
						record.timelineToolNames[item.ID] = item.Name
					}
				}
			}
		}
	}
	if row.Type == "system" && strings.Contains(row.Subtype, "permission") {
		record.lastType = "status"
	}
	return true
}

func finalizeClaudeMetadata(record *sessionRecord) {
	switch {
	case record.claudeCustomTitle != "":
		record.Title = record.claudeCustomTitle
	case record.claudeAITitle != "":
		record.Title = record.claudeAITitle
	case record.claudeFirstUser != "":
		record.Title = truncateRunes(record.claudeFirstUser, 80)
	default:
		record.Title = "Claude 会话"
	}
}

func parseClaudeMetadataFrom(file physicalFile, base *sessionRecord, offset int64, tail []byte) (*sessionRecord, int64, []byte) {
	if metadataParseHook != nil {
		metadataParseHook(file.path, offset)
	}
	f, err := os.Open(file.path)
	if err != nil {
		return nil, 0, nil
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, nil
	}
	record := cloneSessionRecord(base)
	if record == nil {
		record = newClaudeMetadataRecord(file)
	}
	pending := append([]byte(nil), tail...)
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		chunk, readErr := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			line := append(pending, chunk...)
			pending = nil
			if line[len(line)-1] == '\n' {
				line = bytes.TrimSuffix(line, []byte{'\n'})
				line = bytes.TrimSuffix(line, []byte{'\r'})
				applyClaudeMetadataLine(record, line)
			} else {
				pending = append([]byte(nil), line...)
				applyClaudeMetadataLine(record, line)
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return nil, 0, nil
			}
			break
		}
	}
	parsedOffset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, 0, nil
	}
	if info, err := f.Stat(); err == nil {
		record.mtime = info.ModTime()
		record.size = parsedOffset
		record.device, record.inode = file.device, file.inode
	}
	finalizeClaudeMetadata(record)
	return record, parsedOffset, pending
}

func parseClaudeMetadata(file physicalFile) *sessionRecord {
	record, _, _ := parseClaudeMetadataFrom(file, nil, 0, nil)
	return record
}

func loadCodexTitles() map[string]string {
	titles := map[string]string{}
	f, err := os.Open(filepath.Join(homeDir(), ".codex", "session_index.jsonl"))
	if err != nil {
		return titles
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil && row.ID != "" && row.ThreadName != "" {
			titles[strings.ToLower(row.ID)] = row.ThreadName
		}
	}
	return titles
}

func parseCodexMetadata(file physicalFile, titles map[string]string) *sessionRecord {
	f, err := os.Open(file.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	record := &sessionRecord{
		AgentSession:      AgentSession{Kind: "codex", SessionID: file.sessionID, SessionFile: file.path, Title: titles[file.sessionID]},
		mtime:             file.mtime,
		size:              file.size,
		device:            file.device,
		inode:             file.inode,
		cwdHistory:        map[string]bool{},
		timelineToolNames: map[string]string{},
		timelineQueued:    map[string][]time.Time{},
		codexIndexedTitle: titles[file.sessionID] != "",
	}
	var firstUser string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string          `json:"type"`
				ID      string          `json:"id"`
				Cwd     string          `json:"cwd"`
				Model   string          `json:"model"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Name    string          `json:"name"`
				CallID  string          `json:"call_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if row.Type == "session_meta" && row.Payload.Cwd != "" && record.Cwd == "" {
			record.Cwd = row.Payload.Cwd
			record.cwdHistory[row.Payload.Cwd] = true
		}
		if row.Payload.Model != "" {
			record.Model = row.Payload.Model
		}
		if row.Type == "response_item" && row.Payload.CallID != "" && row.Payload.Name != "" {
			record.timelineToolNames[row.Payload.CallID] = row.Payload.Name
		}
		if row.Type != "response_item" || row.Payload.Type != "message" {
			continue
		}
		if row.Payload.Role != "user" && row.Payload.Role != "assistant" {
			continue
		}
		text := parseContentText(row.Payload.Content, "input_text", "output_text")
		if text == "" {
			continue
		}
		if row.Payload.Role == "user" && firstUser == "" && !strings.HasPrefix(strings.TrimSpace(text), "# AGENTS.md instructions") {
			firstUser = text
		}
		record.LastMessagePreview = truncateRunes(text, 160)
		record.lastType = row.Payload.Role + "_message"
	}
	if record.Title == "" {
		record.Title = truncateRunes(firstUser, 80)
	}
	if record.Title == "" {
		record.Title = "Codex 会话"
	}
	return record
}

func validCodexWindowName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if _, err := strconv.Atoi(name); err == nil {
		return false
	}
	switch strings.ToLower(name) {
	case "zsh", "bash", "node", "codex":
		return false
	}
	return true
}

func applyCodexWindowTitle(record *sessionRecord, pane *paneBinding) {
	if record.Kind == "codex" && !record.codexIndexedTitle && validCodexWindowName(pane.WindowName) {
		record.Title = strings.TrimSpace(pane.WindowName)
	}
}

func cloneSessionRecord(record *sessionRecord) *sessionRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.binding = nil
	clone.Live = false
	clone.CanSend = false
	clone.State = ""
	clone.cwdHistory = make(map[string]bool, len(record.cwdHistory))
	for cwd := range record.cwdHistory {
		clone.cwdHistory[cwd] = true
	}
	clone.timelineToolNames = make(map[string]string, len(record.timelineToolNames))
	for id, name := range record.timelineToolNames {
		clone.timelineToolNames[id] = name
	}
	clone.timelineQueued = make(map[string][]time.Time, len(record.timelineQueued))
	for key, timestamps := range record.timelineQueued {
		clone.timelineQueued[key] = append([]time.Time(nil), timestamps...)
	}
	return &clone
}

func cachedMetadata(file physicalFile, titles map[string]string) *sessionRecord {
	titleToken := ""
	if file.kind == "codex" {
		titleToken = titles[file.sessionID]
	}
	metadataCacheMu.Lock()
	entry, ok := metadataCache[file.path]
	if ok && entry.device == file.device && entry.inode == file.inode && entry.titleToken == titleToken &&
		entry.mtime.Equal(file.mtime) && entry.size == file.size {
		record := cloneSessionRecord(entry.record)
		metadataCacheMu.Unlock()
		return record
	}
	metadataCacheMu.Unlock()

	metadataLoadMu.Lock()
	if call := metadataLoads[file.path]; call != nil {
		done := call.done
		metadataLoadMu.Unlock()
		<-done
		return cachedMetadata(file, titles)
	}
	metadataCacheMu.Lock()
	entry, ok = metadataCache[file.path]
	if ok && entry.device == file.device && entry.inode == file.inode && entry.titleToken == titleToken &&
		entry.mtime.Equal(file.mtime) && entry.size == file.size {
		record := cloneSessionRecord(entry.record)
		metadataCacheMu.Unlock()
		metadataLoadMu.Unlock()
		return record
	}
	metadataCacheMu.Unlock()
	call := &metadataLoadCall{done: make(chan struct{})}
	metadataLoads[file.path] = call
	metadataLoadMu.Unlock()
	defer func() {
		metadataLoadMu.Lock()
		delete(metadataLoads, file.path)
		close(call.done)
		metadataLoadMu.Unlock()
	}()

	metadataCacheMu.Lock()
	entry, ok = metadataCache[file.path]
	metadataCacheMu.Unlock()
	var record *sessionRecord
	var offset int64
	var tail []byte
	if file.kind == "claude" {
		if ok && entry.device == file.device && entry.inode == file.inode && file.size > entry.size {
			offset = entry.offset
			tail = entry.tail
			record, offset, tail = parseClaudeMetadataFrom(file, entry.record, offset, tail)
		} else {
			record, offset, tail = parseClaudeMetadataFrom(file, nil, 0, nil)
		}
	} else {
		record = parseCodexMetadata(file, titles)
		offset = file.size
	}
	if record != nil {
		metadataCacheMu.Lock()
		metadataCache[file.path] = metadataCacheEntry{
			mtime: record.mtime, size: record.size, device: record.device, inode: record.inode,
			titleToken: titleToken, record: cloneSessionRecord(record), offset: offset, tail: append([]byte(nil), tail...),
		}
		metadataCacheMu.Unlock()
		_, _ = ensureTimelineLineIndex(file.path)
	}
	return record
}

func collectAllRecords(bind bool) ([]*sessionRecord, bool) {
	return collectAllRecordsWithRecovery(bind, "")
}

func collectAllRecordsWithRecovery(bind bool, recoveryID string) ([]*sessionRecord, bool) {
	started := time.Now()
	physical := discoverPhysicalFiles()
	discoveredAt := time.Now()
	titles := loadCodexTitles()
	records := make([]*sessionRecord, 0, len(physical))
	var recordsMu sync.Mutex
	hints := map[string]bool{}
	history := map[string]claudeHistoryEntry{}
	liveValid := true
	if bind {
		hints, liveValid = liveSessionHintKeys()
		for key := range weakBindings.hintKeys() {
			hints[key] = true
		}
		history = loadClaudeHistoryIndex()
		paneCwds := map[string]bool{}
		inspection := inspectLiveState()
		for _, pane := range inspection.panes {
			if pane.Kind != "claude" {
				continue
			}
			for _, pid := range pane.ProcessPIDs {
				if cwd := inspection.files.cwd[pid]; cwd != "" {
					paneCwds[cwd] = true
				}
			}
		}
		for cwd, entry := range history {
			if paneCwds[cwd] && entry.SessionID != "" {
				hints["claude\x00"+entry.SessionID] = true
			}
		}
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	parseFile := func(file physicalFile) {
		record := cachedMetadata(file, titles)
		if record == nil {
			return
		}
		record.ID = stableSessionID(record.SessionID)
		record.LastActivityAt = record.mtime.UTC().Format(time.RFC3339Nano)
		recordsMu.Lock()
		records = append(records, record)
		recordsMu.Unlock()
	}
	sem := make(chan struct{}, 8)
	var parseWG sync.WaitGroup
	for key, file := range physical {
		if bind && file.mtime.Before(cutoff) && !hints[key] {
			continue
		}
		sem <- struct{}{}
		parseWG.Add(1)
		go func(file physicalFile) {
			defer parseWG.Done()
			defer func() { <-sem }()
			parseFile(file)
		}(file)
	}
	parseWG.Wait()
	parsedAt := time.Now()
	if bind {
		outcome, panes, bindValid := bindLiveRecordsWithRecovery(records, history, recoveryID)
		records = outcome.records
		liveValid = liveValid && bindValid
		if outcome.bound < panes {
			log.Printf("session binding: %d/%d agent panes mapped; unmatched panes remain read-only with bindingReason", outcome.bound, panes)
		}
	}
	finishedAt := time.Now()
	if finishedAt.Sub(started) > time.Second {
		log.Printf("session scan: files=%d discover=%s parse=%s bind=%s", len(physical), discoveredAt.Sub(started), parsedAt.Sub(discoveredAt), finishedAt.Sub(parsedAt))
	}
	return records, liveValid
}

type bindingOutcome struct {
	bound   int
	records []*sessionRecord
}

func bindLiveRecords(records []*sessionRecord, history map[string]claudeHistoryEntry) (bindingOutcome, int, bool) {
	return bindLiveRecordsWithRecovery(records, history, "")
}

func bindLiveRecordsWithRecovery(records []*sessionRecord, history map[string]claudeHistoryEntry, recoveryID string) (bindingOutcome, int, bool) {
	inspection := inspectLiveStateMaxAge(maxWriteBindingAge)
	if !inspection.valid || inspection.observedAt.IsZero() || time.Since(inspection.observedAt) > maxWriteBindingAge {
		return bindingOutcome{records: records}, 0, false
	}
	outcome := bindInspectionDetailedForRecovery(records, inspection, weakBindings, history, time.Now(), recoveryID)
	bindingStateMu.Lock()
	bindingStateAt = time.Now()
	bindingStateMu.Unlock()
	return outcome, len(inspection.panes), true
}

func bindInspection(records []*sessionRecord, inspection liveInspection) int {
	return bindInspectionWithTracker(records, inspection, newWeakBindingTracker(""))
}

func bindInspectionWithTracker(records []*sessionRecord, inspection liveInspection, tracker *weakBindingTracker) int {
	return bindInspectionDetailed(records, inspection, tracker, nil, time.Now()).bound
}

func bindInspectionDetailed(records []*sessionRecord, inspection liveInspection, tracker *weakBindingTracker, history map[string]claudeHistoryEntry, now time.Time) bindingOutcome {
	return bindInspectionDetailedForRecovery(records, inspection, tracker, history, now, "")
}

func bindInspectionDetailedForRecovery(records []*sessionRecord, inspection liveInspection, tracker *weakBindingTracker, history map[string]claudeHistoryEntry, now time.Time, recoveryID string) bindingOutcome {
	byKey := map[string]*sessionRecord{}
	for _, record := range records {
		record.binding = nil
		record.bindingEvidence = ""
		record.candidatePaneCount = 0
		record.Live = false
		record.CanSend = false
		record.State = ""
		record.BindingReason = ""
		byKey[record.Kind+"\x00"+record.SessionID] = record
	}
	panes := inspection.panes
	files := inspection.files
	snap := inspection.snap
	bound := map[string]bool{}
	reservedPanes := map[int]bool{}
	boundPanes := map[string]bool{}
	strongRecordReasons := map[string]string{}
	strongReadOnly := map[int]stickyBinding{}
	bind := func(pane *paneBinding, matchID, evidence string) bool {
		key := pane.Kind + "\x00" + matchID
		record := byKey[key]
		if record == nil || bound[key] {
			return false
		}
		copy := *pane
		record.binding = &copy
		record.bindingEvidence = evidence
		applyCodexWindowTitle(record, &copy)
		record.Live = true
		record.CanSend = true
		bound[key] = true
		boundPanes[paneIdentityKey(pane)] = true
		return true
	}
	// Reserve every strong match before cwd fallback. A strongly identified pane
	// belongs only to that session, even when its record is not in this scan.
	for i := range panes {
		pane := &panes[i]
		matches := map[string]bool{}
		if pane.Kind == "codex" {
			for _, pid := range pane.ProcessPIDs {
				for _, path := range files.open[pid] {
					if strings.Contains(path, "/.codex/sessions/") {
						if id := sessionIDFromPath(path); id != "" {
							matches[id] = true
						}
					}
				}
			}
		}
		if pane.Kind == "claude" {
			for _, pid := range pane.ProcessPIDs {
				id, source := bindingClaudeHint(pid, snap.command[pid])
				if source == "env" {
					if incumbent, ok := tracker.conflictingSticky(pane, id, panes, files, snap); ok {
						logInheritedEnvBindingIncident(id, pane, incumbent)
					}
					id = ""
				}
				if id == "" {
					id = validatedClaudeSessionRegistryHint(pid, files.cwd[pid], snap.started[pid])
					if id != "" {
						source = "registry"
					}
				}
				if id != "" {
					matches[id] = true
				}
			}
		}
		if len(matches) > 0 {
			reservedPanes[i] = true
			if len(matches) == 1 {
				for matchID := range matches {
					if tracker.allowStrong(pane, matchID, files, snap, recoveryID) {
						if bind(pane, matchID, "strong") && pane.Kind == "claude" {
							tracker.rememberStrong(pane, byKey[pane.Kind+"\x00"+matchID], files, snap, now)
						}
					} else {
						reason := "evidence_transition"
						if tracker.isQuarantined(pane.Kind, matchID) {
							reason = "delivery_reconciliation_failed"
							if entry, ok := tracker.quarantinedEntry(pane, byKey[pane.Kind+"\x00"+matchID], files, snap); ok && stableSessionID(entry.SessionID) != recoveryID {
								strongReadOnly[i] = entry
							}
						}
						strongRecordReasons[pane.Kind+"\x00"+matchID] = reason
					}
				}
			} else {
				for matchID := range matches {
					strongRecordReasons[pane.Kind+"\x00"+matchID] = "multi_candidate_ambiguous"
				}
			}
		}
	}
	weakResult := tracker.apply(records, panes, reservedPanes, files, snap, bound, history, bind, now, recoveryID)
	for paneIndex, entry := range strongReadOnly {
		weakResult.readOnly[paneIndex] = entry
	}
	for paneIndex, entry := range weakResult.readOnly {
		if paneIndex < 0 || paneIndex >= len(panes) {
			continue
		}
		key := entry.Kind + "\x00" + entry.SessionID
		record := byKey[key]
		if record == nil || bound[key] {
			continue
		}
		copy := panes[paneIndex]
		record.binding = &copy
		record.bindingEvidence = entry.Evidence
		record.Live = true
		record.CanSend = false
		record.BindingReason = "delivery_reconciliation_failed"
		bound[key] = true
		boundPanes[paneIdentityKey(&panes[paneIndex])] = true
	}
	for _, record := range records {
		for i := range panes {
			cwd, _ := fallbackPaneIdentity(&panes[i], files, snap)
			if panes[i].Kind == record.Kind && cwd != "" && record.cwdHistory[cwd] {
				record.candidatePaneCount++
			}
		}
		if record.binding != nil {
			record.candidatePaneCount = 1
		}
	}
	recentCutoff := now.Add(-5 * time.Minute)
	for _, record := range records {
		record.Live = record.binding != nil || !record.mtime.Before(recentCutoff)
		if record.binding == nil {
			record.CanSend = false
			record.BindingReason = weakResult.recordReasons[record.Kind+"\x00"+record.SessionID]
			if reason := strongRecordReasons[record.Kind+"\x00"+record.SessionID]; reason != "" {
				record.BindingReason = reason
			}
			if record.BindingReason == "" {
				record.BindingReason = "no_pane_candidate"
			}
		}
		setRecordState(record)
	}
	resultRecords := append([]*sessionRecord(nil), records...)
	for i := range panes {
		if boundPanes[paneIdentityKey(&panes[i])] {
			continue
		}
		placeholder := panePlaceholder(&panes[i], files, snap, now)
		placeholder.BindingReason = "no_session_attribution"
		resultRecords = append(resultRecords, placeholder)
	}
	logBindingDecisions(resultRecords)
	return bindingOutcome{bound: len(bound), records: resultRecords}
}

func paneIdentityKey(pane *paneBinding) string {
	return fmt.Sprintf("%s\x00%s\x00%d", pane.Socket, pane.TmuxID, pane.PanePID)
}

func panePlaceholder(pane *paneBinding, files processFiles, snap *procSnapshot, now time.Time) *sessionRecord {
	cwd, started := fallbackPaneIdentity(pane, files, snap)
	if cwd == "" {
		cwd = "未知目录"
	}
	if started.IsZero() {
		started = now
	}
	raw := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", pane.Socket, pane.TmuxID, pane.PanePID, started.UnixNano(), pane.Kind)
	sum := sha256.Sum256([]byte(raw))
	placeholderID := "pane-" + hex.EncodeToString(sum[:6])
	kindTitle := strings.ToUpper(pane.Kind[:1]) + pane.Kind[1:]
	record := &sessionRecord{AgentSession: AgentSession{
		ID: placeholderID, Kind: pane.Kind, Cwd: cwd, Title: filepath.Base(cwd) + " · " + kindTitle,
		SessionID: placeholderID, State: "idle", Live: true, CanSend: false,
		LastActivityAt: started.UTC().Format(time.RFC3339Nano), BindingReason: "no_session_attribution",
	}, mtime: started, cwdHistory: map[string]bool{cwd: true}, candidatePaneCount: 1}
	return record
}

func logBindingDecisions(records []*sessionRecord) {
	bindingDecisionMu.Lock()
	defer bindingDecisionMu.Unlock()
	for _, record := range records {
		if record.binding == nil && !record.Live && record.BindingReason == "no_pane_candidate" {
			continue
		}
		decision := "rejected:" + record.BindingReason
		if record.binding != nil && record.CanSend {
			decision = fmt.Sprintf("bound:%s:%s:%s", record.bindingEvidence, record.binding.Socket, record.binding.TmuxID)
		} else if record.binding != nil {
			decision = fmt.Sprintf("read_only:%s:%s:%s", record.BindingReason, record.binding.Socket, record.binding.TmuxID)
		}
		key := record.Kind + "\x00" + record.SessionID
		if bindingDecisions[key] == decision {
			continue
		}
		bindingDecisions[key] = decision
		log.Printf("binding decision: session=%s kind=%s decision=%s", record.SessionID, record.Kind, decision)
	}
}

func logInheritedEnvBindingIncident(sessionID string, claimant *paneBinding, incumbent stickyBinding) {
	key := "claude\x00" + sessionID
	fingerprint := fmt.Sprintf("%s:%s:%d->%s:%s:%d", incumbent.Socket, incumbent.TmuxID, incumbent.PanePID, claimant.Socket, claimant.TmuxID, claimant.PanePID)
	bindingDecisionMu.Lock()
	defer bindingDecisionMu.Unlock()
	if bindingIncidents[key] == fingerprint {
		return
	}
	bindingIncidents[key] = fingerprint
	log.Printf("BINDING INCIDENT: type=inherited_env_conflict session=%s incumbent_socket=%q incumbent_pane=%s incumbent_pane_pid=%d claimant_socket=%q claimant_pane=%s claimant_pane_pid=%d action=blocked", sessionID, incumbent.Socket, incumbent.TmuxID, incumbent.PanePID, claimant.Socket, claimant.TmuxID, claimant.PanePID)
}

func fallbackRecordForPane(records []*sessionRecord, pane *paneBinding, files processFiles, snap *procSnapshot, bound map[string]bool) *sessionRecord {
	cwd, started := fallbackPaneIdentity(pane, files, snap)
	if pane.Kind != "claude" || cwd == "" || started.IsZero() {
		return nil
	}
	var match *sessionRecord
	for _, record := range records {
		key := record.Kind + "\x00" + record.SessionID
		if record.Kind != "claude" || bound[key] || !record.cwdHistory[cwd] || !record.mtime.After(started) {
			continue
		}
		if match != nil {
			return nil
		}
		match = record
	}
	return match
}

func fallbackPaneIdentity(pane *paneBinding, files processFiles, snap *procSnapshot) (string, time.Time) {
	cwd := ""
	started := time.Time{}
	for _, pid := range pane.ProcessPIDs {
		if cwd == "" && files.cwd[pid] != "" {
			cwd = files.cwd[pid]
		}
		if processStarted := snap.started[pid]; !processStarted.IsZero() && (started.IsZero() || processStarted.Before(started)) {
			started = snap.started[pid]
		}
	}
	return cwd, started
}

func setRecordState(record *sessionRecord) {
	if record.Live {
		if time.Since(record.mtime) < 3*time.Second || record.lastType == "user_message" ||
			record.lastType == "tool_use" || record.lastType == "tool_result" {
			record.State = "running"
		} else if record.lastType == "assistant_message" || record.lastType == "status" {
			record.State = "waiting_input"
		} else {
			record.State = "idle"
		}
	} else {
		record.State = "gone"
	}
}

func visibleSessions() ([]*sessionRecord, bool) {
	visibleCacheMu.Lock()
	defer visibleCacheMu.Unlock()
	if visibleV2Cache != nil {
		cached := make([]*sessionRecord, 0, len(visibleV2Cache))
		for _, record := range visibleV2Cache {
			cached = append(cached, cloneSessionRecordForResponse(record))
		}
		return cached, true
	}
	if visibleCache != nil && time.Since(visibleCacheAt) < 2*time.Second {
		cached := make([]*sessionRecord, 0, len(visibleCache))
		for _, record := range visibleCache {
			cached = append(cached, cloneSessionRecordForResponse(record))
		}
		return cached, true
	}
	records, liveValid := collectAllRecords(true)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	visible := filterVisibleRecords(records, cutoff, closedSessions)
	sort.Slice(visible, func(i, j int) bool {
		if visible[i].mtime.Equal(visible[j].mtime) {
			return visible[i].SessionID < visible[j].SessionID
		}
		return visible[i].mtime.After(visible[j].mtime)
	})
	if !liveValid {
		if visibleCache != nil {
			cached := make([]*sessionRecord, 0, len(visibleCache))
			for _, record := range visibleCache {
				cached = append(cached, cloneSessionRecordForResponse(record))
			}
			return cached, true
		}
		return visible, false
	}
	visibleCache = make([]*sessionRecord, 0, len(visible))
	for _, record := range visible {
		visibleCache = append(visibleCache, cloneSessionRecordForResponse(record))
	}
	visibleCacheAt = time.Now()
	return visible, true
}

func visibleSessionsForRecovery(id string) ([]*sessionRecord, bool) {
	records, liveValid := collectAllRecordsWithRecovery(true, id)
	visible := filterVisibleRecords(records, time.Now().Add(-7*24*time.Hour), closedSessions)
	sort.Slice(visible, func(i, j int) bool {
		if visible[i].mtime.Equal(visible[j].mtime) {
			return visible[i].SessionID < visible[j].SessionID
		}
		return visible[i].mtime.After(visible[j].mtime)
	})
	return visible, liveValid
}

func filterVisibleRecords(records []*sessionRecord, cutoff time.Time, closed *closedSessionStore) []*sessionRecord {
	visible := records[:0]
	for _, record := range records {
		if closed != nil && closed.has(record.SessionID) {
			continue
		}
		if record.Live || !record.mtime.Before(cutoff) {
			visible = append(visible, record)
		}
	}
	return visible
}

func cloneSessionRecordForResponse(record *sessionRecord) *sessionRecord {
	clone := *record
	clone.cwdHistory = nil
	if record.binding != nil {
		binding := *record.binding
		binding.ProcessPIDs = append([]int(nil), record.binding.ProcessPIDs...)
		clone.binding = &binding
	}
	return &clone
}

func publishVisibleSessions(records []*sessionRecord) {
	visibleCacheMu.Lock()
	defer visibleCacheMu.Unlock()
	visibleV2Cache = make([]*sessionRecord, 0, len(records))
	for _, record := range records {
		visibleV2Cache = append(visibleV2Cache, cloneSessionRecordForResponse(record))
	}
}

func bindingStateFresh() bool {
	bindingStateMu.RLock()
	at := bindingStateAt
	bindingStateMu.RUnlock()
	return !at.IsZero() && time.Since(at) <= maxWriteBindingAge
}

func invalidateVisibleCache() {
	visibleCacheMu.Lock()
	visibleV2Cache = nil
	visibleCacheAt = time.Time{}
	visibleCacheMu.Unlock()
}

func findSession(id string, bind bool) *sessionRecord {
	if bind {
		writeBindingMu.Lock()
		defer writeBindingMu.Unlock()
		for attempt := 0; attempt < 2; attempt++ {
			if !bindingStateFresh() {
				invalidateVisibleCache()
			}
			records, _ := visibleSessions()
			if bindingStateFresh() {
				for _, candidate := range records {
					if candidate.ID == id {
						return candidate
					}
				}
				break
			}
		}
	}
	titles := loadCodexTitles()
	for _, file := range discoverPhysicalFiles() {
		if stableSessionID(file.sessionID) != id {
			continue
		}
		if bind && closedSessions.has(file.sessionID) {
			return nil
		}
		record := cachedMetadata(file, titles)
		if record == nil {
			return nil
		}
		record.ID = id
		record.LastActivityAt = record.mtime.UTC().Format(time.RFC3339Nano)
		record.Live = !record.mtime.Before(time.Now().Add(-5 * time.Minute))
		setRecordState(record)
		return record
	}
	return nil
}

func resolveV1WriteSession(store *terminalEntryStore, id string, verify func(v2TerminalEntry) (*paneBinding, *v2WriteError)) *sessionRecord {
	record, entry, attached := store.lookupV1WriteRecord(id)
	if record == nil {
		return nil
	}
	if !attached || !record.CanSend || record.binding == nil {
		if record.BindingReason == "" {
			record.BindingReason = "no_pane_candidate"
		}
		record.CanSend = false
		record.binding = nil
		return record
	}
	binding, writeErr := verify(entry)
	if writeErr != nil {
		record.CanSend = false
		record.binding = nil
		record.BindingReason = writeErr.Code
		return record
	}
	record.binding = binding
	return record
}

func resolveV1StreamState(store *terminalEntryStore, id string) *sessionRecord {
	record, _, _ := store.lookupV1WriteRecord(id)
	return record
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	records, valid := visibleSessions()
	if !valid {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "live session scan unavailable"})
		return
	}
	sessions := make([]AgentSession, 0, len(records))
	for _, record := range records {
		sessions = append(sessions, record.AgentSession)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

func toolSummary(input string) string {
	var value map[string]any
	if json.Unmarshal([]byte(input), &value) == nil {
		if description, ok := value["description"].(string); ok && strings.TrimSpace(description) != "" {
			return truncateRunes(description, 200)
		}
		for _, key := range []string{"command", "query", "path", "file_path", "prompt"} {
			if text, ok := value[key].(string); ok && text != "" {
				return truncateRunes(text, 200)
			}
		}
	}
	return truncateRunes(input, 200)
}

func truncateOutput(text string) (string, bool) {
	const limit = 64 * 1024
	if len(text) <= limit {
		return text, false
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true
}

func slashCommandName(text string) (string, bool) {
	match := commandNamePattern.FindStringSubmatch(text)
	if len(match) != 2 || !commandMessagePattern.MatchString(text) || !commandArgsPattern.MatchString(text) {
		return "", false
	}
	remainder := commandNamePattern.ReplaceAllString(text, "")
	remainder = commandMessagePattern.ReplaceAllString(remainder, "")
	remainder = commandArgsPattern.ReplaceAllString(remainder, "")
	if strings.TrimSpace(remainder) != "" {
		return "", false
	}
	return match[1], true
}

func slashCommandInvocation(text string) (string, bool) {
	name, ok := slashCommandName(text)
	args := commandArgsPattern.FindStringSubmatch(text)
	if !ok || len(args) != 2 {
		return "", false
	}
	invocation := "/" + name
	if value := strings.TrimSpace(args[1]); value != "" {
		invocation += " " + value
	}
	return invocation, true
}

func localCommandOutput(text string) (string, bool) {
	match := localCommandStdoutPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", false
	}
	return strings.TrimSpace(ansiCSIPattern.ReplaceAllString(match[1], "")), true
}

func injectedSkillName(text string) (string, bool) {
	const prefix = "Base directory for this skill: "
	firstLine := text
	if index := strings.IndexByte(firstLine, '\n'); index >= 0 {
		firstLine = firstLine[:index]
	}
	if !strings.HasPrefix(firstLine, prefix) {
		return "", false
	}
	dir := strings.TrimSpace(strings.TrimPrefix(firstLine, prefix))
	name := filepath.Base(filepath.Clean(dir))
	if dir == "" || name == "." || name == string(filepath.Separator) {
		return "", false
	}
	return name, true
}

func timelineFor(record *sessionRecord) ([]TimelineEvent, error) {
	events, _, err := timelineForVersion(record)
	return events, err
}

func timelineVersionForPath(path string) (timelineFileVersion, error) {
	info, err := os.Stat(path)
	if err != nil {
		return timelineFileVersion{}, err
	}
	device, inode := physicalIdentity(info)
	return timelineFileVersion{
		path: path, device: device, inode: inode, size: info.Size(), mtimeNano: info.ModTime().UnixNano(),
	}, nil
}

func cloneTimelineEvents(events []TimelineEvent) []TimelineEvent {
	return append([]TimelineEvent(nil), events...)
}

func timelineForVersion(record *sessionRecord) ([]TimelineEvent, timelineFileVersion, error) {
	version, err := timelineVersionForPath(record.SessionFile)
	if err != nil {
		return nil, timelineFileVersion{}, err
	}
	timelineCacheMu.Lock()
	if entry, ok := timelineCache[record.SessionFile]; ok && entry.version == version {
		events := cloneTimelineEvents(entry.events)
		timelineCacheMu.Unlock()
		return events, version, nil
	}
	timelineCacheMu.Unlock()

	timelineLoadMu.Lock()
	if call := timelineLoads[version]; call != nil {
		done := call.done
		timelineLoadMu.Unlock()
		<-done
		return cloneTimelineEvents(call.events), version, call.err
	}
	timelineCacheMu.Lock()
	if entry, ok := timelineCache[record.SessionFile]; ok && entry.version == version {
		events := cloneTimelineEvents(entry.events)
		timelineCacheMu.Unlock()
		timelineLoadMu.Unlock()
		return events, version, nil
	}
	timelineCacheMu.Unlock()
	call := &timelineLoadCall{done: make(chan struct{})}
	timelineLoads[version] = call
	timelineLoadMu.Unlock()
	if timelineParseHook != nil {
		timelineParseHook(record.SessionFile)
	}
	if record.Kind == "claude" {
		call.events, call.err = claudeTimeline(record.SessionFile)
	} else {
		call.events, call.err = codexTimeline(record.SessionFile)
	}
	if call.err == nil {
		timelineCacheMu.Lock()
		timelineCache[record.SessionFile] = timelineCacheEntry{version: version, events: cloneTimelineEvents(call.events)}
		timelineCacheMu.Unlock()
	}
	timelineLoadMu.Lock()
	delete(timelineLoads, version)
	close(call.done)
	timelineLoadMu.Unlock()
	return cloneTimelineEvents(call.events), version, call.err
}

func claudeTimeline(path string) ([]TimelineEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return claudeTimelineReader(f, 1, nil)
}

func cloneToolNames(names map[string]string) map[string]string {
	clone := make(map[string]string, len(names))
	for id, name := range names {
		clone[id] = name
	}
	return clone
}

func claudeTimelineReader(reader io.Reader, firstLineNo int64, initialToolNames map[string]string) ([]TimelineEvent, error) {
	return claudeTimelineReaderWithContext(reader, firstLineNo, initialToolNames, nil)
}

func claudeTimelineReaderWithContext(reader io.Reader, firstLineNo int64, initialToolNames map[string]string, initialQueued map[string][]time.Time) ([]TimelineEvent, error) {
	events := make([]TimelineEvent, 0)
	toolNames := cloneToolNames(initialToolNames)
	queuedMessages := make(map[string][]time.Time, len(initialQueued))
	for key, timestamps := range initialQueued {
		queuedMessages[key] = append([]time.Time(nil), timestamps...)
	}
	queueMessage := func(key string, queuedAt time.Time) {
		pending := queuedMessages[key]
		for _, existing := range pending {
			if existing.Equal(queuedAt) {
				return
			}
		}
		queuedMessages[key] = append(pending, queuedAt)
	}
	consumeQueuedMessage := func(text, timestamp string) bool {
		key := normalizeSendEcho(text)
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || key == "" {
			return false
		}
		pending := queuedMessages[key]
		for len(pending) > 0 && !at.Before(pending[0]) && at.Sub(pending[0]) > 5*time.Minute {
			pending = pending[1:]
		}
		for index, queuedAt := range pending {
			if at.Before(queuedAt) {
				continue
			}
			pending = append(pending[:index], pending[index+1:]...)
			if len(pending) == 0 {
				delete(queuedMessages, key)
			} else {
				queuedMessages[key] = pending
			}
			return true
		}
		queuedMessages[key] = pending
		return false
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	lineNo := firstLineNo - 1
	for scanner.Scan() {
		lineNo++
		var row struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Subtype   string `json:"subtype"`
			Operation string `json:"operation"`
			Content   string `json:"content"`
			IsMeta    bool   `json:"isMeta"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if row.Type == "queue-operation" {
			if row.Operation == "enqueue" && row.Content != "" {
				events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "user_message", Text: row.Content, Queued: true})
				key := normalizeSendEcho(row.Content)
				if queuedAt, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil && key != "" {
					queueMessage(key, queuedAt)
				}
			}
			continue
		}
		if row.Type == "system" && strings.Contains(row.Subtype, "permission") && row.Content != "" {
			events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "status", Text: row.Content})
			continue
		}
		if row.Type != "user" && row.Type != "assistant" {
			continue
		}
		var direct string
		if json.Unmarshal(row.Message.Content, &direct) == nil {
			if direct != "" {
				if row.Type == "user" {
					if localCommandCaveatPattern.MatchString(direct) {
						continue
					}
					if output, ok := localCommandOutput(direct); ok {
						if output != "" {
							events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "status", Text: output})
						}
						continue
					}
					if name, ok := slashCommandName(direct); ok {
						events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "skill_load", Skill: name, Text: direct})
						continue
					}
					if consumeQueuedMessage(direct, row.Timestamp) {
						continue
					}
				}
				events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: row.Type + "_message", Text: direct})
			}
			continue
		}
		var items []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(row.Message.Content, &items) != nil {
			continue
		}
		for index, item := range items {
			seq := lineNo*1000 + int64(index+1)
			switch item.Type {
			case "text":
				if item.Text != "" {
					if row.Type == "user" && row.IsMeta {
						if name, ok := injectedSkillName(item.Text); ok {
							events = append(events, TimelineEvent{Seq: seq, TS: row.Timestamp, Type: "skill_load", Skill: name, Text: item.Text})
							continue
						}
					}
					if row.Type == "user" && consumeQueuedMessage(item.Text, row.Timestamp) {
						continue
					}
					events = append(events, TimelineEvent{Seq: seq, TS: row.Timestamp, Type: row.Type + "_message", Text: item.Text})
				}
			case "tool_use":
				input := compactJSON(item.Input)
				toolNames[item.ID] = item.Name
				events = append(events, TimelineEvent{Seq: seq, TS: row.Timestamp, Type: "tool_use", Tool: item.Name, Summary: toolSummary(input), Input: input})
			case "tool_result":
				output := parseContentText(item.Content, "text")
				if output == "" {
					output = compactJSON(item.Content)
				}
				output, truncated := truncateOutput(output)
				ok := !item.IsError
				events = append(events, TimelineEvent{Seq: seq, TS: row.Timestamp, Type: "tool_result", Tool: toolNames[item.ToolUseID], OK: &ok, Output: output, Truncated: truncated})
			}
		}
	}
	return events, scanner.Err()
}

func codexTimeline(path string) ([]TimelineEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return codexTimelineReader(f, 1, nil)
}

func codexTimelineReader(reader io.Reader, firstLineNo int64, initialToolNames map[string]string) ([]TimelineEvent, error) {
	events := make([]TimelineEvent, 0)
	toolNames := cloneToolNames(initialToolNames)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	lineNo := firstLineNo - 1
	for scanner.Scan() {
		lineNo++
		var row struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type      string          `json:"type"`
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Input     json.RawMessage `json:"input"`
				Output    json.RawMessage `json:"output"`
				CallID    string          `json:"call_id"`
				Message   string          `json:"message"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if row.Type == "event_msg" && row.Payload.Type == "turn_aborted" {
			text := row.Payload.Message
			if text == "" {
				text = "回合已中止"
			}
			events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "status", Text: text})
			continue
		}
		if row.Type != "response_item" {
			continue
		}
		switch row.Payload.Type {
		case "message":
			if row.Payload.Role != "user" && row.Payload.Role != "assistant" {
				continue
			}
			var items []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(row.Payload.Content, &items) != nil {
				continue
			}
			for index, item := range items {
				if (item.Type == "input_text" || item.Type == "output_text") && item.Text != "" {
					events = append(events, TimelineEvent{Seq: lineNo*1000 + int64(index+1), TS: row.Timestamp, Type: row.Payload.Role + "_message", Text: item.Text})
				}
			}
		case "function_call", "custom_tool_call", "tool_search_call", "web_search_call", "image_generation_call":
			input := compactJSON(row.Payload.Arguments)
			if input == "" {
				input = compactJSON(row.Payload.Input)
			}
			name := row.Payload.Name
			if name == "" {
				name = row.Payload.Type
			}
			toolNames[row.Payload.CallID] = name
			events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "tool_use", Tool: name, Summary: toolSummary(input), Input: input})
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			output := parseContentText(row.Payload.Output, "input_text", "output_text", "text")
			if output == "" {
				output = compactJSON(row.Payload.Output)
			}
			output, truncated := truncateOutput(output)
			ok := true
			events = append(events, TimelineEvent{Seq: lineNo*1000 + 1, TS: row.Timestamp, Type: "tool_result", Tool: toolNames[row.Payload.CallID], OK: &ok, Output: output, Truncated: truncated})
		}
	}
	return events, scanner.Err()
}

func filteredTimeline(events []TimelineEvent, afterSeq int64, hasAfter bool, limit int) []TimelineEvent {
	start := 0
	if hasAfter {
		start = sort.Search(len(events), func(i int) bool { return events[i].Seq > afterSeq })
	} else if len(events) > limit {
		start = len(events) - limit
	}
	end := len(events)
	if hasAfter && end-start > limit {
		end = start + limit
	}
	return events[start:end]
}

func parseTimelineQuery(r *http.Request) (timelineQuery, error) {
	query := timelineQuery{Limit: 200}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			return timelineQuery{}, fmt.Errorf("limit must be 1..1000")
		}
		query.Limit = value
	}
	afterRaw := r.URL.Query().Get("afterSeq")
	beforeRaw := r.URL.Query().Get("beforeSeq")
	if afterRaw != "" && beforeRaw != "" {
		return timelineQuery{}, fmt.Errorf("afterSeq and beforeSeq are mutually exclusive")
	}
	if afterRaw != "" {
		value, err := strconv.ParseInt(afterRaw, 10, 64)
		if err != nil || value < 0 {
			return timelineQuery{}, fmt.Errorf("afterSeq must be a non-negative integer")
		}
		query.AfterSeq, query.HasAfter = value, true
	}
	if beforeRaw != "" {
		value, err := strconv.ParseInt(beforeRaw, 10, 64)
		if err != nil || value < 0 {
			return timelineQuery{}, fmt.Errorf("beforeSeq must be a non-negative integer")
		}
		query.BeforeSeq, query.HasBefore = value, true
	}
	return query, nil
}

func sessionsRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	switch action {
	case "timeline":
		handleTimeline(w, r, id)
	case "stream":
		handleSessionStream(w, r, id)
	case "send":
		handleSessionSend(w, r, id)
	case "upload":
		handleSessionUpload(w, r, id)
	case "kill":
		handleSessionKill(w, r, id)
	case "recover":
		handleSessionRecover(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func handleTimeline(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	query, err := parseTimelineQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	record := findSession(id, false)
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	page, err := timelinePageFor(record, query)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": page.Events, "hasMoreBefore": page.HasMoreBefore, "nextBeforeSeq": page.NextBeforeSeq,
	})
}

func handleSessionSend(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	timingStarted := time.Now()
	requestID := timingStarted.UnixNano()
	log.Printf("v1 send timing: request_id=%d target=%s phase=handler_enter total=0s", requestID, id)
	logResponseTiming := func(status int, result string) {
		log.Printf("v1 send timing: request_id=%d target=%s phase=response_ready status=%d result=%s total=%s", requestID, id, status, result, time.Since(timingStarted))
	}
	var body struct {
		Text string `json:"text"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err := decoder.Decode(&body); err != nil || body.Text == "" {
		logResponseTiming(http.StatusBadRequest, "invalid_request")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "non-empty text is required"})
		return
	}
	findStarted := time.Now()
	record := resolveV1WriteSession(v2Entries, id, defaultV2VerifyEntry)
	findResult := "not_found"
	if record != nil {
		findResult = "found"
	}
	log.Printf("v1 send timing: request_id=%d target=%s phase=find_session_done result=%s duration=%s total=%s", requestID, id, findResult, time.Since(findStarted), time.Since(timingStarted))
	if record == nil {
		logResponseTiming(http.StatusNotFound, "session_not_found")
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if !record.CanSend || record.binding == nil {
		logResponseTiming(http.StatusConflict, "session_not_writable")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not bound to a live tmux pane", "bindingReason": record.BindingReason})
		return
	}
	formatted := formatSessionSendText(record.Kind, body.Text)
	var baseline fileWritePoint
	reconcile := record.Kind == "claude" && record.bindingEvidence != "strong"
	requestBytes, requestSHA256 := len(formatted), fmt.Sprintf("%x", sha256.Sum256([]byte(formatted)))
	if reconcile {
		var err error
		baseline, err = fileWritePointForPath(record.SessionFile)
		if err != nil {
			logResponseTiming(http.StatusConflict, "binding_evidence_stale")
			writeJSON(w, http.StatusConflict, map[string]string{"error": "binding evidence is stale", "bindingReason": "evidence_stale"})
			return
		}
		log.Printf("send reconciliation: session=%s socket=%q pane=%s evidence=%s request_bytes=%d request_sha256=%s outcome=started", record.SessionID, record.binding.Socket, record.binding.TmuxID, record.bindingEvidence, requestBytes, requestSHA256)
	}
	sendStarted := time.Now()
	if err := sendToPane(record.binding, formatted); err != nil {
		log.Printf("v1 send timing: request_id=%d target=%s phase=send_keys_done result=failed duration=%s total=%s", requestID, id, time.Since(sendStarted), time.Since(timingStarted))
		if reconcile {
			log.Printf("send reconciliation: session=%s socket=%q pane=%s evidence=%s request_bytes=%d request_sha256=%s outcome=inject_failed error=%v", record.SessionID, record.binding.Socket, record.binding.TmuxID, record.bindingEvidence, requestBytes, requestSHA256, err)
		}
		logResponseTiming(http.StatusInternalServerError, "send_keys_failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("v1 send timing: request_id=%d target=%s phase=send_keys_done result=ok duration=%s total=%s", requestID, id, time.Since(sendStarted), time.Since(timingStarted))
	if reconcile {
		matchKind, err := waitForClaudeSendReconciliation(record.SessionFile, baseline, formatted, 5*time.Second)
		if err != nil {
			failedBinding := record.binding
			weakBindings.quarantine(failedBinding, record)
			record.binding = nil
			record.CanSend = false
			record.BindingReason = "delivery_reconciliation_failed"
			invalidateSessionCaches(id)
			log.Printf("send reconciliation: session=%s socket=%q pane=%s evidence=%s request_bytes=%d request_sha256=%s outcome=missed error=%v", record.SessionID, failedBinding.Socket, failedBinding.TmuxID, record.bindingEvidence, requestBytes, requestSHA256, err)
			log.Printf("BINDING INCIDENT: session=%s socket=%q pane=%s pane_pid=%d evidence=%s reconciliation=%v; binding quarantined", record.SessionID, failedBinding.Socket, failedBinding.TmuxID, failedBinding.PanePID, record.bindingEvidence, err)
			logResponseTiming(http.StatusBadGateway, "reconciliation_missed")
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "message was injected but did not reconcile to the expected session", "bindingReason": "delivery_reconciliation_failed"})
			return
		}
		log.Printf("send reconciliation: session=%s socket=%q pane=%s evidence=%s request_bytes=%d request_sha256=%s outcome=matched match_kind=%s", record.SessionID, record.binding.Socket, record.binding.TmuxID, record.bindingEvidence, requestBytes, requestSHA256, matchKind)
	}
	logResponseTiming(http.StatusOK, "ok")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func fileWritePointForPath(path string) (fileWritePoint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileWritePoint{}, err
	}
	device, inode := physicalIdentity(info)
	return fileWritePoint{Device: device, Inode: inode, MtimeNano: info.ModTime().UnixNano(), Size: info.Size()}, nil
}

func waitForClaudeSendReconciliation(path string, baseline fileWritePoint, expected string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		current, err := fileWritePointForPath(path)
		if err == nil && sameFileIdentity(baseline, current) && current.Size > baseline.Size {
			file, openErr := os.Open(path)
			if openErr == nil {
				_, seekErr := file.Seek(baseline.Size, io.SeekStart)
				data, readErr := io.ReadAll(io.LimitReader(file, 8*1024*1024))
				_ = file.Close()
				if seekErr == nil && readErr == nil {
					if matchKind := appendedClaudeUserMessageMatch(data, expected); matchKind != "" {
						return matchKind, nil
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("expected user message did not appear in %s within %s", path, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func appendedClaudeUserMessageMatches(data []byte, expected string) bool {
	return appendedClaudeUserMessageMatch(data, expected) != ""
}

func appendedClaudeUserMessageMatch(data []byte, expected string) string {
	want := normalizeSendEcho(expected)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var row struct {
			Type      string `json:"type"`
			Operation string `json:"operation"`
			Content   string `json:"content"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if row.Type == "queue-operation" && row.Operation == "enqueue" {
			actual := normalizeSendEcho(row.Content)
			if actual == want {
				return "queue_operation_enqueue"
			}
			if want != "" && strings.HasSuffix(actual, want) {
				return "queue_operation_enqueue_suffix"
			}
		}
		if row.Type == "user" && row.Message.Role == "user" {
			actual := parseContentText(row.Message.Content, "text")
			if normalizeSendEcho(actual) == want {
				return "user_message"
			}
			if invocation, ok := slashCommandInvocation(actual); ok && normalizeSendEcho(invocation) == want {
				return "slash_command_user"
			}
		}
	}
	return ""
}

func normalizeSendEcho(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
}

func formatSessionSendText(kind, text string) string {
	if kind != "claude" {
		return text
	}
	uploadDir := filepath.Join(homeDir(), "Library", "Caches", "corral-uploads")
	lines := strings.Split(text, "\n")
	formatted := false
	for i, line := range lines {
		clean := filepath.Clean(line)
		if !filepath.IsAbs(line) || clean != line || filepath.Dir(clean) != uploadDir {
			continue
		}
		lines[i] = "@" + line
		formatted = true
	}
	if !formatted {
		return text
	}
	return strings.Join(lines, "\n") + " "
}

func newUploadUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func writeUploadedFile(dir, originalName string, source io.Reader) (string, int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	ext := strings.ToLower(filepath.Ext(originalName))
	if !uploadExtPattern.MatchString(ext) {
		ext = ""
	}
	uuid, err := newUploadUUID()
	if err != nil {
		return "", 0, err
	}
	path := filepath.Join(dir, uuid+ext)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	written, err := io.Copy(file, io.LimitReader(source, maxUploadBytes+1))
	if err != nil {
		return "", written, err
	}
	if written > maxUploadBytes {
		return "", written, errUploadTooLarge
	}
	if err := file.Close(); err != nil {
		return "", written, err
	}
	remove = false
	return path, written, nil
}

func handleSessionUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	record := findSession(id, true)
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if !record.CanSend || record.binding == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not bound to a live tmux pane", "bindingReason": record.BindingReason})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1024*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart file is required"})
		return
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart field file is required"})
			return
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": errUploadTooLarge.Error()})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart body"})
			}
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		dir := filepath.Join(homeDir(), "Library", "Caches", "corral-uploads")
		path, written, writeErr := writeUploadedFile(dir, part.FileName(), part)
		_ = part.Close()
		if writeErr != nil {
			var maxErr *http.MaxBytesError
			if errors.Is(writeErr, errUploadTooLarge) || errors.As(writeErr, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": errUploadTooLarge.Error()})
			} else {
				log.Printf("upload failed: session=%s error=%v", id, writeErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store attachment"})
			}
			return
		}
		log.Printf("upload: session=%s bytes=%d path=%q", id, written, path)
		writeJSON(w, http.StatusOK, map[string]string{"path": path})
		return
	}
}

func invalidateSessionCaches(id string) {
	visibleCacheMu.Lock()
	visibleCache, visibleCacheAt, visibleV2Cache = nil, time.Time{}, nil
	visibleCacheMu.Unlock()
	liveMu.Lock()
	liveValue, liveAt = nil, time.Time{}
	liveMu.Unlock()
	procMu.Lock()
	procValue, procAt = nil, time.Time{}
	procMu.Unlock()
	socketMu.Lock()
	sockets, socketsAt, socketCandidates = nil, time.Time{}, nil
	socketMu.Unlock()
	bindingStateMu.Lock()
	bindingStateAt = time.Time{}
	bindingStateMu.Unlock()
}

func invalidateAllSessionCaches() {
	invalidateSessionCaches("")
	metadataCacheMu.Lock()
	metadataCache = map[string]metadataCacheEntry{}
	metadataCacheMu.Unlock()
	invalidateClaudeHistoryCache()
}

func recoverSessionWithScan(id string, scan func() ([]*sessionRecord, bool)) (*sessionRecord, bool) {
	writeBindingMu.Lock()
	defer writeBindingMu.Unlock()
	invalidateAllSessionCaches()
	records, valid := scan()
	if !valid {
		return nil, false
	}
	for _, record := range records {
		if record.ID == id {
			return record, true
		}
	}
	return nil, true
}

func handleSessionRecover(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	record, valid := recoverSessionWithScan(id, func() ([]*sessionRecord, bool) {
		return visibleSessionsForRecovery(id)
	})
	if !valid {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "live session scan unavailable", "bindingReason": "evidence_stale"})
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found", "bindingReason": "no_session_attribution"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recovered":          record.CanSend,
		"bindingReason":      record.BindingReason,
		"candidatePaneCount": record.candidatePaneCount,
		"evidenceLevel":      record.bindingEvidence,
		"session":            record.AgentSession,
	})
}

func handleSessionKill(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	killMu.Lock()
	defer killMu.Unlock()
	records, valid := visibleSessions()
	if !valid {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "live session scan unavailable"})
		return
	}
	var record *sessionRecord
	for _, candidate := range records {
		if candidate.ID == id {
			record = candidate
			break
		}
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if !record.CanSend || record.binding == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not bound to a live tmux pane", "bindingReason": record.BindingReason})
		return
	}
	pids, err := terminatePane(record.binding)
	if err != nil {
		invalidateSessionCaches(id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := closedSessions.add(record.SessionID); err != nil {
		invalidateSessionCaches(id)
		log.Printf("kill tombstone persist failed: session=%s error=%v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "CLI terminated but closed state could not be persisted"})
		return
	}
	invalidateSessionCaches(id)
	writeJSON(w, http.StatusOK, map[string]any{"killed": true, "pids": pids})
}

func writeSSE(w http.ResponseWriter, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	return err
}

func handleSessionStream(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	record := findSession(id, true)
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	query, err := parseTimelineQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if query.HasBefore {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "beforeSeq is not valid for a stream"})
		return
	}
	after := query.AfterSeq
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_ = writeSSE(w, "state", map[string]any{"state": record.State, "live": record.Live, "canSend": record.CanSend})
	if query.HasAfter {
		if !writeV1TimelineAfter(w, record, &after) {
			return
		}
	} else if page, pageErr := timelinePageFor(record, timelineQuery{Limit: 1}); pageErr == nil && len(page.Events) > 0 {
		// Without an explicit resume cursor, stream only future appends.
		after = page.Events[len(page.Events)-1].Seq
	}
	flusher.Flush()
	lastInfo, _ := os.Stat(record.SessionFile)
	ticker := time.NewTicker(500 * time.Millisecond)
	stateTicker := time.NewTicker(5 * time.Second)
	keepalive := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer stateTicker.Stop()
	defer keepalive.Stop()
	lastState := fmt.Sprintf("%s:%t:%t", record.State, record.Live, record.CanSend)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-stateTicker.C:
			current := resolveV1StreamState(v2Entries, id)
			if current == nil {
				return
			}
			state := fmt.Sprintf("%s:%t:%t", current.State, current.Live, current.CanSend)
			if state != lastState {
				_ = writeSSE(w, "state", map[string]any{"state": current.State, "live": current.Live, "canSend": current.CanSend})
				lastState = state
				flusher.Flush()
			}
		case <-ticker.C:
			info, err := os.Stat(record.SessionFile)
			if err != nil {
				return
			}
			if lastInfo != nil && info.Size() == lastInfo.Size() && info.ModTime().Equal(lastInfo.ModTime()) {
				continue
			}
			lastInfo = info
			if !writeV1TimelineAfter(w, record, &after) {
				return
			}
			flusher.Flush()
		}
	}
}

func writeV1TimelineAfter(w http.ResponseWriter, record *sessionRecord, after *int64) bool {
	for {
		page, err := timelinePageFor(record, timelineQuery{Limit: 1000, HasAfter: true, AfterSeq: *after})
		if err != nil {
			return true
		}
		for _, event := range page.Events {
			if writeSSE(w, "timeline", event) != nil {
				return false
			}
			*after = event.Seq
		}
		if !page.HasMoreAfter || len(page.Events) == 0 {
			return true
		}
	}
}

type truthSource struct {
	PhysicalFiles   int    `json:"physicalFiles"`
	TopLevelFiles   int    `json:"topLevelFiles,omitempty"`
	SubagentFiles   int    `json:"subagentFiles,omitempty"`
	LogicalSessions int    `json:"logicalSessions"`
	DuplicateIDs    int    `json:"duplicateIds"`
	Recent7d        int    `json:"recent7d"`
	LatestMtime     string `json:"latestMtime"`
	LatestFile      string `json:"latestFile"`
}

func sessionTruth() map[string]any {
	home := homeDir()
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	claude := truthSource{}
	claudeCopies := map[string]int{}
	claudeWinners := map[string]physicalFile{}
	_ = filepath.WalkDir(filepath.Join(home, ".claude", "projects"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		claude.PhysicalFiles++
		if hasPathSegment(path, "subagents") {
			claude.SubagentFiles++
			return nil
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !uuidPattern.MatchString(id) {
			return nil
		}
		claude.TopLevelFiles++
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		id = strings.ToLower(id)
		claudeCopies[id]++
		claudeWinners[id] = preferPhysical(claudeWinners[id], physicalFile{path: path, mtime: info.ModTime()})
		return nil
	})
	for id, winner := range claudeWinners {
		claude.LogicalSessions++
		if claudeCopies[id] > 1 {
			claude.DuplicateIDs++
		}
		if !winner.mtime.Before(cutoff) {
			claude.Recent7d++
		}
		if claude.LatestMtime == "" || winner.mtime.After(parseTimeOrZero(claude.LatestMtime)) {
			claude.LatestMtime = winner.mtime.UTC().Format(time.RFC3339Nano)
			claude.LatestFile = winner.path
		}
	}
	codex := truthSource{}
	codexCopies := map[string]int{}
	codexWinners := map[string]physicalFile{}
	for _, root := range []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")} {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			codex.PhysicalFiles++
			if hasPathSegment(path, "subagents") {
				codex.SubagentFiles++
				return nil
			}
			id := sessionIDFromPath(path)
			if id == "" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			codexCopies[id]++
			codexWinners[id] = preferPhysical(codexWinners[id], physicalFile{path: path, mtime: info.ModTime()})
			return nil
		})
	}
	for id, winner := range codexWinners {
		codex.LogicalSessions++
		if codexCopies[id] > 1 {
			codex.DuplicateIDs++
		}
		if !winner.mtime.Before(cutoff) {
			codex.Recent7d++
		}
		if codex.LatestMtime == "" || winner.mtime.After(parseTimeOrZero(codex.LatestMtime)) {
			codex.LatestMtime = winner.mtime.UTC().Format(time.RFC3339Nano)
			codex.LatestFile = winner.path
		}
	}
	return map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"cutoff":      cutoff.UTC().Format(time.RFC3339Nano),
		"claude":      claude,
		"codex":       codex,
	}
}

func parseTimeOrZero(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func handleSessionTruth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, sessionTruth())
}
