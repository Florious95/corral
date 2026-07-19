package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreferPhysical(t *testing.T) {
	mtime := time.Unix(100, 0)
	current := physicalFile{path: "/b.jsonl", mtime: mtime}
	if got := preferPhysical(current, physicalFile{path: "/a.jsonl", mtime: mtime}); got.path != "/a.jsonl" {
		t.Fatalf("equal mtime must choose lexicographically smaller path, got %s", got.path)
	}
	if got := preferPhysical(current, physicalFile{path: "/z.jsonl", mtime: mtime.Add(time.Second)}); got.path != "/z.jsonl" {
		t.Fatalf("newer mtime must win, got %s", got.path)
	}
}

func TestMetadataCacheInvalidatesOnFileIdentityChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := []byte(`{"type":"user","cwd":"/work","message":{"role":"user","content":"hello"}}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	metadataCacheMu.Lock()
	previous := metadataCache
	metadataCache = map[string]metadataCacheEntry{}
	metadataCacheMu.Unlock()
	t.Cleanup(func() {
		metadataCacheMu.Lock()
		metadataCache = previous
		metadataCacheMu.Unlock()
	})
	first := physicalFile{kind: "claude", sessionID: "test", path: path, mtime: info.ModTime(), size: info.Size(), device: 1, inode: 1}
	if record := cachedMetadata(first, nil); record == nil || record.inode != 1 {
		t.Fatalf("first record=%#v", record)
	}
	second := first
	second.inode = 2
	if record := cachedMetadata(second, nil); record == nil || record.inode != 2 {
		t.Fatalf("file identity change must invalidate metadata cache, got %#v", record)
	}
}

func TestClaudeMetadataCacheParsesOnlyAppendedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := []byte(`{"type":"user","cwd":"/work","message":{"role":"user","content":"first"}}` + "\n")
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	var offsets []int64
	metadataParseHook = func(_ string, offset int64) { offsets = append(offsets, offset) }
	t.Cleanup(func() { metadataParseHook = nil })

	file := physicalFileForTest(t, path, "claude", "test")
	if record := cachedMetadata(file, nil); record == nil || record.LastMessagePreview != "first" {
		t.Fatalf("first record=%#v", record)
	}
	second := []byte(`{"type":"assistant","message":{"role":"assistant","content":"second"}}` + "\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(second); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	file = physicalFileForTest(t, path, "claude", "test")
	if record := cachedMetadata(file, nil); record == nil || record.LastMessagePreview != "second" {
		t.Fatalf("appended record=%#v", record)
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != int64(len(first)) {
		t.Fatalf("parse offsets=%v want=[0 %d]", offsets, len(first))
	}
}

func TestClaudeMetadataCacheTruncateTriggersFullParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	initial := []byte(`{"type":"user","cwd":"/a","message":{"role":"user","content":"a long first message"}}` + "\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	var offsets []int64
	metadataParseHook = func(_ string, offset int64) { offsets = append(offsets, offset) }
	t.Cleanup(func() { metadataParseHook = nil })
	_ = cachedMetadata(physicalFileForTest(t, path, "claude", "test"), nil)
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"new"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "test"), nil)
	if record == nil || record.LastMessagePreview != "new" {
		t.Fatalf("truncated record=%#v", record)
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 0 {
		t.Fatalf("parse offsets=%v want=[0 0]", offsets)
	}
}

func TestClaudeMetadataCacheCompletesPartialTailOnAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := []byte(`{"type":"user","message":{"role":"user","content":"first"}}` + "\n" + `{"type":"assistant","message":{"role":"assistant","content":"sec`)
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "test"), nil)
	if record == nil || record.LastMessagePreview != "first" {
		t.Fatalf("partial record=%#v", record)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`ond"}}` + "\n")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	record = cachedMetadata(physicalFileForTest(t, path, "claude", "test"), nil)
	if record == nil || record.LastMessagePreview != "second" {
		t.Fatalf("completed tail record=%#v", record)
	}
}

func TestMetadataCacheConcurrentSameVersionParsesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"one"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	var parses atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	metadataParseHook = func(_ string, _ int64) {
		if parses.Add(1) == 1 {
			close(started)
			<-release
		}
	}
	t.Cleanup(func() { metadataParseHook = nil })
	file := physicalFileForTest(t, path, "claude", "test")
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cachedMetadata(file, nil) == nil {
				t.Error("metadata missing")
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if got := parses.Load(); got != 1 {
		t.Fatalf("physical metadata parses=%d want=1", got)
	}
}

func TestTimelineConcurrentSameVersionParsesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"2026-07-18T00:00:00Z","message":{"role":"user","content":"one"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetTimelineCacheForTest(t)
	var parses atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	timelineParseHook = func(string) {
		if parses.Add(1) == 1 {
			close(started)
			<-release
		}
	}
	t.Cleanup(func() { timelineParseHook = nil })
	record := &sessionRecord{AgentSession: AgentSession{Kind: "claude", SessionFile: path}}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, err := timelineFor(record)
			if err != nil || len(events) != 1 {
				t.Errorf("timeline events=%d err=%v", len(events), err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if got := parses.Load(); got != 1 {
		t.Fatalf("physical timeline parses=%d want=1", got)
	}
}

func TestTimelineCacheReloadsChangedVersionWithoutLossOrDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := []byte(`{"type":"user","timestamp":"2026-07-18T00:00:00Z","message":{"role":"user","content":"one"}}` + "\n")
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	resetTimelineCacheForTest(t)
	var parses atomic.Int32
	timelineParseHook = func(string) { parses.Add(1) }
	t.Cleanup(func() { timelineParseHook = nil })
	record := &sessionRecord{AgentSession: AgentSession{Kind: "claude", SessionFile: path}}
	events, err := timelineFor(record)
	if err != nil || len(events) != 1 || events[0].Seq != 1001 {
		t.Fatalf("first timeline=%#v err=%v", events, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"type":"assistant","timestamp":"2026-07-18T00:00:01Z","message":{"role":"assistant","content":"two"}}` + "\n")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	events, err = timelineFor(record)
	if err != nil || len(events) != 2 || events[0].Seq != 1001 || events[1].Seq != 2001 {
		t.Fatalf("appended timeline=%#v err=%v", events, err)
	}
	if _, err := timelineFor(record); err != nil {
		t.Fatal(err)
	}
	if got := parses.Load(); got != 2 {
		t.Fatalf("physical parses=%d want one per two file versions", got)
	}
}

func TestP0LargeClaudeRecordPerformance(t *testing.T) {
	path := os.Getenv("P0_LARGE_CLAUDE_RECORD")
	if path == "" {
		t.Skip("set P0_LARGE_CLAUDE_RECORD to an isolated large JSONL fixture")
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	file := physicalFileForTest(t, path, "claude", "large-fixture")
	fullStarted := time.Now()
	record := cachedMetadata(file, nil)
	if record == nil {
		t.Fatal("initial large metadata parse failed")
	}
	fullElapsed := time.Since(fullStarted)
	var latestBytes atomic.Int64
	timelinePageReadHook = func(_ string, count int64) { latestBytes.Add(count) }
	pageStarted := time.Now()
	latest, err := timelinePageFor(record, timelineQuery{Limit: 200})
	pageElapsed := time.Since(pageStarted)
	timelinePageReadHook = nil
	if err != nil || len(latest.Events) == 0 || len(latest.Events) > 200 {
		t.Fatalf("large default page events=%d err=%v", len(latest.Events), err)
	}
	if pageElapsed >= time.Second {
		t.Fatalf("large default page=%s want<1s", pageElapsed)
	}
	if got := latestBytes.Load(); got <= 0 || got >= file.size/4 {
		t.Fatalf("large default page bytesRead=%d file=%d", got, file.size)
	}
	latestSeq := latest.Events[len(latest.Events)-1].Seq

	appendRow := []byte(`{"type":"assistant","timestamp":"2026-07-18T00:00:00Z","message":{"role":"assistant","content":"p0 tail marker"}}` + "\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(appendRow); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	file = physicalFileForTest(t, path, "claude", "large-fixture")
	tailStarted := time.Now()
	record = cachedMetadata(file, nil)
	tailElapsed := time.Since(tailStarted)
	if record == nil || record.LastMessagePreview != "p0 tail marker" {
		t.Fatalf("incremental large metadata=%#v", record)
	}
	if tailElapsed >= time.Second {
		t.Fatalf("large metadata append-tail parse=%s want<1s (full=%s)", tailElapsed, fullElapsed)
	}

	var parses atomic.Int32
	var afterBytes atomic.Int64
	timelineParseHook = func(string) { parses.Add(1) }
	timelinePageReadHook = func(_ string, count int64) { afterBytes.Add(count) }
	t.Cleanup(func() {
		timelineParseHook = nil
		timelinePageReadHook = nil
	})
	streamStarted := time.Now()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := timelinePageFor(record, timelineQuery{Limit: 200, HasAfter: true, AfterSeq: latestSeq})
			if err != nil {
				t.Errorf("large timeline: %v", err)
				return
			}
			if len(page.Events) != 1 || page.Events[0].Text != "p0 tail marker" {
				t.Errorf("large append page=%#v", page)
			}
		}()
	}
	wg.Wait()
	streamElapsed := time.Since(streamStarted)
	if got := parses.Load(); got != 1 {
		t.Fatalf("100 subscribers physical timeline parses=%d want=1", got)
	}
	if got := afterBytes.Load(); got <= 0 || got >= file.size/4 {
		t.Fatalf("large append page bytesRead=%d file=%d", got, file.size)
	}
	t.Logf("large fixture bytes=%d metadata_full=%s metadata_tail=%s latest_page=%s latest_bytes=%d append_100=%s append_bytes=%d", file.size, fullElapsed, tailElapsed, pageElapsed, latestBytes.Load(), streamElapsed, afterBytes.Load())
}

func physicalFileForTest(t *testing.T, path, kind, sessionID string) physicalFile {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	device, inode := physicalIdentity(info)
	return physicalFile{kind: kind, sessionID: sessionID, path: path, mtime: info.ModTime(), size: info.Size(), device: device, inode: inode}
}

func resetMetadataCacheForTest(t *testing.T) {
	t.Helper()
	metadataCacheMu.Lock()
	previous := metadataCache
	metadataCache = map[string]metadataCacheEntry{}
	metadataCacheMu.Unlock()
	t.Cleanup(func() {
		metadataCacheMu.Lock()
		metadataCache = previous
		metadataCacheMu.Unlock()
	})
}

func resetTimelineCacheForTest(t *testing.T) {
	t.Helper()
	timelineCacheMu.Lock()
	previous := timelineCache
	timelineCache = map[string]timelineCacheEntry{}
	timelineCacheMu.Unlock()
	t.Cleanup(func() {
		timelineCacheMu.Lock()
		timelineCache = previous
		timelineCacheMu.Unlock()
	})
}

func TestSubagentPathSegment(t *testing.T) {
	if !hasPathSegment("/projects/x/id/subagents/agent-a.jsonl", "subagents") {
		t.Fatal("subagents path segment was not detected")
	}
	if hasPathSegment("/projects/x/not-subagents/file.jsonl", "subagents") {
		t.Fatal("partial segment must not be excluded")
	}
}

func TestClaudeTimelineStableStrictSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"assistant","timestamp":"2026-07-14T00:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"hello"},{"type":"tool_use","id":"one","name":"Bash","input":{"command":"pwd"}},{"type":"tool_use","id":"two","name":"Read","input":{"path":"x"}}]}}
{"type":"user","timestamp":"2026-07-14T00:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"one","content":"ok","is_error":false}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := claudeTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1002, 1003, 1004, 2001}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i := range want {
		if events[i].Seq != want[i] {
			t.Fatalf("event %d seq=%d want=%d", i, events[i].Seq, want[i])
		}
		if i > 0 && events[i].Seq <= events[i-1].Seq {
			t.Fatalf("seq is not strictly increasing: %d then %d", events[i-1].Seq, events[i].Seq)
		}
	}
}

func TestClaudeTimelineClassifiesSkillLoadsWithoutMisclassifyingUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"assistant","timestamp":"2026-07-15T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"skill-1","name":"Skill","input":{"skill":"cloudflare-pages-deploy"}}]}}
{"type":"user","timestamp":"2026-07-15T00:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"skill-1","content":"Launching skill: cloudflare-pages-deploy"}]}}
{"type":"user","isMeta":true,"timestamp":"2026-07-15T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /Users/test/.claude/skills/cloudflare-pages-deploy\n\n# Cloudflare Pages deploy"}]}}
{"type":"user","timestamp":"2026-07-15T00:00:02Z","message":{"role":"user","content":"<command-message>model</command-message>\n<command-name>/model</command-name>\n<command-args></command-args>"}}
{"type":"user","timestamp":"2026-07-15T00:00:03Z","message":{"role":"user","content":"Please explain <command-name>/model</command-name> literally."}}
{"type":"user","timestamp":"2026-07-15T00:00:04Z","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /Users/test/not-a-skill-load\nThis is real user text."}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := claudeTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded[2]; got["type"] != "skill_load" || got["skill"] != "cloudflare-pages-deploy" {
		t.Fatalf("skill body event=%#v", got)
	}
	if got := decoded[3]; got["type"] != "skill_load" || got["skill"] != "model" {
		t.Fatalf("slash command event=%#v", got)
	}
	for _, index := range []int{4, 5} {
		if got := decoded[index]; got["type"] != "user_message" {
			t.Fatalf("real user event %d was misclassified: %#v", index, got)
		}
	}
}

func TestClaudeTimelineClassifiesLocalCommandOutputAsStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"user","isMeta":true,"timestamp":"2026-07-15T16:57:15.990Z","message":{"role":"user","content":"<local-command-caveat>Caveat: local command output follows.</local-command-caveat>"}}
{"type":"user","timestamp":"2026-07-15T16:57:15.989Z","message":{"role":"user","content":"<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args>claude-fable-5</command-args>"}}
{"type":"user","timestamp":"2026-07-15T16:57:15.989Z","message":{"role":"user","content":"<local-command-stdout>Set model to \u001b[1mFable 5\u001b[22m and saved as your default for new sessions</local-command-stdout>"}}
{"type":"user","timestamp":"2026-07-15T16:57:16Z","message":{"role":"user","content":"Please explain <local-command-stdout> literally."}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := claudeTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %#v, want slash command, local command status, and real user", events)
	}
	if got := events[0]; got.Type != "skill_load" || got.Skill != "model" {
		t.Fatalf("slash command event=%#v", got)
	}
	if got := events[1]; got.Type != "status" || got.Text != "Set model to Fable 5 and saved as your default for new sessions" || strings.Contains(got.Text, "\x1b") {
		t.Fatalf("local command output event=%#v", got)
	}
	if got := events[2]; got.Type != "user_message" || got.Text != "Please explain <local-command-stdout> literally." {
		t.Fatalf("real user event=%#v", got)
	}
}

func TestClaudeTimelineRendersQueuedMessagesAndDeduplicatesMaterializedUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-15T00:00:00Z","content":"queued alpha"}
{"type":"queue-operation","operation":"remove","timestamp":"2026-07-15T00:00:01Z","content":"queued alpha"}
{"type":"queue-operation","operation":"dequeue","timestamp":"2026-07-15T00:00:02Z"}
{"type":"user","timestamp":"2026-07-15T00:00:30Z","message":{"role":"user","content":"queued alpha"}}
{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-15T00:01:00Z","content":"queued beta"}
{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-15T00:01:01Z","content":"queued beta"}
{"type":"user","timestamp":"2026-07-15T00:01:30Z","message":{"role":"user","content":"queued beta"}}
{"type":"user","timestamp":"2026-07-15T00:01:31Z","message":{"role":"user","content":"queued beta"}}
{"type":"user","timestamp":"2026-07-15T00:02:00Z","message":{"role":"user","content":"real gamma"}}
{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-15T00:03:00Z","content":"stale delta"}
{"type":"user","timestamp":"2026-07-15T00:08:01Z","message":{"role":"user","content":"stale delta"}}
{"type":"assistant","timestamp":"2026-07-15T00:08:02Z","message":{"role":"assistant","content":"ack"}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := claudeTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 7 {
		t.Fatalf("events=%#v, want queued/real user messages plus assistant", decoded)
	}
	for index, want := range []struct {
		seq    float64
		text   string
		queued bool
	}{
		{1001, "queued alpha", true},
		{5001, "queued beta", true},
		{6001, "queued beta", true},
		{9001, "real gamma", false},
		{10001, "stale delta", true},
		{11001, "stale delta", false},
	} {
		got := decoded[index]
		if got["seq"] != want.seq || got["type"] != "user_message" || got["text"] != want.text {
			t.Fatalf("event %d=%#v", index, got)
		}
		if queued, _ := got["queued"].(bool); queued != want.queued {
			t.Fatalf("event %d queued=%v, want %v", index, queued, want.queued)
		}
	}
	if got := decoded[6]; got["seq"] != float64(12001) || got["type"] != "assistant_message" {
		t.Fatalf("assistant event=%#v", got)
	}
}

func TestClosedSessionTombstonePersistsAndFiltersOnlyThatSessionID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed-sessions.json")
	closedID := "11111111-1111-1111-1111-111111111111"
	newID := "22222222-2222-2222-2222-222222222222"
	store := newClosedSessionStore(path)
	if err := store.add(closedID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	records := []*sessionRecord{
		{AgentSession: AgentSession{SessionID: closedID, Cwd: "/work"}, mtime: now},
		{AgentSession: AgentSession{SessionID: newID, Cwd: "/work"}, mtime: now},
	}
	visible := filterVisibleRecords(records, now.Add(-7*24*time.Hour), store)
	if len(visible) != 1 || visible[0].SessionID != newID {
		t.Fatalf("visible sessions=%#v, want only new session", visible)
	}
	reloaded := newClosedSessionStore(path)
	if !reloaded.has(closedID) || reloaded.has(newID) {
		t.Fatalf("reloaded tombstones: closed=%v new=%v", reloaded.has(closedID), reloaded.has(newID))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
}

func TestMatchAgentKindUsesExecutablePrefix(t *testing.T) {
	command := "node /opt/homebrew/bin/codex --model x -c developer_instructions=read /Users/a/.claude/projects"
	if got := matchAgentKind(command); got != "codex" {
		t.Fatalf("got %q, want codex", got)
	}
}

func TestClassifyPaneAgentIgnoresInheritedClaudeEnvironmentOnCodexLauncher(t *testing.T) {
	snap := &procSnapshot{
		command: map[int]string{
			10: "sh -lc cd /work && ai_agent=claude-code_2-1-207_agent claude_code_session_id=77777777-7777-4777-8777-777777777777 codex resume 99999999-9999-4999-8999-999999999999",
			11: "node /opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js resume 99999999-9999-4999-8999-999999999999",
			12: "/opt/homebrew/lib/node_modules/@openai/codex/vendor/codex resume 99999999-9999-4999-8999-999999999999",
		},
		children: map[int][]int{10: {11}, 11: {12}},
	}
	kind, pids := classifyPaneAgent(snap, 10)
	if kind != "codex" || fmt.Sprint(pids) != "[11 12]" {
		t.Fatalf("kind=%q pids=%v, want codex [11 12]", kind, pids)
	}
}

func TestClassifyPaneAgentRejectsSameDepthProviderAmbiguity(t *testing.T) {
	snap := &procSnapshot{
		command:  map[int]string{10: "sh -lc agents", 11: "claude --resume x", 12: "codex resume x"},
		children: map[int][]int{10: {11, 12}},
	}
	if kind, pids := classifyPaneAgent(snap, 10); kind != "" || len(pids) != 0 {
		t.Fatalf("ambiguous pane must fail closed, got kind=%q pids=%v", kind, pids)
	}
}

func TestValidEmptyPaneScanIsSuccessfulSnapshot(t *testing.T) {
	if livePaneScanIncomplete(nil, true) {
		t.Fatal("valid scan with zero agent panes must remain readable")
	}
	if !livePaneScanIncomplete(nil, false) {
		t.Fatal("failed pane enumeration must remain fail closed")
	}
}

func TestToolSummaryPrefersDescription(t *testing.T) {
	input := `{"command":"go test ./...","description":"验证代码可编译"}`
	if got := toolSummary(input); got != "验证代码可编译" {
		t.Fatalf("got %q, want description", got)
	}
	if got := toolSummary(`{"command":"go test ./..."}`); got != "go test ./..." {
		t.Fatalf("got %q, want command fallback", got)
	}
}

func TestClaudeMetadataKeepsFirstCwd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"user","cwd":"/work/project","message":{"role":"user","content":"first"}}
{"type":"assistant","cwd":"/work/project/web","message":{"role":"assistant","content":"later"}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	record := parseClaudeMetadata(physicalFile{kind: "claude", sessionID: "test", path: path})
	if record == nil || record.Cwd != "/work/project" {
		t.Fatalf("cwd=%q, want first cwd", record.Cwd)
	}
	if !record.cwdHistory["/work/project"] || !record.cwdHistory["/work/project/web"] {
		t.Fatalf("cwd history must retain binding hints, got %#v", record.cwdHistory)
	}
}

func TestCodexMetadataUsesSessionMetaCwd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"type":"session_meta","payload":{"id":"test","cwd":"/work/project"}}
{"type":"turn_context","payload":{"cwd":"/work/project/web","model":"gpt-test"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	record := parseCodexMetadata(physicalFile{kind: "codex", sessionID: "test", path: path}, nil)
	if record == nil || record.Cwd != "/work/project" {
		t.Fatalf("cwd=%q, want session_meta cwd", record.Cwd)
	}
	if record.cwdHistory["/work/project/web"] {
		t.Fatalf("turn_context cwd must not become session cwd history: %#v", record.cwdHistory)
	}
}

func TestCodexTitlePriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"type":"session_meta","payload":{"id":"test","cwd":"/work"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n\n<INSTRUCTIONS>hidden</INSTRUCTIONS>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"real question"}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	file := physicalFile{kind: "codex", sessionID: "test", path: path}
	fallback := parseCodexMetadata(file, nil)
	if fallback.Title != "real question" {
		t.Fatalf("fallback title=%q, want first non-injected user message", fallback.Title)
	}
	applyCodexWindowTitle(fallback, &paneBinding{WindowName: "collector"})
	if fallback.Title != "collector" {
		t.Fatalf("bound title=%q, want window name", fallback.Title)
	}
	indexed := parseCodexMetadata(file, map[string]string{"test": "你的名字"})
	applyCodexWindowTitle(indexed, &paneBinding{WindowName: "collector"})
	if indexed.Title != "你的名字" {
		t.Fatalf("indexed title=%q, window name must not override thread_name", indexed.Title)
	}
}

func TestValidCodexWindowName(t *testing.T) {
	for _, name := range []string{"", "123", "zsh", "BASH", "node", "codex"} {
		if validCodexWindowName(name) {
			t.Fatalf("%q must be rejected", name)
		}
	}
	for _, name := range []string{"collector", "qa", "你的名字"} {
		if !validCodexWindowName(name) {
			t.Fatalf("%q must be accepted", name)
		}
	}
}

func TestWriteUploadedFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	path, written, err := writeUploadedFile(dir, "photo.HEIC", strings.NewReader("image"))
	if err != nil {
		t.Fatal(err)
	}
	if written != 5 || filepath.Ext(path) != ".heic" || !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		t.Fatalf("path=%q written=%d", path, written)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "image" {
		t.Fatalf("data=%q error=%v", data, err)
	}
}

func TestFormatSessionSendTextPrefixesClaudeUploadPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	uploadDir := filepath.Join(home, "Library", "Caches", "corral-uploads")
	first := filepath.Join(uploadDir, "11111111-1111-4111-8111-111111111111.png")
	second := filepath.Join(uploadDir, "22222222-2222-4222-8222-222222222222.jpg")
	input := "caption\n" + first + "\n" + second
	want := "caption\n@" + first + "\n@" + second + " "
	if got := formatSessionSendText("claude", input); got != want {
		t.Fatalf("formatted text=%q, want %q", got, want)
	}
}

func TestFormatSessionSendTextLeavesCodexAndOrdinaryPathsUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	upload := filepath.Join(home, "Library", "Caches", "corral-uploads", "11111111-1111-4111-8111-111111111111.png")
	ordinary := filepath.Join(home, "Documents", "photo.png")
	input := "caption\n" + upload
	if got := formatSessionSendText("codex", input); got != input {
		t.Fatalf("Codex text changed: %q", got)
	}
	if got := formatSessionSendText("claude", "caption\n"+ordinary); got != "caption\n"+ordinary {
		t.Fatalf("ordinary path changed: %q", got)
	}
}

func TestWriteUploadedFileRejectsMoreThan20MB(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	_, _, err := writeUploadedFile(dir, "large.bin", bytes.NewReader(make([]byte, maxUploadBytes+1)))
	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("error=%v, want errUploadTooLarge", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized upload left files: %v", entries)
	}
}

func TestParseElapsed(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"01:02":      time.Minute + 2*time.Second,
		"03:04:05":   3*time.Hour + 4*time.Minute + 5*time.Second,
		"2-03:04:05": 51*time.Hour + 4*time.Minute + 5*time.Second,
	} {
		got, err := parseElapsed(input)
		if err != nil || got != want {
			t.Fatalf("parseElapsed(%q)=(%s,%v), want %s", input, got, err, want)
		}
	}
}

func TestParseProcessStartedPrefersStableLstart(t *testing.T) {
	fields := strings.Fields("73776 73773 Tue Jul 14 02:41:08 2026 13:54:25 claude --dangerously-skip-permissions")
	first := parseProcessStarted(fields, time.Now().Truncate(time.Second))
	second := parseProcessStarted(fields, time.Now().Add(time.Second).Truncate(time.Second))
	if first.IsZero() || !first.Equal(second) || first.Hour() != 2 || first.Minute() != 41 || first.Second() != 8 {
		t.Fatalf("lstart must be stable across scan boundaries: first=%s second=%s", first, second)
	}
}

func TestFallbackBindingOnlyForClaudeWrittenAfterStart(t *testing.T) {
	started := time.Unix(100, 0)
	records := []*sessionRecord{
		{AgentSession: AgentSession{Kind: "claude", SessionID: "old"}, mtime: started.Add(-time.Second), cwdHistory: map[string]bool{"/work": true}},
		{AgentSession: AgentSession{Kind: "claude", SessionID: "new"}, mtime: started.Add(time.Second), cwdHistory: map[string]bool{"/work": true}},
	}
	files := processFiles{cwd: map[int]string{10: "/work"}, open: map[int][]string{}, valid: true}
	snap := &procSnapshot{started: map[int]time.Time{10: started}}
	pane := &paneBinding{Kind: "claude", ProcessPIDs: []int{10}}
	if got := fallbackRecordForPane(records, pane, files, snap, map[string]bool{}); got == nil || got.SessionID != "new" {
		t.Fatalf("got %#v, want new Claude session", got)
	}
	pane.Kind = "codex"
	if got := fallbackRecordForPane(records, pane, files, snap, map[string]bool{}); got != nil {
		t.Fatalf("Codex cwd fallback must be disabled, got %#v", got)
	}
}

func TestFallbackPaneIdentityUsesStableRootProcessStart(t *testing.T) {
	rootStarted := time.Now().Add(-time.Hour)
	childStarted := time.Now().Add(-time.Second)
	pane := &paneBinding{Kind: "claude", ProcessPIDs: []int{10, 11}}
	files := processFiles{cwd: map[int]string{10: "/work", 11: "/work"}}
	snap := &procSnapshot{started: map[int]time.Time{10: rootStarted, 11: childStarted}}
	_, got := fallbackPaneIdentity(pane, files, snap)
	if !got.Equal(rootStarted) {
		t.Fatalf("process start=%s, want stable root start %s", got, rootStarted)
	}
}

func TestExplicitBindingIsReservedBeforeFallback(t *testing.T) {
	started := time.Now().Add(-10 * time.Second)
	fallbackSessionID := "11111111-1111-1111-1111-111111111111"
	explicitSessionID := "22222222-2222-2222-2222-222222222222"
	records := []*sessionRecord{
		{AgentSession: AgentSession{ID: "fallback-id", Kind: "claude", SessionID: fallbackSessionID}, mtime: started.Add(time.Second), cwdHistory: map[string]bool{"/work": true}},
		{AgentSession: AgentSession{ID: "explicit-id", Kind: "claude", SessionID: explicitSessionID}, mtime: started.Add(2 * time.Second), cwdHistory: map[string]bool{"/work": true}},
	}
	inspection := liveInspection{
		panes: []paneBinding{
			{TmuxID: "%fallback", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}},
			{TmuxID: "%explicit", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}},
		},
		files: processFiles{
			cwd:   map[int]string{10: "/work", 20: "/work"},
			open:  map[int][]string{},
			valid: true,
		},
		snap:  &procSnapshot{command: map[int]string{20: "claude --session-id " + explicitSessionID}, started: map[int]time.Time{10: started, 20: started}, valid: true},
		valid: true,
	}
	if got := bindInspection(records, inspection); got != 2 {
		t.Fatalf("bound=%d, want 2", got)
	}
	if records[0].binding == nil || records[0].binding.TmuxID != "%fallback" {
		t.Fatalf("fallback session binding=%#v", records[0].binding)
	}
	if records[1].binding == nil || records[1].binding.TmuxID != "%explicit" {
		t.Fatalf("explicit session binding=%#v", records[1].binding)
	}
}

func TestStrongClaudePaneIsExcludedFromFallbackWhenOwnSessionIsNotLoaded(t *testing.T) {
	started := time.Now().Add(-10 * time.Second)
	targetSessionID := "11111111-1111-1111-1111-111111111111"
	strongSessionID := "22222222-2222-2222-2222-222222222222"
	inspection := liveInspection{
		panes: []paneBinding{
			{TmuxID: "%51", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}},
			{TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}},
		},
		files: processFiles{
			cwd:   map[int]string{10: "/work", 20: "/work"},
			open:  map[int][]string{},
			valid: true,
		},
		snap: &procSnapshot{
			command: map[int]string{20: "claude --session-id " + strongSessionID},
			started: map[int]time.Time{10: started, 20: started},
			valid:   true,
		},
		valid: true,
	}
	for attempt := 0; attempt < 5; attempt++ {
		records := []*sessionRecord{
			{AgentSession: AgentSession{ID: "target-id", Kind: "claude", SessionID: targetSessionID}, mtime: started.Add(time.Second), cwdHistory: map[string]bool{"/work": true}},
		}
		if got := bindInspection(records, inspection); got != 1 {
			t.Fatalf("attempt %d: bound=%d, want 1", attempt, got)
		}
		if records[0].binding == nil || records[0].binding.TmuxID != "%0" {
			t.Fatalf("attempt %d: target binding=%#v, strong pane %%51 must be excluded from cwd fallback", attempt, records[0].binding)
		}
	}
}

func TestAmbiguousClaudeCwdFallbackDoesNotBind(t *testing.T) {
	started := time.Unix(100, 0)
	records := []*sessionRecord{
		{AgentSession: AgentSession{ID: "first-id", Kind: "claude", SessionID: "11111111-1111-1111-1111-111111111111"}, mtime: started.Add(time.Second), cwdHistory: map[string]bool{"/work": true}},
		{AgentSession: AgentSession{ID: "second-id", Kind: "claude", SessionID: "22222222-2222-2222-2222-222222222222"}, mtime: started.Add(2 * time.Second), cwdHistory: map[string]bool{"/work": true}},
	}
	inspection := liveInspection{
		panes: []paneBinding{
			{TmuxID: "%0", Kind: "claude", ProcessPIDs: []int{10}},
			{TmuxID: "%1", Kind: "claude", ProcessPIDs: []int{20}},
		},
		files: processFiles{
			cwd:   map[int]string{10: "/work", 20: "/work"},
			open:  map[int][]string{},
			valid: true,
		},
		snap:  &procSnapshot{command: map[int]string{}, started: map[int]time.Time{10: started, 20: started}, valid: true},
		valid: true,
	}
	if got := bindInspection(records, inspection); got != 0 {
		t.Fatalf("bound=%d, want 0 for ambiguous cwd fallback", got)
	}
	for _, record := range records {
		if record.binding != nil || record.CanSend {
			t.Fatalf("ambiguous record must stay read-only: %#v", record)
		}
	}
}

func TestRecentUnboundSessionsStayLiveReadOnly(t *testing.T) {
	now := time.Now()
	records := []*sessionRecord{
		{AgentSession: AgentSession{ID: "recent-assistant", Kind: "claude", SessionID: "11111111-1111-1111-1111-111111111111"}, mtime: now.Add(-4 * time.Second), cwdHistory: map[string]bool{"/work": true}, lastType: "assistant_message"},
		{AgentSession: AgentSession{ID: "recent-user", Kind: "claude", SessionID: "22222222-2222-2222-2222-222222222222"}, mtime: now.Add(-5 * time.Second), cwdHistory: map[string]bool{"/work": true}, lastType: "user_message"},
		{AgentSession: AgentSession{ID: "old", Kind: "claude", SessionID: "33333333-3333-3333-3333-333333333333"}, mtime: now.Add(-6 * time.Minute), cwdHistory: map[string]bool{"/work": true}, lastType: "assistant_message"},
	}
	inspection := liveInspection{
		panes: []paneBinding{{TmuxID: "%weak", Kind: "claude", ProcessPIDs: []int{10}}},
		files: processFiles{cwd: map[int]string{10: "/work"}, open: map[int][]string{}, valid: true},
		snap:  &procSnapshot{command: map[int]string{}, started: map[int]time.Time{10: now.Add(-10 * time.Minute)}, valid: true},
		valid: true,
	}
	if got := bindInspection(records, inspection); got != 0 {
		t.Fatalf("bound=%d, want 0 for ambiguous cwd fallback", got)
	}
	for _, record := range records[:2] {
		if !record.Live || record.CanSend || record.State == "gone" {
			t.Fatalf("recent unbound session must stay live read-only: %#v", record)
		}
	}
	if records[0].State != "waiting_input" || records[1].State != "running" {
		t.Fatalf("recent states must follow record content: assistant=%q user=%q", records[0].State, records[1].State)
	}
	if records[2].Live || records[2].CanSend || records[2].State != "gone" {
		t.Fatalf("old unbound session must stay gone: %#v", records[2])
	}
}

func TestVisibleSessionsUsesPublishedV2SnapshotUntilInvalidated(t *testing.T) {
	visibleCacheMu.Lock()
	oldVisible, oldVisibleAt, oldV2 := visibleCache, visibleCacheAt, visibleV2Cache
	visibleCache = []*sessionRecord{{AgentSession: AgentSession{ID: "legacy"}}}
	visibleCacheAt = time.Now()
	visibleV2Cache = nil
	visibleCacheMu.Unlock()
	t.Cleanup(func() {
		visibleCacheMu.Lock()
		visibleCache, visibleCacheAt, visibleV2Cache = oldVisible, oldVisibleAt, oldV2
		visibleCacheMu.Unlock()
	})

	publishVisibleSessions([]*sessionRecord{{AgentSession: AgentSession{ID: "v2-first", SessionID: "session-first"}}})
	if records, valid := visibleSessions(); !valid || len(records) != 1 || records[0].ID != "v2-first" {
		t.Fatalf("published v2 cache=(%#v,%v)", records, valid)
	}
	publishVisibleSessions([]*sessionRecord{{AgentSession: AgentSession{ID: "v2-next", SessionID: "session-next"}}})
	if records, valid := visibleSessions(); !valid || len(records) != 1 || records[0].ID != "v2-next" {
		t.Fatalf("replacement v2 cache=(%#v,%v)", records, valid)
	}

	invalidateVisibleCache()
	visibleCacheMu.Lock()
	if visibleV2Cache != nil {
		visibleCacheMu.Unlock()
		t.Fatal("invalidated v2 cache remained published")
	}
	visibleCache = []*sessionRecord{{AgentSession: AgentSession{ID: "legacy-after-invalidate"}}}
	visibleCacheAt = time.Now()
	visibleCacheMu.Unlock()
	if records, valid := visibleSessions(); !valid || len(records) != 1 || records[0].ID != "legacy-after-invalidate" {
		t.Fatalf("post-invalidate cache=(%#v,%v)", records, valid)
	}
}

func TestFindSessionKeepsFullScanAmbiguityReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessionID := "33333333-3333-3333-3333-333333333333"
	id := stableSessionID(sessionID)
	path := filepath.Join(home, ".claude", "projects", "work", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/work","message":{"role":"user","content":"target"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	visibleCacheMu.Lock()
	previousVisible, previousVisibleAt := visibleCache, visibleCacheAt
	visibleCache = []*sessionRecord{{AgentSession: AgentSession{ID: id, Kind: "claude", SessionID: sessionID, CanSend: false}}}
	visibleCacheAt = time.Now()
	visibleCacheMu.Unlock()
	liveMu.Lock()
	previousLive, previousLiveAt := liveValue, liveAt
	liveValue = &liveInspection{
		panes: []paneBinding{{TmuxID: "%weak", Kind: "claude", ProcessPIDs: []int{10}}},
		files: processFiles{cwd: map[int]string{10: "/work"}, open: map[int][]string{}, valid: true},
		snap:  &procSnapshot{command: map[int]string{}, started: map[int]time.Time{10: time.Now().Add(-time.Second)}, valid: true},
		valid: true,
	}
	liveAt = time.Now()
	liveMu.Unlock()
	t.Cleanup(func() {
		visibleCacheMu.Lock()
		visibleCache, visibleCacheAt = previousVisible, previousVisibleAt
		visibleCacheMu.Unlock()
		liveMu.Lock()
		liveValue, liveAt = previousLive, previousLiveAt
		liveMu.Unlock()
	})

	record := findSession(id, true)
	if record == nil {
		t.Fatal("session not found")
	}
	if record.CanSend || record.binding != nil {
		t.Fatalf("full scan marked the session ambiguous; single-id lookup must stay read-only: %#v", record)
	}
}

func testV1WriteLookupStore(record *sessionRecord) *terminalEntryStore {
	store := newTerminalEntryStore("v1-write")
	entry := testV2Entry("entry-target")
	entry.runtime = &v2EntryRuntime{
		Identity: v2EntryIdentity{HostID: "host", SocketPath: "/tmux", PaneID: "%0", AgentPID: 10},
		Binding:  *record.binding,
	}
	v2Record := v2RecordFromSession(record)
	store.commit(v2SnapshotInput{
		Host: v2Host{ID: "host"},
		Entries: []v2EntryDraft{{
			Entry: entry, Record: &v2Record, BoundRecord: record, EvidenceRank: 3,
		}},
	})
	return store
}

func testV1WritableRecord(id string) *sessionRecord {
	binding := &paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	return &sessionRecord{
		AgentSession: AgentSession{
			ID: id, SessionID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Kind: "claude",
			Cwd: "/work", Title: "target", SessionFile: "/work/target.jsonl",
			Live: true, CanSend: true, State: "waiting_input",
		},
		binding: binding, bindingEvidence: "strong",
	}
}

func TestV1WriteLookupDoesNotWaitForVisibleFullScan(t *testing.T) {
	record := testV1WritableRecord("record-target")
	largePath := filepath.Join(t.TempDir(), "large-session.jsonl")
	file, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(512 << 20); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	record.SessionFile = largePath
	store := testV1WriteLookupStore(record)
	verified := &paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}

	visibleCacheMu.Lock()
	done := make(chan *sessionRecord, 1)
	started := time.Now()
	go func() {
		done <- resolveV1WriteSession(store, record.ID, func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			return verified, nil
		})
	}()
	select {
	case got := <-done:
		visibleCacheMu.Unlock()
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatalf("write lookup took %s while full scan lock was held", elapsed)
		}
		if got == nil || !got.CanSend || got.binding == nil || got.binding.Socket != verified.Socket || got.binding.TmuxID != verified.TmuxID {
			t.Fatalf("write lookup=%#v binding=%#v", got, got.binding)
		}
	case <-time.After(time.Second):
		visibleCacheMu.Unlock()
		<-done
		t.Fatal("write lookup waited for visibleCacheMu/full scan")
	}
}

func TestV1StreamStateLookupDoesNotWaitForVisibleFullScan(t *testing.T) {
	record := testV1WritableRecord("record-target")
	store := testV1WriteLookupStore(record)
	visibleCacheMu.Lock()
	done := make(chan *sessionRecord, 1)
	started := time.Now()
	go func() { done <- resolveV1StreamState(store, record.ID) }()
	select {
	case got := <-done:
		visibleCacheMu.Unlock()
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatalf("stream state lookup took %s while full scan lock was held", elapsed)
		}
		if got == nil || !got.CanSend || got.State != "waiting_input" {
			t.Fatalf("stream state lookup=%#v", got)
		}
	case <-time.After(time.Second):
		visibleCacheMu.Unlock()
		<-done
		t.Fatal("stream state lookup waited for visibleCacheMu/full scan")
	}
}

func TestV1WriteLookupRejectsStaleEntryIdentity(t *testing.T) {
	record := testV1WritableRecord("record-target")
	got := resolveV1WriteSession(testV1WriteLookupStore(record), record.ID, func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
		return nil, &v2WriteError{Status: 410, Code: "entry_gone", Message: "generation exited"}
	})
	if got == nil || got.CanSend || got.binding != nil || got.BindingReason != "entry_gone" {
		t.Fatalf("stale identity must fail closed: %#v", got)
	}
}

func TestV1WriteLookupKeepsAmbiguousAttachmentReadOnly(t *testing.T) {
	record := testV1WritableRecord("record-target")
	record.CanSend = false
	record.BindingReason = "multi_candidate_ambiguous"
	called := false
	got := resolveV1WriteSession(testV1WriteLookupStore(record), record.ID, func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
		called = true
		return record.binding, nil
	})
	if got == nil || got.CanSend || got.BindingReason != "multi_candidate_ambiguous" || called {
		t.Fatalf("ambiguous record was upgraded: record=%#v verifyCalled=%v", got, called)
	}
}

func TestV1WriteHistoryLookupDoesNotStartFullScan(t *testing.T) {
	store := newTerminalEntryStore("v1-history")
	history := testV2Record("history-target")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, History: []v2HistoryRecord{history}})

	visibleCacheMu.Lock()
	done := make(chan *sessionRecord, 1)
	go func() {
		done <- resolveV1WriteSession(store, history.RecordID, func(v2TerminalEntry) (*paneBinding, *v2WriteError) {
			t.Error("history record must not run pane identity verification")
			return nil, nil
		})
	}()
	select {
	case got := <-done:
		visibleCacheMu.Unlock()
		if got == nil || got.CanSend || got.binding != nil || got.BindingReason != "no_pane_candidate" {
			t.Fatalf("history lookup=%#v", got)
		}
	case <-time.After(time.Second):
		visibleCacheMu.Unlock()
		<-done
		t.Fatal("history lookup waited for visibleCacheMu/full scan")
	}
}

func TestProcessTreePIDsChildrenBeforeParents(t *testing.T) {
	snap := &procSnapshot{children: map[int][]int{10: {11, 12}, 11: {13}}}
	got := processTreePIDs(snap, []int{10, 11})
	want := []int{13, 11, 12, 10}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSignalProcessesTerminatesChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	if err := signalProcesses([]int{cmd.Process.Pid}, 15); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit after SIGTERM")
	}
}

func TestPaneExistsOnIsolatedTmuxServer(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not available in test PATH")
	}
	socket := fmt.Sprintf("/private/tmp/corral-pane-%d-%d", os.Getpid(), time.Now().UnixNano())
	defer os.Remove(socket)
	if err := exec.Command(tmuxPath, "-S", socket, "new-session", "-d", "-s", "pane-test", "sleep", "30").Run(); err != nil {
		t.Fatal(err)
	}
	defer exec.Command(tmuxPath, "-S", socket, "kill-server").Run()
	output, err := exec.Command(tmuxPath, "-S", socket, "list-panes", "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatal(err)
	}
	binding := &paneBinding{Socket: socket, TmuxID: strings.TrimSpace(string(output))}
	if exists, confirmed := paneExists(binding); !exists || !confirmed {
		t.Fatalf("live pane exists=%v confirmed=%v", exists, confirmed)
	}
	if err := exec.Command(tmuxPath, "-S", socket, "kill-pane", "-t", binding.TmuxID).Run(); err != nil {
		t.Fatal(err)
	}
	if exists, confirmed := paneExists(binding); exists || !confirmed {
		t.Fatalf("gone pane exists=%v confirmed=%v", exists, confirmed)
	}
}

func TestRecoverSessionBypassesAllScanCaches(t *testing.T) {
	visibleCacheMu.Lock()
	oldVisible, oldVisibleAt := visibleCache, visibleCacheAt
	visibleCache = []*sessionRecord{{AgentSession: AgentSession{ID: "stale"}}}
	visibleCacheAt = time.Now()
	visibleCacheMu.Unlock()
	liveMu.Lock()
	oldLive, oldLiveAt := liveValue, liveAt
	liveValue = &liveInspection{valid: true}
	liveAt = time.Now()
	liveMu.Unlock()
	procMu.Lock()
	oldProc, oldProcAt := procValue, procAt
	procValue = &procSnapshot{valid: true}
	procAt = time.Now()
	procMu.Unlock()
	socketMu.Lock()
	oldSockets, oldSocketsAt, oldSocketCandidates := sockets, socketsAt, socketCandidates
	sockets = []string{"stale"}
	socketsAt = time.Now()
	socketCandidates = []string{"stale"}
	socketMu.Unlock()
	bindingStateMu.Lock()
	oldBindingStateAt := bindingStateAt
	bindingStateAt = time.Now()
	bindingStateMu.Unlock()
	t.Cleanup(func() {
		visibleCacheMu.Lock()
		visibleCache, visibleCacheAt = oldVisible, oldVisibleAt
		visibleCacheMu.Unlock()
		liveMu.Lock()
		liveValue, liveAt = oldLive, oldLiveAt
		liveMu.Unlock()
		procMu.Lock()
		procValue, procAt = oldProc, oldProcAt
		procMu.Unlock()
		socketMu.Lock()
		sockets, socketsAt, socketCandidates = oldSockets, oldSocketsAt, oldSocketCandidates
		socketMu.Unlock()
		bindingStateMu.Lock()
		bindingStateAt = oldBindingStateAt
		bindingStateMu.Unlock()
	})

	want := &sessionRecord{AgentSession: AgentSession{ID: "target", CanSend: false, BindingReason: "no_pane_candidate"}}
	got, valid := recoverSessionWithScan("target", func() ([]*sessionRecord, bool) {
		visibleCacheMu.Lock()
		visibleCleared := visibleCache == nil && visibleCacheAt.IsZero()
		visibleCacheMu.Unlock()
		liveMu.Lock()
		liveCleared := liveValue == nil && liveAt.IsZero()
		liveMu.Unlock()
		procMu.Lock()
		procCleared := procValue == nil && procAt.IsZero()
		procMu.Unlock()
		socketMu.Lock()
		socketCleared := sockets == nil && socketsAt.IsZero() && socketCandidates == nil
		socketMu.Unlock()
		if !visibleCleared || !liveCleared || !procCleared || !socketCleared || bindingStateFresh() {
			t.Fatalf("recover scan observed stale caches: visible=%v live=%v proc=%v sockets=%v binding=%v", visibleCleared, liveCleared, procCleared, socketCleared, bindingStateFresh())
		}
		return []*sessionRecord{want}, true
	})
	if !valid || got != want || got.BindingReason != "no_pane_candidate" {
		t.Fatalf("recover result=(%#v,%v), want target structured failure", got, valid)
	}
}

func TestWaitForClaudeSendReconciliation(t *testing.T) {
	for _, test := range []struct {
		name      string
		expected  string
		row       map[string]any
		matchKind string
	}{
		{
			name:     "type user row",
			expected: "caption\n@/Users/example/Library/Caches/corral-uploads/image.png ",
			row: map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": "caption\n@/Users/example/Library/Caches/corral-uploads/image.png"},
			},
			matchKind: "user_message",
		},
		{
			name:      "busy queue enqueue 09:11 sample",
			expected:  "忽略这个问题。",
			row:       map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "忽略这个问题。"},
			matchKind: "queue_operation_enqueue",
		},
		{
			name:      "busy queue enqueue 09:13 sample",
			expected:  "因为是旧 air 的收藏，显示的是离线",
			row:       map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "因为是旧 air 的收藏，显示的是离线"},
			matchKind: "queue_operation_enqueue",
		},
		{
			name:      "busy composer appends marker to existing prompt",
			expected:  "[CANARY-1784396100]",
			row:       map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "existing queued message\nunrelated prefix [CANARY-1784396100]"},
			matchKind: "queue_operation_enqueue_suffix",
		},
		{
			name:     "synthetic slash command skill load",
			expected: "/example-skill load this skill only",
			row: map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": "<command-message>example-skill</command-message>\n<command-name>/example-skill</command-name>\n<command-args>load this skill only</command-args>",
				},
			},
			matchKind: "slash_command_user",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			baseline, err := fileWritePointForPath(path)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				time.Sleep(20 * time.Millisecond)
				data, _ := json.Marshal(test.row)
				file, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
				if file != nil {
					_, _ = file.Write(append(data, '\n'))
					_ = file.Close()
				}
			}()
			matchKind, err := waitForClaudeSendReconciliation(path, baseline, test.expected, 500*time.Millisecond)
			if err != nil || matchKind != test.matchKind {
				t.Fatalf("reconciliation=(%q,%v), want %q", matchKind, err, test.matchKind)
			}
		})
	}

	expected := "忽略这个问题。"
	nonEvidence := `{"type":"queue-operation","operation":"remove","content":"忽略这个问题。"}
{"type":"queue-operation","operation":"dequeue"}
{"type":"queue-operation","operation":"enqueue","content":"prefix 忽略这个问题。 trailing"}
{"type":"attachment","attachment":{"type":"queued_command","prompt":"忽略这个问题。"}}
{"type":"user","message":{"role":"user","content":"different message"}}
`
	if appendedClaudeUserMessageMatches([]byte(nonEvidence), expected) {
		t.Fatal("remove/dequeue/attachment or a different user row must not reconcile")
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"+nonEvidence), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := fileWritePointForPath(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"type":"queue-operation","operation":"remove","content":"忽略这个问题。"}` + "\n")
	_ = file.Close()
	if _, err := waitForClaudeSendReconciliation(path, baseline, expected, 30*time.Millisecond); err == nil {
		t.Fatal("absence of enqueue and matching user row must be an incident")
	}
}

func TestClaudeHistoryIndexLoadsIncrementally(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	first := `{"project":"/work","timestamp":1000,"sessionId":"AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"}` + "\n"
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateClaudeHistoryCache()
	t.Cleanup(invalidateClaudeHistoryCache)
	index := loadClaudeHistoryIndex()
	if got := index["/work"]; got.SessionID != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" || got.At.UnixMilli() != 1000 {
		t.Fatalf("initial history=%#v", got)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"project":"/work","timestamp":2000,"sessionId":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}` + "\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	index = loadClaudeHistoryIndex()
	if got := index["/work"]; got.SessionID != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" || got.At.UnixMilli() != 2000 || got.Ambiguous {
		t.Fatalf("incremental history=%#v", got)
	}
}

func TestQuarantinedWeakBindingStaysReadOnly(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := weakTestInspection(started, pane)
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	tracker := newWeakBindingTracker("")
	first := bindInspectionDetailed([]*sessionRecord{record}, inspection, tracker, nil, now)
	if first.bound != 1 || record.binding == nil {
		t.Fatalf("initial weak bind failed: %#v", record)
	}
	tracker.quarantine(record.binding, record)
	secondRecord := weakRecord(sessionID, now.Add(-time.Hour), 11)
	second := bindInspectionDetailed([]*sessionRecord{secondRecord}, inspection, tracker, nil, now.Add(time.Second))
	if second.bound != 1 || secondRecord.binding == nil || !secondRecord.Live || secondRecord.State == "gone" || secondRecord.CanSend || secondRecord.BindingReason != "delivery_reconciliation_failed" {
		t.Fatalf("quarantined binding must remain read-only: %#v", secondRecord.AgentSession)
	}
	if len(second.records) != 1 || !tracker.isQuarantined("claude", sessionID) {
		t.Fatalf("read-only attribution must suppress duplicate placeholder and preserve quarantine: %#v", second.records)
	}
}

func TestQuarantinedBindingLosesAttributionWhenProcessDies(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	tracker := newWeakBindingTracker("")
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	if got := bindInspectionDetailed([]*sessionRecord{record}, weakTestInspection(started, pane), tracker, nil, now); got.bound != 1 {
		t.Fatalf("initial bind failed: %#v", got)
	}
	tracker.quarantine(record.binding, record)
	gone := weakRecord(sessionID, now.Add(-time.Hour), 10)
	inspection := liveInspection{files: processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true}, snap: &procSnapshot{command: map[int]string{}, started: map[int]time.Time{}, valid: true}, valid: true}
	got := bindInspectionDetailed([]*sessionRecord{gone}, inspection, tracker, nil, now.Add(time.Second))
	if got.bound != 0 || gone.binding != nil || gone.Live || gone.State != "gone" || gone.CanSend {
		t.Fatalf("dead process must lose read attribution and fall back to history state: outcome=%#v record=%#v", got, gone)
	}
}

func TestQuarantinedBindingLosesAttributionOnIdentityMismatch(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	tracker := newWeakBindingTracker("")
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	if got := bindInspectionDetailed([]*sessionRecord{record}, weakTestInspection(started, pane), tracker, nil, now); got.bound != 1 {
		t.Fatalf("initial bind failed: %#v", got)
	}
	tracker.quarantine(record.binding, record)
	replaced := weakRecord(sessionID, now.Add(-time.Hour), 10)
	got := bindInspectionDetailed([]*sessionRecord{replaced}, weakTestInspection(started.Add(time.Second), pane), tracker, nil, now.Add(time.Second))
	if got.bound != 0 || replaced.binding != nil || replaced.Live || replaced.State != "gone" || replaced.CanSend {
		t.Fatalf("identity mismatch must invalidate read attribution: outcome=%#v record=%#v", got, replaced)
	}
	if len(got.records) != 2 || got.records[1].BindingReason != "no_session_attribution" {
		t.Fatalf("new pane identity must receive a separate placeholder: %#v", got.records)
	}
}

func TestRecoverQuarantinedUniqueActiveWriter(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := weakTestInspection(started, pane)
	tracker := newWeakBindingTracker("")
	initial := weakRecord(sessionID, now.Add(-time.Second), 10)
	if got := bindInspectionDetailed([]*sessionRecord{initial}, inspection, tracker, nil, now); got.bound != 1 {
		t.Fatalf("initial bind failed: %#v", got)
	}
	tracker.quarantine(initial.binding, initial)

	recovered := weakRecord(sessionID, now.Add(time.Second), 11)
	got := bindInspectionDetailedForRecovery([]*sessionRecord{recovered}, inspection, tracker, nil, now.Add(time.Second), stableSessionID(sessionID))
	if got.bound != 1 || !recovered.CanSend || recovered.bindingEvidence != "active_writer" || tracker.isQuarantined("claude", sessionID) {
		t.Fatalf("fresh unique recovery failed: outcome=%#v record=%#v", got, recovered)
	}
}

func TestRecoverQuarantinedAmbiguityStaysReadOnly(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	aID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	bID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := weakTestInspection(started, pane)
	tracker := newWeakBindingTracker("")
	a := weakRecord(aID, now.Add(-time.Second), 10)
	b := weakRecord(bID, now.Add(-time.Minute), 10)
	if got := bindInspectionDetailed([]*sessionRecord{a, b}, inspection, tracker, nil, now); got.bound != 1 {
		t.Fatalf("initial bind failed: %#v", got)
	}
	tracker.quarantine(a.binding, a)
	a = weakRecord(aID, now.Add(time.Second), 11)
	b = weakRecord(bID, now.Add(time.Second), 11)
	got := bindInspectionDetailedForRecovery([]*sessionRecord{a, b}, inspection, tracker, nil, now.Add(time.Second), stableSessionID(aID))
	if got.bound != 0 || a.CanSend || a.BindingReason != "multi_candidate_ambiguous" || !tracker.isQuarantined("claude", aID) {
		t.Fatalf("ambiguous recovery must preserve quarantine: outcome=%#v record=%#v", got, a)
	}
}

func TestRecoverQuarantinedStaleEvidenceStaysReadOnly(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := weakTestInspection(started, pane)
	tracker := newWeakBindingTracker("")
	initial := weakRecord(sessionID, now.Add(-time.Second), 10)
	if got := bindInspectionDetailed([]*sessionRecord{initial}, inspection, tracker, nil, now); got.bound != 1 {
		t.Fatalf("initial bind failed: %#v", got)
	}
	tracker.quarantine(initial.binding, initial)
	stale := weakRecord(sessionID, now.Add(-time.Hour), 10)
	history := map[string]claudeHistoryEntry{"/work": {SessionID: sessionID, At: started.Add(-time.Second)}}
	got := bindInspectionDetailedForRecovery([]*sessionRecord{stale}, inspection, tracker, history, now.Add(time.Second), stableSessionID(sessionID))
	if got.bound != 0 || stale.CanSend || stale.BindingReason != "evidence_stale" || !tracker.isQuarantined("claude", sessionID) {
		t.Fatalf("stale recovery must preserve quarantine: outcome=%#v record=%#v", got, stale)
	}
}

func TestBackgroundStrongScanDoesNotClearQuarantine(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := weakTestInspection(started, pane)
	tracker := newWeakBindingTracker("")
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	if got := bindInspectionDetailed([]*sessionRecord{record}, inspection, tracker, nil, now); got.bound != 1 {
		t.Fatalf("initial bind failed: %#v", got)
	}
	tracker.quarantine(record.binding, record)
	strong := weakTestInspection(started, pane)
	strong.snap.command[10] = "claude --session-id " + sessionID
	record = weakRecord(sessionID, now.Add(-time.Hour), 11)
	got := bindInspectionDetailed([]*sessionRecord{record}, strong, tracker, nil, now.Add(time.Second))
	if got.bound != 1 || record.binding == nil || !record.Live || record.State == "gone" || record.CanSend || record.BindingReason != "delivery_reconciliation_failed" || !tracker.isQuarantined("claude", sessionID) {
		t.Fatalf("background strong scan must not clear quarantine: outcome=%#v record=%#v", got, record)
	}
}
