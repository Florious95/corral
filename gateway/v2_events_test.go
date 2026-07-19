package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTerminalEntryStorePublishesAttachmentRemovalAndStaleRevision(t *testing.T) {
	store := newTerminalEntryStore("events")
	record1, record2 := testV2Record("r1"), testV2Record("r2")
	first := store.commit(v2SnapshotInput{
		Host:    v2Host{ID: "host"},
		Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record1, EvidenceRank: 3}},
		History: []v2HistoryRecord{record1, record2},
	})

	changes, cancel := store.subscribe(first.Revision)
	defer cancel()
	second := store.commit(v2SnapshotInput{
		Host:    v2Host{ID: "host"},
		Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record2, EvidenceRank: 3}},
		History: []v2HistoryRecord{record1, record2},
	})
	change := receiveV2Change(t, changes)
	if change.SnapshotRequired || change.PreviousRevision != first.Revision || change.Revision != second.Revision {
		t.Fatalf("change=%#v", change)
	}
	if len(change.Attachments) != 1 || change.Attachments[0].EntryID != "e1" ||
		change.Attachments[0].PreviousRecordID != "r1" || change.Attachments[0].Attachment == nil ||
		change.Attachments[0].Attachment.RecordID != "r2" || change.Attachments[0].AttachmentRevision != 2 {
		t.Fatalf("attachment change=%#v", change.Attachments)
	}

	cleared := store.commit(v2SnapshotInput{
		Host:    v2Host{ID: "host"},
		Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}},
		History: []v2HistoryRecord{record1, record2},
	})
	change = receiveV2Change(t, changes)
	if change.Revision != cleared.Revision || len(change.Attachments) != 1 ||
		change.Attachments[0].Attachment != nil || change.Attachments[0].AttachmentRevision != 3 {
		t.Fatalf("attachment clear=%#v", change)
	}

	removed := store.commitWithRemovalReason(v2SnapshotInput{Host: v2Host{ID: "host"}}, "process_exit")
	change = receiveV2Change(t, changes)
	if change.Revision != removed.Revision || len(change.Removed) != 1 ||
		change.Removed[0].EntryID != "e1" || change.Removed[0].Reason != "process_exit" {
		t.Fatalf("removal change=%#v", change)
	}

	stale, staleCancel := store.subscribe(first.Revision)
	defer staleCancel()
	change = receiveV2Change(t, stale)
	if !change.SnapshotRequired || change.Revision != removed.Revision || change.PreviousRevision != first.Revision {
		t.Fatalf("stale change=%#v", change)
	}
}

func TestTerminalEntryStoreClassifiesSamePaneGenerationReplacement(t *testing.T) {
	store := newTerminalEntryStore("generation")
	oldEntry := testV2Entry("old")
	oldEntry.logicalKey = "host\x00socket\x00%0"
	first := store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: oldEntry}}})
	changes, cancel := store.subscribe(first.Revision)
	defer cancel()
	newEntry := testV2Entry("new")
	newEntry.logicalKey = oldEntry.logicalKey
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: newEntry}}})
	change := receiveV2Change(t, changes)
	if len(change.Removed) != 1 || change.Removed[0].EntryID != "old" || change.Removed[0].Reason != "generation_replaced" {
		t.Fatalf("generation replacement=%#v", change.Removed)
	}
}

func TestServeV2HostStreamUsesRevisionAsSSEIDAndDoesNotPoll(t *testing.T) {
	store := newTerminalEntryStore("stream")
	first := store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2HostStream(w, r, store)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"?afterRevision=stale", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	event := readV2SSEEvent(t, response, "snapshot_required")
	_ = response.Body.Close()
	if event.ID != first.Revision || event.Data["revision"] != first.Revision || event.Data["previousRevision"] != "stale" {
		t.Fatalf("snapshot_required=%#v", event)
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL, nil)
	request.Header.Set("Last-Event-ID", first.Revision)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan [2]v2SSETestEvent, 1)
	go func() {
		reader := bufio.NewReader(response.Body)
		done <- [2]v2SSETestEvent{
			readV2SSEEventFromReader(t, reader, "snapshot_changed"),
			readV2SSEEventFromReader(t, reader, "entry_removed"),
		}
	}()
	time.Sleep(20 * time.Millisecond)
	second := store.commitWithRemovalReason(v2SnapshotInput{Host: v2Host{ID: "host"}}, "pane_gone")
	select {
	case events := <-done:
		if events[0].ID != second.Revision || events[0].Data["previousRevision"] != first.Revision {
			t.Fatalf("snapshot_changed=%#v", events[0])
		}
		event = events[1]
	case <-time.After(2 * time.Second):
		t.Fatal("host stream did not publish committed change")
	}
	_ = response.Body.Close()
	if event.ID != second.Revision || event.Data["previousRevision"] != first.Revision || event.Data["reason"] != "pane_gone" {
		t.Fatalf("entry_removed=%#v", event)
	}
}

func TestServeV2SnapshotCurrentNeverRunsASynchronousScan(t *testing.T) {
	store := newTerminalEntryStore("current")
	recorder := httptest.NewRecorder()
	serveV2SnapshotCurrent(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/snapshot", nil), store)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty store status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	snapshot := store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	recorder = httptest.NewRecorder()
	serveV2SnapshotCurrent(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/snapshot", nil), store)
	if recorder.Code != http.StatusOK {
		t.Fatalf("committed store status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response v2Snapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Revision != snapshot.Revision || len(response.Entries) != 1 {
		t.Fatalf("snapshot response=%#v", response)
	}
}

type fakeV2PathWatcher struct{ events chan v2PathInvalidation }

func (watcher *fakeV2PathWatcher) Events() <-chan v2PathInvalidation { return watcher.events }
func (watcher *fakeV2PathWatcher) Close() error                      { return nil }

type fakeV2ProcessWatcher struct {
	events chan int
	mu     sync.Mutex
	pids   []int
}

type fakeV2SocketWatcher struct{ events chan string }

func (watcher *fakeV2SocketWatcher) Events() <-chan string { return watcher.events }
func (watcher *fakeV2SocketWatcher) Close() error          { return nil }

func (watcher *fakeV2ProcessWatcher) Events() <-chan int { return watcher.events }
func (watcher *fakeV2ProcessWatcher) Close() error       { return nil }
func (watcher *fakeV2ProcessWatcher) Set(pids []int) error {
	watcher.mu.Lock()
	watcher.pids = append([]int(nil), pids...)
	watcher.mu.Unlock()
	return nil
}

func TestV2PaneDiscoveryIntervalMeetsTwoSecondContract(t *testing.T) {
	if v2PaneDiscoveryInterval != 2*time.Second {
		t.Fatalf("pane discovery interval=%s want=2s", v2PaneDiscoveryInterval)
	}
}

func TestV2SocketAndPathEventsContinueWhileFullInspectionIsBlocked(t *testing.T) {
	store := newTerminalEntryStore("socket-event")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation, 1)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	sockets := &fakeV2SocketWatcher{events: make(chan string, 1)}
	inspectStarted := make(chan struct{})
	releaseInspect := make(chan struct{})
	releaseBuild := make(chan struct{})
	var releaseInspectOnce sync.Once
	var inspectCalls int
	t.Cleanup(func() {
		releaseInspectOnce.Do(func() { close(releaseInspect) })
		close(releaseBuild)
	})
	fastPaths := make(chan []string, 1)
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, SocketWatcher: sockets, PaneInterval: time.Millisecond,
		Inspect: func() liveInspection {
			inspectCalls++
			call := inspectCalls
			if call == 2 {
				select {
				case <-inspectStarted:
				default:
					close(inspectStarted)
				}
				<-releaseInspect
			}
			inspection := liveInspection{files: processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true}, snap: &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true}, valid: true}
			if call >= 3 {
				inspection.panes = []paneBinding{{Socket: "/tmp/new-socket", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}}
			}
			return inspection
		},
		InspectSocket: func(socket string) []paneBinding {
			return []paneBinding{{Socket: socket, TmuxID: "%0", PanePID: 10, Kind: "claude"}}
		},
		ClassifyPane: func(int) (string, []int, *procSnapshot, bool) {
			return "claude", []int{11}, &procSnapshot{command: map[int]string{11: "claude"}, children: map[int][]int{10: {11}}, started: map[int]time.Time{}, valid: true}, true
		},
		Build: func(liveInspection) (v2SnapshotInput, error) {
			<-releaseBuild
			return v2SnapshotInput{Host: v2Host{ID: "host"}}, nil
		},
		FastBuild: func(inspection liveInspection, changed []string) (v2SnapshotInput, bool, error) {
			if len(changed) > 0 {
				fastPaths <- append([]string(nil), changed...)
			}
			entries := make([]v2EntryDraft, 0, len(inspection.panes))
			for _, pane := range inspection.panes {
				entry := testV2Entry("entry-" + pane.TmuxID)
				entry.logicalKey = v2PaneLogicalKey(v2Host{ID: "host"}, pane)
				entry.runtime = &v2EntryRuntime{Binding: pane}
				entries = append(entries, v2EntryDraft{Entry: entry})
			}
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: entries}, true, nil
		},
		Invalidate: func([]string, bool) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	select {
	case <-inspectStarted:
	case <-time.After(time.Second):
		t.Fatal("background inspection did not block")
	}
	started := time.Now()
	sockets.events <- "/tmp/new-socket"
	deadline := started.Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(store.snapshot().Entries) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(store.snapshot().Entries) != 1 {
		t.Fatal("socket event was blocked behind full inspection")
	}
	paths.events <- v2PathInvalidation{Paths: []string{"/work/exact.jsonl"}}
	select {
	case changed := <-fastPaths:
		if len(changed) != 1 || changed[0] != "/work/exact.jsonl" {
			t.Fatalf("fast paths=%#v", changed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("path event was blocked behind full inspection")
	}
	releaseInspectOnce.Do(func() { close(releaseInspect) })
	time.Sleep(10 * time.Millisecond)
	if len(store.snapshot().Entries) != 1 {
		t.Fatal("stale full inspection removed the socket-event entry")
	}
}

func TestV2EventEngineDisableKeepsInitialSnapshotWithoutBackgroundRefresh(t *testing.T) {
	t.Setenv("V2_EVENT_ENGINE_DISABLE", "1")
	store := newTerminalEntryStore("engine-disabled")
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation, 1)}
	processes := &fakeV2ProcessWatcher{events: make(chan int, 1)}
	var mu sync.Mutex
	inspectCalls, buildCalls, invalidations := 0, 0, 0
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, PaneInterval: 10 * time.Millisecond,
		Inspect: func() liveInspection {
			mu.Lock()
			inspectCalls++
			mu.Unlock()
			return liveInspection{
				panes: []paneBinding{{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}},
				valid: true,
			}
		},
		Build: func(liveInspection) (v2SnapshotInput, error) {
			mu.Lock()
			buildCalls++
			mu.Unlock()
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("entry-%0")}}}, nil
		},
		Invalidate: func([]string, bool) {
			mu.Lock()
			invalidations++
			mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	waitForV2Revision(t, store, "rv1_engine-disabled_1")
	paths.events <- v2PathInvalidation{Paths: []string{"/work/session.jsonl"}}
	time.Sleep(4 * v2PathInvalidationDebounce)
	mu.Lock()
	defer mu.Unlock()
	if inspectCalls != 1 || buildCalls != 1 || invalidations != 0 {
		t.Fatalf("disabled engine inspect/build/invalidate=%d/%d/%d, want 1/1/0", inspectCalls, buildCalls, invalidations)
	}
	if snapshot := store.snapshot(); len(snapshot.Entries) != 1 || snapshot.Entries[0].EntryID != "entry-%0" {
		t.Fatalf("initial snapshot=%#v", snapshot)
	}
}

func TestV2EventEngineSuccessfulBuildPublishesV1VisibleSessions(t *testing.T) {
	visibleCacheMu.Lock()
	oldVisible, oldVisibleAt, oldV2 := visibleCache, visibleCacheAt, visibleV2Cache
	visibleCache, visibleCacheAt, visibleV2Cache = nil, time.Time{}, nil
	visibleCacheMu.Unlock()
	t.Cleanup(func() {
		visibleCacheMu.Lock()
		visibleCache, visibleCacheAt, visibleV2Cache = oldVisible, oldVisibleAt, oldV2
		visibleCacheMu.Unlock()
	})

	record := &sessionRecord{AgentSession: AgentSession{ID: "record-v2", SessionID: "session-v2", Kind: "claude"}}
	store := newTerminalEntryStore("publish-v1")
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store,
		Build: func(liveInspection) (v2SnapshotInput, error) {
			return v2SnapshotInput{Host: v2Host{ID: "host"}, visibleRecords: []*sessionRecord{record}}, nil
		},
	})
	engine.inspection = liveInspection{valid: true}
	engine.refreshSnapshot("pane_gone")
	records, valid := visibleSessions()
	if !valid || len(records) != 1 || records[0].ID != record.ID || records[0].SessionID != record.SessionID {
		t.Fatalf("published sessions=(%#v,%v)", records, valid)
	}
}

func TestV2EventEngineBadPaneDoesNotFreezeGoodPaneRevision(t *testing.T) {
	host := v2Host{ID: "host"}
	started := time.Date(2026, 7, 19, 3, 42, 0, 0, time.UTC)
	bad := paneBinding{Socket: "/tmp/ta-bad", TmuxID: "%29", PanePID: 29, Kind: "claude", WindowName: "bad-tty", ProcessPIDs: []int{129}}
	good := paneBinding{Socket: "/tmp/ta-good", TmuxID: "%0", PanePID: 10, Kind: "claude", WindowName: "leader", ProcessPIDs: []int{110}}
	goodIdentity := v2EntryIdentity{HostID: host.ID, SocketPath: good.Socket, PaneID: good.TmuxID, AgentPID: good.ProcessPIDs[0], StartSec: started.Unix()}
	badPrevious := testV2Entry("bad-previous")
	badPrevious.logicalKey = v2PaneLogicalKey(host, bad)
	badPrevious.Pane = v2Pane{PaneID: bad.TmuxID, WindowName: bad.WindowName}
	badPrevious.State = "waiting_input"
	store := newTerminalEntryStore("bad-pane")
	first := store.commit(v2SnapshotInput{Host: host, Entries: []v2EntryDraft{{Entry: badPrevious}}})
	changes, cancel := store.subscribe(first.Revision)
	defer cancel()

	inspection := liveInspection{panes: []paneBinding{bad, good}, files: processFiles{cwd: map[int]string{110: "/work"}, valid: true}, snap: &procSnapshot{valid: true}, valid: true}
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store,
		Build: func(current liveInspection) (v2SnapshotInput, error) {
			return v2InputFromRecords(host, nil, current, func(_ v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
				if pane.TmuxID == bad.TmuxID {
					return v2EntryIdentity{}, time.Time{}, errors.New("input/output error")
				}
				return goodIdentity, started, nil
			})
		},
	})
	engine.inspection = inspection
	engine.refreshSnapshot("pane_gone")

	snapshot := store.snapshot()
	if snapshot.Revision != "rv1_bad-pane_2" {
		t.Fatalf("bad pane froze revision at %q", snapshot.Revision)
	}
	select {
	case change := <-changes:
		if change.Revision != snapshot.Revision || change.SnapshotRequired {
			t.Fatalf("SSE change=%#v", change)
		}
	case <-time.After(time.Second):
		t.Fatal("good pane refresh did not publish an SSE revision")
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("entries=%#v, want preserved bad pane plus good pane", snapshot.Entries)
	}
	badEntry := entryByID(t, snapshot, badPrevious.EntryID)
	if badEntry.State != badPrevious.State || badEntry.Pane.PaneID != bad.TmuxID {
		t.Fatalf("preserved bad entry=%#v", badEntry)
	}
	if goodEntry := entryByID(t, snapshot, v2EntryID(goodIdentity)); goodEntry.Pane.PaneID != good.TmuxID {
		t.Fatalf("good entry=%#v", goodEntry)
	}
}

func TestV2EventEngineDiscoversNewPaneOnNextTick(t *testing.T) {
	store := newTerminalEntryStore("pane-birth")
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	var mu sync.Mutex
	paneReady := false
	releaseSlowBuild := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSlowBuild) }) })
	buildEntries := func(current liveInspection) v2SnapshotInput {
		entries := make([]v2EntryDraft, 0, len(current.panes))
		for _, pane := range current.panes {
			entries = append(entries, v2EntryDraft{Entry: testV2Entry("entry-" + pane.TmuxID)})
		}
		return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: entries}
	}
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, PaneInterval: 20 * time.Millisecond,
		Inspect: func() liveInspection {
			mu.Lock()
			defer mu.Unlock()
			inspection := liveInspection{
				files: processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true},
				snap:  &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true},
				hints: map[string]bool{}, valid: true,
			}
			if paneReady {
				inspection = augmentV2InspectionWithNewPanes(inspection,
					[]paneBinding{{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude"}},
					func(int) (string, []int, *procSnapshot, bool) {
						return "claude", []int{11}, &procSnapshot{command: map[int]string{11: "claude"}, children: map[int][]int{10: {11}}, started: map[int]time.Time{}, valid: true}, true
					},
					func([]int) processFiles {
						return processFiles{cwd: map[int]string{11: "/fixture"}, open: map[int][]string{}, valid: true}
					},
				)
			}
			return inspection
		},
		Build: func(current liveInspection) (v2SnapshotInput, error) {
			if len(current.panes) > 0 {
				<-releaseSlowBuild
			}
			return buildEntries(current), nil
		},
		FastBuild: func(current liveInspection, _ []string) (v2SnapshotInput, bool, error) {
			return buildEntries(current), true, nil
		},
		Invalidate: func([]string, bool) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	waitForV2Revision(t, store, "rv1_pane-birth_1")

	mu.Lock()
	paneReady = true
	mu.Unlock()
	bornAt := time.Now()
	deadline := bornAt.Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		snapshot := store.snapshot()
		if len(snapshot.Entries) == 1 && snapshot.Entries[0].EntryID == "entry-%0" {
			if elapsed := time.Since(bornAt); elapsed > 100*time.Millisecond {
				t.Fatalf("pane discovery=%s want<=100ms", elapsed)
			}
			releaseOnce.Do(func() { close(releaseSlowBuild) })
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("new pane missing after %s", time.Since(bornAt))
}

func TestV2PaneDiscoveryContinuesWhileFullSnapshotBuildIsBlocked(t *testing.T) {
	store := newTerminalEntryStore("pane-birth-during-build")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	var startOnce sync.Once
	t.Cleanup(func() {
		select {
		case <-releaseBuild:
		default:
			close(releaseBuild)
		}
	})
	var mu sync.Mutex
	inspection := liveInspection{files: processFiles{cwd: map[int]string{}, open: map[int][]string{}, valid: true}, snap: &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true}, valid: true}
	buildInput := func(current liveInspection) v2SnapshotInput {
		entries := make([]v2EntryDraft, 0, len(current.panes))
		for _, pane := range current.panes {
			entries = append(entries, v2EntryDraft{Entry: testV2Entry("entry-" + pane.TmuxID)})
		}
		return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: entries}
	}
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, PaneInterval: 10 * time.Millisecond,
		Inspect: func() liveInspection {
			mu.Lock()
			defer mu.Unlock()
			return inspection
		},
		Build: func(current liveInspection) (v2SnapshotInput, error) {
			startOnce.Do(func() { close(buildStarted) })
			<-releaseBuild
			return buildInput(current), nil
		},
		FastBuild: func(current liveInspection, _ []string) (v2SnapshotInput, bool, error) {
			return buildInput(current), true, nil
		},
		Invalidate: func([]string, bool) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		t.Fatal("full snapshot build did not start")
	}
	mu.Lock()
	inspection.panes = []paneBinding{{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}}
	mu.Unlock()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if snapshot := store.snapshot(); len(snapshot.Entries) == 1 && snapshot.Entries[0].EntryID == "entry-%0" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pane discovery was blocked behind the full snapshot build")
}

func TestV2PaneFirstCommitPreservesExistingAttachmentAndHistory(t *testing.T) {
	oldDiscover := v2FastPhysicalFilesForPaths
	discoverCalls := 0
	v2FastPhysicalFilesForPaths = func([]string) map[string]physicalFile {
		discoverCalls++
		return nil
	}
	t.Cleanup(func() { v2FastPhysicalFilesForPaths = oldDiscover })
	store := newTerminalEntryStore("pane-first-preserve")
	host := v2Host{ID: "host"}
	oldPane := paneBinding{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}
	oldIdentity := v2EntryIdentity{HostID: host.ID, SocketPath: oldPane.Socket, SocketDevice: 1, SocketInode: 2, PaneID: oldPane.TmuxID, AgentPID: 11, StartSec: 100}
	oldEntry := testV2Entry(v2EntryID(oldIdentity))
	oldEntry.logicalKey = v2PaneLogicalKey(host, oldPane)
	oldEntry.runtime = &v2EntryRuntime{Identity: oldIdentity, Binding: oldPane}
	record := testV2Record("r1")
	history := testV2Record("history")
	store.commit(v2SnapshotInput{Host: host, Entries: []v2EntryDraft{{Entry: oldEntry, Record: &record, EvidenceRank: 3}}, History: []v2HistoryRecord{history}})
	newPane := paneBinding{Socket: "/tmp/new", TmuxID: "%1", PanePID: 20, Kind: "claude", ProcessPIDs: []int{21}}
	inspection := liveInspection{
		panes: []paneBinding{oldPane, newPane}, files: processFiles{cwd: map[int]string{11: "/old", 21: "/new"}, open: map[int][]string{}, valid: true},
		snap: &procSnapshot{command: map[int]string{}, children: map[int][]int{}, started: map[int]time.Time{}, valid: true}, valid: true,
	}
	identifyCalls := 0
	input, ok, err := buildV2PaneFirstInputWithIdentifier(store, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		identifyCalls++
		pid := v2AgentRootPID(pane)
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, SocketDevice: 1, SocketInode: uint64(pid), PaneID: pane.TmuxID, AgentPID: pid, StartSec: int64(pid)}, time.Unix(int64(pid), 0), nil
	}, nil)
	if err != nil || !ok || len(input.Entries) != 2 || input.Entries[0].Record == nil || input.Entries[0].Record.RecordID != "r1" || input.Entries[1].Record != nil {
		t.Fatalf("ok=%t err=%v input=%#v", ok, err, input)
	}
	if identifyCalls != 1 {
		t.Fatalf("identifier calls=%d want=1 for only the new pane", identifyCalls)
	}
	if discoverCalls != 0 {
		t.Fatalf("physical record scans=%d want=0 without an unattached explicit session", discoverCalls)
	}
	snapshot := store.commit(input)
	attached := 0
	for _, entry := range snapshot.Entries {
		if entry.Attachment != nil && entry.Attachment.RecordID == "r1" {
			attached++
		}
	}
	if len(snapshot.Entries) != 2 || attached != 1 || len(snapshot.History) != 1 || snapshot.History[0].RecordID != "history" {
		t.Fatalf("pane-first snapshot=%#v", snapshot)
	}
	removedInput, ok, err := buildV2PaneFirstInputWithIdentifier(store, liveInspection{
		panes: []paneBinding{newPane}, files: inspection.files, snap: inspection.snap, valid: true,
	}, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		pid := v2AgentRootPID(pane)
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, SocketDevice: 1, SocketInode: uint64(pid), PaneID: pane.TmuxID, AgentPID: pid, StartSec: int64(pid)}, time.Unix(int64(pid), 0), nil
	}, nil)
	if err != nil || !ok {
		t.Fatalf("removed fast build ok=%t err=%v", ok, err)
	}
	removedSnapshot := store.commit(removedInput)
	historyIDs := map[string]bool{}
	for _, item := range removedSnapshot.History {
		historyIDs[item.RecordID] = true
	}
	if len(removedSnapshot.Entries) != 1 || !historyIDs["r1"] || !historyIDs["history"] {
		t.Fatalf("removed pane snapshot=%#v", removedSnapshot)
	}
}

func TestV2PathFastBuildParsesOnlyExactChangedRecordAndTriggersPendingEcho(t *testing.T) {
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, time.Now(), 10)
	record.ID = "s-fast"
	record.Cwd = "/work"
	pane := paneBinding{Socket: "/tmp/new", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}
	inspection := weakTestInspection(time.Now().Add(-time.Minute), pane)
	inspection.snap.command[11] = "claude --session-id " + sessionID
	file := physicalFile{kind: "claude", sessionID: sessionID, path: "/fixture/session.jsonl", size: 10, device: 1, inode: 2}

	oldDiscover, oldCached, oldBindings := v2FastPhysicalFilesForPaths, v2FastCachedMetadata, weakBindings
	v2FastPhysicalFilesForPaths = func(paths []string) map[string]physicalFile {
		if len(paths) != 1 || paths[0] != file.path {
			t.Fatalf("fast paths=%#v want=%q", paths, file.path)
		}
		return map[string]physicalFile{"claude\x00" + sessionID: file}
	}
	v2FastCachedMetadata = func(got physicalFile, _ map[string]string) *sessionRecord {
		if got.path != file.path {
			t.Fatalf("metadata file=%#v want=%#v", got, file)
		}
		copy := *record
		return &copy
	}
	weakBindings = newWeakBindingTracker("")
	t.Cleanup(func() {
		v2FastPhysicalFilesForPaths, v2FastCachedMetadata, weakBindings = oldDiscover, oldCached, oldBindings
	})

	store := newTerminalEntryStore("fast-attachment")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	input, ok, err := buildV2PaneFirstInputWithIdentifier(store, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, SocketDevice: 1, SocketInode: 2, PaneID: pane.TmuxID, AgentPID: 11, StartSec: 1}, time.Unix(1, 0), nil
	}, []string{file.path})
	if err != nil || !ok || len(input.Entries) != 1 || input.Entries[0].Record == nil || input.Entries[0].Record.RecordID != "s-fast" || input.Entries[0].EvidenceRank != v2EvidenceRank("strong") {
		t.Fatalf("ok=%t err=%v input=%#v", ok, err, input)
	}
	entryID := input.Entries[0].Entry.EntryID
	unattached := input
	unattached.Entries = append([]v2EntryDraft(nil), input.Entries...)
	unattached.Entries[0].Record = nil
	unattached.Entries[0].BoundRecord = nil
	store.commit(unattached)
	service := newV2WriteService(store, v2WriteDependencies{MatchAttached: func(gotEntryID, expected string) bool {
		entry, status := store.lookup(gotEntryID)
		return status == http.StatusOK && gotEntryID == entryID && expected == "hello" && entry.Attachment != nil && entry.Attachment.RecordID == "s-fast"
	}})
	deliveries, cancel := store.subscribeDeliveries(entryID)
	defer cancel()
	trace := &v2DeliveryTrace{DeliveryID: "d-fast", EntryID: entryID, Started: time.Now()}
	service.queuePendingEcho(entryID, "hello", "nonce-fast", trace, time.Now())
	store.commit(input)
	select {
	case event := <-deliveries:
		if event.Status != "echoed" || event.DeliveryID != trace.DeliveryID {
			t.Fatalf("delivery=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("exact-path attachment did not trigger pending echo")
	}
}

func TestValidatedClaudeRegistryHintRequiresExactPIDCwdAndStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	started := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":11,"sessionId":%q,"cwd":"/work","procStart":%q}`, sessionID, started.Format(claudeProcessStartLayout))
	if err := os.WriteFile(filepath.Join(dir, "11.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := validatedClaudeSessionRegistryHint(11, "/work", started); got != sessionID {
		t.Fatalf("validated registry hint=%q want=%q", got, sessionID)
	}
	if got := validatedClaudeSessionRegistryHint(11, "/other", started); got != "" {
		t.Fatalf("cwd mismatch trusted registry hint %q", got)
	}
	if got := validatedClaudeSessionRegistryHint(11, "/work", started.Add(time.Second)); got != "" {
		t.Fatalf("process start mismatch trusted registry hint %q", got)
	}
}

func TestV2PathFastBuildAttachesValidatedClaudeRegistryWithoutArgvOrEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	started := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	sessionID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":11,"sessionId":%q,"cwd":"/work","procStart":%q}`, sessionID, started.Format(claudeProcessStartLayout))
	if err := os.WriteFile(filepath.Join(dir, "11.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	record := weakRecord(sessionID, time.Now(), 10)
	record.ID = "s-fast-registry"
	pane := paneBinding{Socket: "/tmp/new", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}
	inspection := weakTestInspection(started, pane)
	inspection.snap.command[11] = "claude --dangerously-skip-permissions"
	file := physicalFile{kind: "claude", sessionID: sessionID, path: "/fixture/session.jsonl", size: 10, device: 1, inode: 2}

	oldDiscover, oldCached, oldBindings := v2FastPhysicalFilesForPaths, v2FastCachedMetadata, weakBindings
	v2FastPhysicalFilesForPaths = func([]string) map[string]physicalFile {
		return map[string]physicalFile{"claude\x00" + sessionID: file}
	}
	v2FastCachedMetadata = func(physicalFile, map[string]string) *sessionRecord {
		copy := *record
		return &copy
	}
	weakBindings = newWeakBindingTracker("")
	t.Cleanup(func() {
		v2FastPhysicalFilesForPaths, v2FastCachedMetadata, weakBindings = oldDiscover, oldCached, oldBindings
	})

	store := newTerminalEntryStore("fast-registry")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	input, ok, err := buildV2PaneFirstInputWithIdentifier(store, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, SocketDevice: 1, SocketInode: 2, PaneID: pane.TmuxID, AgentPID: 11, StartSec: started.Unix()}, started, nil
	}, []string{file.path})
	if err != nil || !ok || len(input.Entries) != 1 || input.Entries[0].Record == nil || input.Entries[0].Record.SessionID != sessionID || input.Entries[0].EvidenceRank != v2EvidenceRank("strong") {
		t.Fatalf("registry fast attachment ok=%t err=%v input=%#v", ok, err, input)
	}
}

func TestV2PaneOnlyFastBuildDoesNotWaitForRecordDiscovery(t *testing.T) {
	store := newTerminalEntryStore("pane-only")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}})
	sessionID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	pane := paneBinding{Socket: "/tmp/new", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}
	inspection := weakTestInspection(time.Now(), pane)
	inspection.snap.command[11] = "claude --session-id " + sessionID
	oldDiscover := v2FastPhysicalFilesForPaths
	v2FastPhysicalFilesForPaths = func([]string) map[string]physicalFile {
		t.Fatal("pane-only build attempted record discovery")
		return nil
	}
	t.Cleanup(func() { v2FastPhysicalFilesForPaths = oldDiscover })

	started := time.Now()
	input, ok, err := buildV2PaneFirstInputWithIdentifier(store, inspection, func(host v2Host, pane paneBinding) (v2EntryIdentity, time.Time, error) {
		return v2EntryIdentity{HostID: host.ID, SocketPath: pane.Socket, SocketDevice: 1, SocketInode: 2, PaneID: pane.TmuxID, AgentPID: 11, StartSec: 1}, time.Unix(1, 0), nil
	}, nil)
	if err != nil || !ok || len(input.Entries) != 1 || input.Entries[0].Record != nil {
		t.Fatalf("ok=%t err=%v input=%#v", ok, err, input)
	}
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("pane-only build=%s want<100ms", elapsed)
	}
}

func TestV2EventEngineUnchangedPaneTicksDoNotRebuild(t *testing.T) {
	store := newTerminalEntryStore("pane-unchanged")
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	inspection := liveInspection{
		panes: []paneBinding{{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}},
		valid: true,
	}
	var mu sync.Mutex
	inspectCalls, buildCalls := 0, 0
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, PaneInterval: 10 * time.Millisecond,
		Inspect: func() liveInspection {
			mu.Lock()
			inspectCalls++
			mu.Unlock()
			return inspection
		},
		Build: func(current liveInspection) (v2SnapshotInput, error) {
			mu.Lock()
			buildCalls++
			mu.Unlock()
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("entry-%0")}}}, nil
		},
		Invalidate: func([]string, bool) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	waitForV2Count(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		return inspectCalls
	}, 4)
	mu.Lock()
	defer mu.Unlock()
	if inspectCalls < 4 || buildCalls != 1 {
		t.Fatalf("unchanged pane inspect/build=%d/%d want>=4/1", inspectCalls, buildCalls)
	}
}

func TestV2SnapshotRebuildsDoNotCommitOutOfObservationOrder(t *testing.T) {
	store := newTerminalEntryStore("rebuild-order")
	store.commit(v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}})
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	staleStarted := make(chan struct{})
	releaseStale := make(chan struct{})
	staleDone := make(chan struct{})
	freshDone := make(chan struct{})
	stale := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes,
		Build: func(liveInspection) (v2SnapshotInput, error) {
			close(staleStarted)
			<-releaseStale
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1")}}}, nil
		},
	})
	stale.inspection = liveInspection{valid: true}
	record := testV2Record("r1")
	fresh := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes,
		Build: func(liveInspection) (v2SnapshotInput, error) {
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: []v2EntryDraft{{Entry: testV2Entry("e1"), Record: &record, EvidenceRank: 3}}}, nil
		},
	})
	fresh.inspection = liveInspection{valid: true}

	go func() {
		stale.refreshSnapshot("pane_gone")
		close(staleDone)
	}()
	<-staleStarted
	go func() {
		fresh.refreshSnapshot("pane_gone")
		close(freshDone)
	}()
	select {
	case <-freshDone:
		close(releaseStale)
		<-staleDone
		t.Fatal("fresh rebuild overtook an older in-flight rebuild")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseStale)
	<-staleDone
	<-freshDone
	entry, status := store.lookup("e1")
	if status != http.StatusOK || entry.Attachment == nil || entry.Attachment.RecordID != "r1" {
		t.Fatalf("final entry=%#v status=%d", entry, status)
	}
}

func TestV2EventEnginePathInvalidationReusesPanesAndProcessExitRemovesImmediately(t *testing.T) {
	store := newTerminalEntryStore("engine")
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation, 4)}
	processes := &fakeV2ProcessWatcher{events: make(chan int, 4)}
	inspection := liveInspection{
		panes: []paneBinding{{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}},
		valid: true, observedAt: time.Now(), files: processFiles{cwd: map[int]string{11: "/work"}, valid: true},
		snap: &procSnapshot{started: map[int]time.Time{11: time.Now()}, valid: true},
	}
	var mu sync.Mutex
	inspectCalls, buildCalls, invalidations := 0, 0, 0
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, PaneInterval: time.Hour,
		Inspect: func() liveInspection {
			mu.Lock()
			inspectCalls++
			mu.Unlock()
			return inspection
		},
		Build: func(current liveInspection) (v2SnapshotInput, error) {
			mu.Lock()
			buildCalls++
			mu.Unlock()
			entries := make([]v2EntryDraft, 0, len(current.panes))
			for _, pane := range current.panes {
				entries = append(entries, v2EntryDraft{Entry: testV2Entry("entry-" + pane.TmuxID)})
			}
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: entries}, nil
		},
		Invalidate: func(paths []string, fullScan bool) {
			mu.Lock()
			invalidations++
			mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	waitForV2Revision(t, store, "rv1_engine_1")
	changes, unsubscribe := store.subscribe(store.snapshot().Revision)
	defer unsubscribe()

	paths.events <- v2PathInvalidation{Paths: []string{"/work/session.jsonl"}}
	paths.events <- v2PathInvalidation{Paths: []string{"/work/session.jsonl"}}
	waitForV2Count(t, func() int { mu.Lock(); defer mu.Unlock(); return buildCalls }, 2)
	mu.Lock()
	if inspectCalls != 1 || invalidations != 1 {
		t.Fatalf("path invalidation inspect/build/invalidate=%d/%d/%d", inspectCalls, buildCalls, invalidations)
	}
	mu.Unlock()

	exitAt := time.Now()
	processes.events <- 11
	waitForV2Revision(t, store, "rv1_engine_2")
	change := receiveV2Change(t, changes)
	if len(change.Removed) != 1 || change.Removed[0].Reason != "process_exit" {
		t.Fatalf("agent exit removal=%#v", change.Removed)
	}
	if elapsed := time.Since(exitAt); elapsed >= time.Second {
		t.Fatalf("agent exit propagation=%s, want sub-second", elapsed)
	}
	if snapshot := store.snapshot(); len(snapshot.Entries) != 0 {
		t.Fatalf("exited process entry remained: %#v", snapshot.Entries)
	}
}

func TestV2PathInvalidationKeepsSelfValidatingMetadataCache(t *testing.T) {
	resetMetadataCacheForTest(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	metadataCacheMu.Lock()
	metadataCache[path] = metadataCacheEntry{size: 10, device: 1, inode: 2, record: &sessionRecord{}}
	metadataCacheMu.Unlock()

	invalidateV2RecordPaths([]string{path}, false)
	metadataCacheMu.Lock()
	_, targetedPresent := metadataCache[path]
	metadataCacheMu.Unlock()
	if !targetedPresent {
		t.Fatal("targeted FSEvent cleared metadata cache before stat self-validation")
	}
	invalidateV2RecordPaths(nil, true)
	metadataCacheMu.Lock()
	_, fullScanPresent := metadataCache[path]
	metadataCacheMu.Unlock()
	if !fullScanPresent {
		t.Fatal("FullScan cleared metadata cache and would force every file to reparse")
	}
}

func TestV2EventEngineCoalescesBurstDuringBuildIntoOneFollowup(t *testing.T) {
	store := newTerminalEntryStore("burst")
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation, 256)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	inspection := liveInspection{valid: true}
	var mu sync.Mutex
	buildCalls, invalidations := 0, 0
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes, PaneInterval: time.Hour,
		Inspect: func() liveInspection { return inspection },
		Build: func(liveInspection) (v2SnapshotInput, error) {
			mu.Lock()
			buildCalls++
			call := buildCalls
			mu.Unlock()
			if call == 2 {
				close(secondStarted)
				<-releaseSecond
			}
			return v2SnapshotInput{Host: v2Host{ID: "host"}}, nil
		},
		Invalidate: func([]string, bool) {
			mu.Lock()
			invalidations++
			mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	waitForV2Count(t, func() int { mu.Lock(); defer mu.Unlock(); return buildCalls }, 1)
	paths.events <- v2PathInvalidation{Paths: []string{"/work/first.jsonl"}}
	<-secondStarted
	for index := range 100 {
		paths.events <- v2PathInvalidation{Paths: []string{fmt.Sprintf("/work/%d.jsonl", index)}}
	}
	close(releaseSecond)
	waitForV2Count(t, func() int { mu.Lock(); defer mu.Unlock(); return buildCalls }, 3)
	time.Sleep(3 * v2PathInvalidationDebounce)
	mu.Lock()
	defer mu.Unlock()
	if buildCalls != 3 || invalidations != 2 {
		t.Fatalf("initial+burst builds=%d invalidations=%d want=3/2", buildCalls, invalidations)
	}
}

func TestV2EventEngineCommitsInitialBuildDespiteConcurrentInvalidation(t *testing.T) {
	store := newTerminalEntryStore("initial-stale")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls int
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store,
		Build: func(liveInspection) (v2SnapshotInput, error) {
			calls++
			if calls == 1 {
				close(firstStarted)
				<-releaseFirst
			} else {
				<-releaseSecond
			}
			return v2SnapshotInput{Host: v2Host{ID: "host"}}, nil
		},
	})
	t.Cleanup(func() {
		select {
		case <-releaseSecond:
		default:
			close(releaseSecond)
		}
	})
	inspection := liveInspection{valid: true}
	engine.scheduleSnapshot(inspection, "pane_gone")
	<-firstStarted
	engine.scheduleSnapshot(inspection, "pane_gone")
	close(releaseFirst)
	waitForV2Revision(t, store, "rv1_initial-stale_1")
}

func TestV2EventEngineWholePaneRemovalUsesPaneGone(t *testing.T) {
	store := newTerminalEntryStore("pane-gone")
	paths := &fakeV2PathWatcher{events: make(chan v2PathInvalidation)}
	processes := &fakeV2ProcessWatcher{events: make(chan int)}
	inspection := liveInspection{
		panes: []paneBinding{{Socket: "/tmp/test", TmuxID: "%0", PanePID: 10, Kind: "claude", ProcessPIDs: []int{11}}},
		valid: true,
	}
	engine := newV2EventEngine(v2EventEngineConfig{
		Store: store, PathWatcher: paths, ProcessWatcher: processes,
		Inspect: func() liveInspection { return inspection },
		Build: func(current liveInspection) (v2SnapshotInput, error) {
			entries := make([]v2EntryDraft, 0, len(current.panes))
			for _, pane := range current.panes {
				entries = append(entries, v2EntryDraft{Entry: testV2Entry("entry-" + pane.TmuxID)})
			}
			return v2SnapshotInput{Host: v2Host{ID: "host"}, Entries: entries}, nil
		},
		Invalidate: func([]string, bool) {},
	})
	engine.refreshPanes("pane_gone")
	deadline := time.Now().Add(time.Second)
	for len(store.snapshot().Entries) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(store.snapshot().Entries) != 1 {
		t.Fatal("timed out waiting for initial pane snapshot")
	}
	changes, unsubscribe := store.subscribe(store.snapshot().Revision)
	defer unsubscribe()
	inspection.panes = nil
	engine.refreshPanes("pane_gone")
	change := receiveV2Change(t, changes)
	if len(change.Removed) != 1 || change.Removed[0].Reason != "pane_gone" {
		t.Fatalf("whole pane removal=%#v", change.Removed)
	}
}

func receiveV2Change(t *testing.T, changes <-chan v2ChangeSet) v2ChangeSet {
	t.Helper()
	select {
	case change := <-changes:
		return change
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for store change")
		return v2ChangeSet{}
	}
}

type v2SSETestEvent struct {
	ID   string
	Type string
	Data map[string]any
}

func readV2SSEEvent(t *testing.T, response *http.Response, want string) v2SSETestEvent {
	t.Helper()
	return readV2SSEEventFromReader(t, bufio.NewReader(response.Body), want)
}

func readV2SSEEventFromReader(t *testing.T, reader *bufio.Reader, want string) v2SSETestEvent {
	t.Helper()
	current := v2SSETestEvent{Data: map[string]any{}}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("read SSE: %v", err)
			return current
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			current.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &current.Data); err != nil {
				t.Errorf("decode SSE data: %v", err)
			}
		case line == "":
			if current.Type == want {
				return current
			}
			current = v2SSETestEvent{Data: map[string]any{}}
		}
	}
}

func waitForV2Revision(t *testing.T, store *terminalEntryStore, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.snapshot().Revision == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("revision=%q want=%q", store.snapshot().Revision, want)
}

func waitForV2Count(t *testing.T, value func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("count=%d want>=%d", value(), want))
}
