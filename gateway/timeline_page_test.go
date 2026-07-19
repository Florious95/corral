package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTimelineQuerySupportsExclusiveBeforeSeq(t *testing.T) {
	query, err := parseTimelineQuery(httptest.NewRequest("GET", "/timeline?beforeSeq=9001&limit=3", nil))
	if err != nil || !query.HasBefore || query.BeforeSeq != 9001 || query.HasAfter || query.Limit != 3 {
		t.Fatalf("query=%#v err=%v", query, err)
	}
	if _, err := parseTimelineQuery(httptest.NewRequest("GET", "/timeline?afterSeq=1&beforeSeq=2", nil)); err == nil {
		t.Fatal("afterSeq and beforeSeq together must be rejected")
	}
}

func TestTimelineBeforePagesMergeToFullBaselineWithoutLossOrDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	rows := []string{
		`{"type":"assistant","timestamp":"2026-07-18T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"pwd"}},{"type":"text","text":"one"}]}}`,
		`{"type":"system","timestamp":"2026-07-18T00:00:01Z","subtype":"status"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-18T00:00:02Z","content":"queued once"}`,
		`{"type":"user","timestamp":"2026-07-18T00:00:03Z","message":{"role":"user","content":"queued once"}}`,
		`{"type":"user","timestamp":"2026-07-18T00:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok","is_error":false},{"type":"text","text":"two"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-18T00:00:05Z","message":{"role":"assistant","content":"three"}}`,
		`{"type":"user","timestamp":"2026-07-18T00:00:06Z","message":{"role":"user","content":"four"}}`,
		`{"type":"assistant","timestamp":"2026-07-18T00:00:07Z","message":{"role":"assistant","content":"five"}}`,
		`{"type":"user","timestamp":"2026-07-18T00:00:08Z","message":{"role":"user","content":"six"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "page-fixture"), nil)
	if record == nil {
		t.Fatal("metadata missing")
	}
	want, err := claudeTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := timelinePageFor(record, timelineQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	got := append([]TimelineEvent(nil), page.Events...)
	for page.HasMoreBefore {
		if page.NextBeforeSeq == nil {
			t.Fatal("hasMoreBefore without nextBeforeSeq")
		}
		page, err = timelinePageFor(record, timelineQuery{Limit: 3, HasBefore: true, BeforeSeq: *page.NextBeforeSeq})
		if err != nil {
			t.Fatal(err)
		}
		got = append(append([]TimelineEvent(nil), page.Events...), got...)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("paged timeline mismatch\n got=%s\nwant=%s", gotJSON, wantJSON)
	}
	queued := 0
	toolResultName := ""
	for _, event := range got {
		if event.Text == "queued once" {
			queued++
		}
		if event.Type == "tool_result" {
			toolResultName = event.Tool
		}
	}
	if queued != 1 || toolResultName != "Bash" {
		t.Fatalf("queued=%d toolResultName=%q", queued, toolResultName)
	}
}

func TestTimelineLatestPageReadsOnlyTailWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	var data strings.Builder
	for index := 0; index < 24000; index++ {
		data.WriteString(`{"type":"assistant","timestamp":"2026-07-18T00:00:00Z","message":{"role":"assistant","content":"`)
		data.WriteString(strings.Repeat("x", 256))
		data.WriteString(`"}}`)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "large-page"), nil)
	if record == nil {
		t.Fatal("metadata missing")
	}
	var bytesRead atomic.Int64
	timelinePageReadHook = func(_ string, count int64) { bytesRead.Add(count) }
	t.Cleanup(func() { timelinePageReadHook = nil })
	page, err := timelinePageFor(record, timelineQuery{Limit: 200})
	if err != nil || len(page.Events) != 200 || !page.HasMoreBefore {
		t.Fatalf("events=%d hasMore=%t err=%v", len(page.Events), page.HasMoreBefore, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytesRead.Load(); got <= 0 || got >= info.Size()/4 {
		t.Fatalf("tail page bytesRead=%d file=%d; default page scanned too much", got, info.Size())
	}
}

func TestTimelinePagePreservesQueueAndToolContextAcrossWindowBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boundary.jsonl")
	rows := []string{
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-18T00:00:00Z","content":"queued once"}`,
		`{"type":"assistant","timestamp":"2026-07-18T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-old","name":"Bash","input":{"command":"pwd"}}]}}`,
	}
	for index := 0; index < timelineInitialWindowLines+20; index++ {
		rows = append(rows, `{"type":"system","timestamp":"2026-07-18T00:00:02Z","subtype":"noop"}`)
	}
	rows = append(rows,
		`{"type":"user","timestamp":"2026-07-18T00:00:03Z","message":{"role":"user","content":"queued once"}}`,
		`{"type":"user","timestamp":"2026-07-18T00:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-old","content":"ok","is_error":false}]}}`,
		`{"type":"assistant","timestamp":"2026-07-18T00:00:05Z","message":{"role":"assistant","content":"tail one"}}`,
		`{"type":"assistant","timestamp":"2026-07-18T00:00:06Z","message":{"role":"assistant","content":"tail two"}}`,
		`{"type":"assistant","timestamp":"2026-07-18T00:00:07Z","message":{"role":"assistant","content":"tail three"}}`,
	)
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "boundary"), nil)
	page, err := timelinePageFor(record, timelineQuery{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 4 {
		t.Fatalf("events=%#v", page.Events)
	}
	queuedEchoes := 0
	toolName := ""
	for _, event := range page.Events {
		if event.Text == "queued once" {
			queuedEchoes++
		}
		if event.Type == "tool_result" {
			toolName = event.Tool
		}
	}
	if queuedEchoes != 0 || toolName != "Bash" {
		t.Fatalf("queuedEchoes=%d tool=%q events=%#v", queuedEchoes, toolName, page.Events)
	}
	got := append([]TimelineEvent(nil), page.Events...)
	for page.HasMoreBefore {
		if page.NextBeforeSeq == nil {
			t.Fatal("hasMoreBefore without cursor")
		}
		page, err = timelinePageFor(record, timelineQuery{Limit: 4, HasBefore: true, BeforeSeq: *page.NextBeforeSeq})
		if err != nil {
			t.Fatal(err)
		}
		got = append(append([]TimelineEvent(nil), page.Events...), got...)
	}
	want, err := claudeTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boundary pages differ\n got=%#v\nwant=%#v", got, want)
	}
}

func TestTimelineAfterPageReadsBoundedRangeAndSharesParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "after.jsonl")
	var data strings.Builder
	for index := 0; index < 24000; index++ {
		data.WriteString(`{"type":"assistant","timestamp":"2026-07-18T00:00:00Z","message":{"role":"assistant","content":"line"}}`)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "after"), nil)
	var bytesRead atomic.Int64
	var parses atomic.Int32
	timelinePageReadHook = func(_ string, count int64) { bytesRead.Add(count) }
	timelineParseHook = func(string) { parses.Add(1) }
	t.Cleanup(func() {
		timelinePageReadHook = nil
		timelineParseHook = nil
	})
	query := timelineQuery{Limit: 200, HasAfter: true, AfterSeq: 1000 * 1000}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := timelinePageFor(record, query)
			if err != nil || len(page.Events) != 200 || !page.HasMoreAfter {
				t.Errorf("events=%d more=%t err=%v", len(page.Events), page.HasMoreAfter, err)
			}
		}()
	}
	wg.Wait()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := parses.Load(); got != 1 {
		t.Fatalf("physical parses=%d want=1", got)
	}
	if got := bytesRead.Load(); got <= 0 || got >= info.Size()/4 {
		t.Fatalf("after page bytesRead=%d file=%d", got, info.Size())
	}
}

func TestTimelineLineIndexRebuildsAfterTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncate.jsonl")
	row := func(text string) string {
		return `{"type":"assistant","timestamp":"2026-07-18T00:00:00Z","message":{"role":"assistant","content":"` + text + `"}}`
	}
	if err := os.WriteFile(path, []byte(strings.Join([]string{row("one"), row("two"), row("three")}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "truncate"), nil)
	page, err := timelinePageFor(record, timelineQuery{Limit: 10})
	if err != nil || len(page.Events) != 3 {
		t.Fatalf("before truncate events=%d err=%v", len(page.Events), err)
	}
	if err := os.WriteFile(path, []byte(row("replacement")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record = cachedMetadata(physicalFileForTest(t, path, "claude", "truncate"), nil)
	page, err = timelinePageFor(record, timelineQuery{Limit: 10})
	if err != nil || len(page.Events) != 1 || page.Events[0].Seq != 1001 || page.Events[0].Text != "replacement" {
		t.Fatalf("after truncate page=%#v err=%v", page, err)
	}
}

func TestTimelineLineIndexRebuildsAfterInodeReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replace.jsonl")
	row := func(text string) string {
		return `{"type":"assistant","timestamp":"2026-07-18T00:00:00Z","message":{"role":"assistant","content":"` + text + `"}}`
	}
	if err := os.WriteFile(path, []byte(row("old")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMetadataCacheForTest(t)
	resetTimelinePageStateForTest(t)
	record := cachedMetadata(physicalFileForTest(t, path, "claude", "replace"), nil)
	oldIndex, err := ensureTimelineLineIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join([]string{row("new one"), row("new two")}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record = cachedMetadata(physicalFileForTest(t, path, "claude", "replace"), nil)
	page, err := timelinePageFor(record, timelineQuery{Limit: 10})
	if err != nil || len(page.Events) != 2 || page.Events[0].Text != "new one" || page.Events[1].Text != "new two" {
		t.Fatalf("replacement page=%#v err=%v", page, err)
	}
	newIndex, err := ensureTimelineLineIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if newIndex.Version.inode == oldIndex.Version.inode {
		t.Fatalf("test did not replace inode: %d", newIndex.Version.inode)
	}
}
