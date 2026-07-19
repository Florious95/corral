package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInheritedEnvCannotStealActiveWriterStickyFromLivePane(t *testing.T) {
	now := time.Now()
	sessionID := "77777777-7777-4777-8777-777777777777"
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	incumbent := paneBinding{Socket: "/tmux/leader", TmuxID: "%0", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}}
	claimant := paneBinding{Socket: "/tmux/test", TmuxID: "%0", PanePID: 14, Kind: "claude", ProcessPIDs: []int{140}}
	inspection := weakTestInspection(now.Add(-time.Hour), incumbent, claimant)
	tracker := newWeakBindingTracker("")
	key, cwd, started := paneStickyIdentity(&inspection.panes[0], inspection.files, inspection.snap)
	tracker.sticky[key] = makeSticky(&inspection.panes[0], cwd, started, record, "active_writer", now)

	oldHint := bindingClaudeHint
	bindingClaudeHint = func(pid int, _ string) (string, string) {
		if pid == 140 {
			return sessionID, "env"
		}
		return "", ""
	}
	t.Cleanup(func() { bindingClaudeHint = oldHint })
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	bindingDecisionMu.Lock()
	delete(bindingIncidents, "claude\x00"+sessionID)
	bindingDecisionMu.Unlock()

	outcome := bindInspectionDetailed([]*sessionRecord{record}, inspection, tracker, nil, now.Add(time.Second))
	if outcome.bound != 1 || record.binding == nil || record.binding.Socket != incumbent.Socket || record.bindingEvidence != "active_writer" || !record.CanSend {
		t.Fatalf("inherited env stole binding: outcome=%#v record=%#v", outcome, record)
	}
	if !strings.Contains(logs.String(), "BINDING INCIDENT: type=inherited_env_conflict") || !strings.Contains(logs.String(), "action=blocked") {
		t.Fatalf("incident log missing: %s", logs.String())
	}
}

func applyWeakForTest(tracker *weakBindingTracker, records []*sessionRecord, inspection liveInspection, reserved map[int]bool, now time.Time) map[string]string {
	bound := map[string]bool{}
	matches := map[string]string{}
	tracker.apply(records, inspection.panes, reserved, inspection.files, inspection.snap, bound, nil, func(pane *paneBinding, sessionID, _ string) bool {
		matches[sessionID] = pane.TmuxID
		bound["claude\x00"+sessionID] = true
		return true
	}, now, "")
	return matches
}

func weakTestInspection(started time.Time, panes ...paneBinding) liveInspection {
	cwd := map[int]string{}
	starts := map[int]time.Time{}
	commands := map[int]string{}
	for _, pane := range panes {
		for _, pid := range pane.ProcessPIDs {
			cwd[pid] = "/work"
			starts[pid] = started
			commands[pid] = "claude"
		}
	}
	return liveInspection{panes: panes, files: processFiles{cwd: cwd, open: map[int][]string{}, valid: true}, snap: &procSnapshot{command: commands, started: starts, valid: true}, valid: true}
}

func weakRecord(id string, mtime time.Time, size int64) *sessionRecord {
	return &sessionRecord{
		AgentSession: AgentSession{Kind: "claude", SessionID: id, SessionFile: "/sessions/" + id + ".jsonl"},
		mtime:        mtime,
		size:         size,
		inode:        1,
		cwdHistory:   map[string]bool{"/work": true},
	}
}

func TestWeakBindingINC1203TwoActiveWritersFailClosed(t *testing.T) {
	now := time.Now()
	strong := paneBinding{Socket: "/tmux", TmuxID: "%51", PanePID: 51, Kind: "claude", ProcessPIDs: []int{510}}
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	records := []*sessionRecord{weakRecord("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", now.Add(-time.Second), 10), weakRecord("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", now.Add(-2*time.Second), 10)}
	matches := applyWeakForTest(newWeakBindingTracker(""), records, weakTestInspection(now.Add(-time.Minute), strong, weak), map[int]bool{0: true}, now)
	if len(matches) != 0 {
		t.Fatalf("INC-1203 replay must fail closed, got %#v", matches)
	}
}

func TestWeakBindingUniqueActiveWriterBinds(t *testing.T) {
	now := time.Now()
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	active := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	records := []*sessionRecord{weakRecord(active, now.Add(-time.Second), 10), weakRecord("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", now.Add(-time.Minute), 10)}
	matches := applyWeakForTest(newWeakBindingTracker(""), records, weakTestInspection(now.Add(-2*time.Minute), weak), map[int]bool{}, now)
	if matches[active] != "%0" || len(matches) != 1 {
		t.Fatalf("unique active writer must bind, got %#v", matches)
	}
}

func TestWeakBindingSecondWriterNeedsTwoRoundsBeforeRevocation(t *testing.T) {
	now := time.Now()
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	a := weakRecord("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", now.Add(-time.Second), 10)
	b := weakRecord("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", now.Add(-time.Minute), 10)
	tracker := newWeakBindingTracker("")
	inspection := weakTestInspection(now.Add(-2*time.Minute), weak)
	if got := applyWeakForTest(tracker, []*sessionRecord{a, b}, inspection, map[int]bool{}, now); len(got) != 1 {
		t.Fatalf("initial bind=%#v", got)
	}
	b.mtime, b.size = now.Add(time.Second), 11
	if got := applyWeakForTest(tracker, []*sessionRecord{a, b}, inspection, map[int]bool{}, now.Add(2*time.Second)); got[a.SessionID] != "%0" {
		t.Fatalf("first contradictory round must preserve the established binding, got %#v", got)
	}
	if got := applyWeakForTest(tracker, []*sessionRecord{a, b}, inspection, map[int]bool{}, now.Add(3*time.Second)); len(got) != 0 {
		t.Fatalf("second contradictory round must revoke to read-only, got %#v", got)
	}
}

func TestWeakBindingIdleStickyKeepsBinding(t *testing.T) {
	now := time.Now()
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	tracker := newWeakBindingTracker("")
	inspection := weakTestInspection(now.Add(-2*time.Minute), weak)
	if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now); got[sessionID] != "%0" {
		t.Fatalf("initial bind=%#v", got)
	}
	if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now.Add(time.Minute)); got[sessionID] != "%0" {
		t.Fatalf("idle sticky must remain bound, got %#v", got)
	}
}

func TestWeakBindingPIDReuseDropsSticky(t *testing.T) {
	now := time.Now()
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	tracker := newWeakBindingTracker("")
	if got := applyWeakForTest(tracker, []*sessionRecord{record}, weakTestInspection(now.Add(-2*time.Minute), weak), map[int]bool{}, now); got[sessionID] != "%0" {
		t.Fatalf("initial bind=%#v", got)
	}
	record.mtime = now.Add(-time.Minute)
	if got := applyWeakForTest(tracker, []*sessionRecord{record}, weakTestInspection(now.Add(-30*time.Second), weak), map[int]bool{}, now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("PID reuse with a different start time must drop sticky, got %#v", got)
	}
}

func TestWeakBindingSameMtimeSizeGrowthIsActive(t *testing.T) {
	now := time.Now()
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, now.Add(-time.Minute), 10)
	tracker := newWeakBindingTracker("")
	inspection := weakTestInspection(now.Add(-2*time.Minute), weak)
	if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now); len(got) != 0 {
		t.Fatalf("old first observation must not bind, got %#v", got)
	}
	record.size = 11
	if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now.Add(5*time.Second)); got[sessionID] != "%0" {
		t.Fatalf("same-mtime size growth must count as active, got %#v", got)
	}
}

func TestWeakBindingAbnormalFileEpochNeedsTwoGrowthRounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*sessionRecord)
	}{
		{name: "inode replacement", mutate: func(record *sessionRecord) { record.inode = 2 }},
		{name: "size retreat", mutate: func(record *sessionRecord) { record.size = 5 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
			sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			record := weakRecord(sessionID, now.Add(-time.Second), 10)
			tracker := newWeakBindingTracker("")
			inspection := weakTestInspection(now.Add(-2*time.Minute), weak)
			if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now); got[sessionID] != "%0" {
				t.Fatalf("initial bind=%#v", got)
			}
			test.mutate(record)
			record.mtime = now.Add(time.Second)
			if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now.Add(time.Second)); len(got) != 0 {
				t.Fatalf("abnormal epoch must revoke sticky, got %#v", got)
			}
			record.size++
			if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now.Add(2*time.Second)); len(got) != 0 {
				t.Fatalf("first growth round must stay read-only, got %#v", got)
			}
			record.size++
			if got := applyWeakForTest(tracker, []*sessionRecord{record}, inspection, map[int]bool{}, now.Add(3*time.Second)); got[sessionID] != "%0" {
				t.Fatalf("second growth round must restore binding, got %#v", got)
			}
		})
	}
}

func TestWeakBindingSessionSwitchNeedsTwoAdvancingRounds(t *testing.T) {
	now := time.Now()
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	aID, bID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	a, b := weakRecord(aID, now.Add(-time.Second), 10), weakRecord(bID, now.Add(-time.Minute), 10)
	tracker := newWeakBindingTracker("")
	inspection := weakTestInspection(now.Add(-2*time.Minute), weak)
	if got := applyWeakForTest(tracker, []*sessionRecord{a, b}, inspection, map[int]bool{}, now); got[aID] != "%0" {
		t.Fatalf("initial bind=%#v", got)
	}
	a.mtime, b.mtime, b.size = now.Add(-time.Minute), now.Add(40*time.Second), 11
	if got := applyWeakForTest(tracker, []*sessionRecord{a, b}, inspection, map[int]bool{}, now.Add(41*time.Second)); len(got) != 0 {
		t.Fatalf("first switch round must fail closed, got %#v", got)
	}
	// The second consecutive observation confirms the stronger conflicting
	// evidence even if the write point has not advanced again in the same second.
	if got := applyWeakForTest(tracker, []*sessionRecord{a, b}, inspection, map[int]bool{}, now.Add(46*time.Second)); got[bID] != "%0" {
		t.Fatalf("second advancing round must confirm switch, got %#v", got)
	}
}

func TestStrongEvidenceTakeoverNeedsTwoRounds(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	aID, bID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	a, b := weakRecord(aID, now.Add(-time.Second), 10), weakRecord(bID, now.Add(-time.Hour), 10)
	tracker := newWeakBindingTracker("")
	initial := weakTestInspection(started, pane)
	if got := bindInspectionDetailed([]*sessionRecord{a, b}, initial, tracker, nil, now); got.bound != 1 || a.binding == nil {
		t.Fatalf("initial active binding failed: %#v", got)
	}
	strong := weakTestInspection(started, pane)
	strong.snap.command[10] = "claude --session-id " + bID
	first := bindInspectionDetailed([]*sessionRecord{a, b}, strong, tracker, nil, now.Add(time.Second))
	if first.bound != 0 || b.CanSend || b.BindingReason != "evidence_transition" {
		t.Fatalf("first strong takeover round must fail closed: %#v", b.AgentSession)
	}
	second := bindInspectionDetailed([]*sessionRecord{a, b}, strong, tracker, nil, now.Add(2*time.Second))
	if second.bound != 1 || b.binding == nil || b.bindingEvidence != "strong" {
		t.Fatalf("second strong takeover round must bind new session: %#v", b)
	}
}

func TestHistoryBindingSystemManagementShape(t *testing.T) {
	now := time.Now()
	started := now.Add(-2 * time.Hour)
	sessionID := "88888888-8888-4888-8888-888888888888"
	record := weakRecord(sessionID, now.Add(-time.Hour), 10)
	record.Cwd = "/Users/example/Projects/system-console"
	record.cwdHistory = map[string]bool{record.Cwd: true}
	record.SessionFile = filepath.Join(t.TempDir(), sessionID+".jsonl")
	if err := os.WriteFile(record.SessionFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := liveInspection{
		panes: []paneBinding{pane},
		files: processFiles{cwd: map[int]string{10: record.Cwd}, open: map[int][]string{}, valid: true},
		snap:  &procSnapshot{command: map[int]string{10: "claude"}, started: map[int]time.Time{10: started}, valid: true},
		valid: true,
	}
	history := map[string]claudeHistoryEntry{record.Cwd: {SessionID: sessionID, At: started.Add(time.Minute)}}
	outcome := bindInspectionDetailed([]*sessionRecord{record}, inspection, newWeakBindingTracker(""), history, now)
	if outcome.bound != 1 || record.binding == nil || record.binding.TmuxID != "%0" || record.bindingEvidence != "history" {
		t.Fatalf("unique history owner must bind on first round: bound=%d record=%#v evidence=%q", outcome.bound, record, record.bindingEvidence)
	}
	if !record.Live || !record.CanSend || record.BindingReason != "" {
		t.Fatalf("history-bound live pane must be visible and sendable: %#v", record.AgentSession)
	}
}

func TestHistoryBindingRequiresFreshUniquePaneAndExistingFile(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Hour)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	baseRecord := func() *sessionRecord {
		record := weakRecord(sessionID, now.Add(-2*time.Hour), 10)
		record.Cwd = "/work"
		record.cwdHistory = map[string]bool{"/work": true}
		record.SessionFile = filepath.Join(t.TempDir(), sessionID+".jsonl")
		if err := os.WriteFile(record.SessionFile, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return record
	}
	pane := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	inspection := weakTestInspection(started, pane)
	inspection.files.cwd[10] = "/work"

	t.Run("history predates process", func(t *testing.T) {
		record := baseRecord()
		outcome := bindInspectionDetailed([]*sessionRecord{record}, inspection, newWeakBindingTracker(""), map[string]claudeHistoryEntry{"/work": {SessionID: sessionID, At: started.Add(-time.Second)}}, now)
		if outcome.bound != 0 || record.CanSend || record.BindingReason != "evidence_stale" {
			t.Fatalf("stale history must stay read-only: %#v", record.AgentSession)
		}
	})

	t.Run("multiple unclaimed panes", func(t *testing.T) {
		record := baseRecord()
		other := paneBinding{Socket: "/tmux", TmuxID: "%1", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}}
		ambiguous := weakTestInspection(started, pane, other)
		outcome := bindInspectionDetailed([]*sessionRecord{record}, ambiguous, newWeakBindingTracker(""), map[string]claudeHistoryEntry{"/work": {SessionID: sessionID, At: started.Add(time.Second)}}, now)
		if outcome.bound != 0 || record.CanSend || record.BindingReason != "multi_pane_ambiguous" {
			t.Fatalf("multiple panes must fail closed: %#v", record.AgentSession)
		}
	})

	t.Run("missing session file", func(t *testing.T) {
		record := baseRecord()
		if err := os.Remove(record.SessionFile); err != nil {
			t.Fatal(err)
		}
		outcome := bindInspectionDetailed([]*sessionRecord{record}, inspection, newWeakBindingTracker(""), map[string]claudeHistoryEntry{"/work": {SessionID: sessionID, At: started.Add(time.Second)}}, now)
		if outcome.bound != 0 || record.CanSend {
			t.Fatalf("missing history session file must stay read-only: %#v", record.AgentSession)
		}
	})
}

func TestMultiPaneHistoryCannotDisplaceValidActiveWriterSticky(t *testing.T) {
	now := time.Now()
	started := now.Add(-2 * time.Hour)
	stickyID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	historyID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	stickyRecord := weakRecord(stickyID, now.Add(-time.Hour), 10)
	historyRecord := weakRecord(historyID, now.Add(-time.Hour), 10)
	stickyPane := paneBinding{Socket: "/tmux", TmuxID: "%2", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}}
	otherPane := paneBinding{Socket: "/tmux", TmuxID: "%14", PanePID: 14, Kind: "claude", ProcessPIDs: []int{140}}
	inspection := weakTestInspection(started, stickyPane, otherPane)
	tracker := newWeakBindingTracker("")
	key, cwd, observedStart := paneStickyIdentity(&inspection.panes[0], inspection.files, inspection.snap)
	tracker.sticky[key] = makeSticky(&inspection.panes[0], cwd, observedStart, stickyRecord, "active_writer", now)
	history := map[string]claudeHistoryEntry{cwd: {SessionID: historyID, At: started.Add(time.Minute)}}

	for round := 1; round <= 3; round++ {
		outcome := bindInspectionDetailed([]*sessionRecord{stickyRecord, historyRecord}, inspection, tracker, history, now.Add(time.Duration(round)*time.Second))
		if outcome.bound != 1 || stickyRecord.binding == nil || stickyRecord.binding.TmuxID != "%2" || stickyRecord.bindingEvidence != "active_writer" || !stickyRecord.CanSend {
			t.Fatalf("round %d lost valid sticky: outcome=%#v record=%#v", round, outcome, stickyRecord)
		}
		if historyRecord.binding != nil || historyRecord.CanSend {
			t.Fatalf("round %d history stole an existing sticky attachment: %#v", round, historyRecord)
		}
	}
}

func TestMultiPaneStickySurvivesOneIncompleteIdentityRound(t *testing.T) {
	now := time.Now()
	started := now.Add(-2 * time.Hour)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, now.Add(-time.Hour), 10)
	leader := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}}
	worker := paneBinding{Socket: "/tmux", TmuxID: "%28", PanePID: 28, Kind: "claude", ProcessPIDs: []int{280}}
	inspection := weakTestInspection(started, leader, worker)
	tracker := newWeakBindingTracker("")
	key, cwd, observedStart := paneStickyIdentity(&inspection.panes[0], inspection.files, inspection.snap)
	tracker.sticky[key] = makeSticky(&inspection.panes[0], cwd, observedStart, record, "active_writer", now)

	delete(inspection.files.cwd, 20)
	delete(inspection.snap.started, 20)
	outcome := bindInspectionDetailed([]*sessionRecord{record}, inspection, tracker, nil, now.Add(time.Second))
	if outcome.bound != 1 || record.binding == nil || record.binding.TmuxID != "%0" || record.bindingEvidence != "active_writer" || !record.CanSend {
		t.Fatalf("incomplete identity round dropped live sticky: outcome=%#v record=%#v", outcome, record)
	}
	if len(tracker.sticky) != 1 {
		t.Fatalf("incomplete identity round deleted sticky: %#v", tracker.sticky)
	}
}

func TestStrongAttachmentBecomesStickyAndEvictsHistoryFromOtherPane(t *testing.T) {
	now := time.Now()
	started := now.Add(-2 * time.Hour)
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, now.Add(-time.Hour), 10)
	historyPane := paneBinding{Socket: "/tmux", TmuxID: "%2", PanePID: 2, Kind: "claude", ProcessPIDs: []int{20}}
	strongPane := paneBinding{Socket: "/tmux", TmuxID: "%14", PanePID: 14, Kind: "claude", ProcessPIDs: []int{140}}
	inspection := weakTestInspection(started, historyPane, strongPane)
	tracker := newWeakBindingTracker("")
	key, cwd, observedStart := paneStickyIdentity(&inspection.panes[0], inspection.files, inspection.snap)
	tracker.sticky[key] = makeSticky(&inspection.panes[0], cwd, observedStart, record, "history", now)
	inspection.snap.command[140] = "claude --session-id " + sessionID

	first := bindInspectionDetailed([]*sessionRecord{record}, inspection, tracker, nil, now.Add(time.Second))
	if first.bound != 1 || record.binding == nil || record.binding.TmuxID != "%14" || record.bindingEvidence != "strong" {
		t.Fatalf("strong attachment did not win: outcome=%#v record=%#v", first, record)
	}

	inspection.snap.command[140] = "claude"
	second := bindInspectionDetailed([]*sessionRecord{record}, inspection, tracker, nil, now.Add(2*time.Second))
	if second.bound != 1 || record.binding == nil || record.binding.TmuxID != "%14" || record.bindingEvidence != "strong" {
		t.Fatalf("strong attachment was not sticky after transient evidence loss: outcome=%#v record=%#v", second, record)
	}
}

func TestPaneBackedPlaceholderIsStableAndVisible(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Hour)
	pane := paneBinding{Socket: "/tmux", TmuxID: "%7", PanePID: 7, Kind: "claude", ProcessPIDs: []int{70}}
	inspection := liveInspection{
		panes: []paneBinding{pane},
		files: processFiles{cwd: map[int]string{70: "/Users/example/Projects/system-console"}, open: map[int][]string{}, valid: true},
		snap:  &procSnapshot{command: map[int]string{70: "claude"}, started: map[int]time.Time{70: started}, valid: true},
		valid: true,
	}
	first := bindInspectionDetailed(nil, inspection, newWeakBindingTracker(""), nil, now)
	second := bindInspectionDetailed(nil, inspection, newWeakBindingTracker(""), nil, now.Add(time.Second))
	if len(first.records) != 1 || len(second.records) != 1 || first.records[0].ID != second.records[0].ID {
		t.Fatalf("placeholder must be stable: first=%#v second=%#v", first.records, second.records)
	}
	placeholder := first.records[0]
	if placeholder.Title != "system-console · Claude" || !placeholder.Live || placeholder.CanSend || placeholder.BindingReason != "no_session_attribution" {
		t.Fatalf("unexpected placeholder: %#v", placeholder.AgentSession)
	}
}

func TestWeakBindingPersistenceValidatesProcessStart(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "bindings.json")
	weak := paneBinding{Socket: "/tmux", TmuxID: "%0", PanePID: 1, Kind: "claude", ProcessPIDs: []int{10}}
	sessionID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record := weakRecord(sessionID, now.Add(-time.Second), 10)
	started := now.Add(-2 * time.Minute)
	if got := applyWeakForTest(newWeakBindingTracker(path), []*sessionRecord{record}, weakTestInspection(started, weak), map[int]bool{}, now); got[sessionID] != "%0" {
		t.Fatalf("initial persisted bind=%#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("binding cache mode=%o, want 600", info.Mode().Perm())
	}
	record.mtime = now.Add(-time.Minute)
	if got := applyWeakForTest(newWeakBindingTracker(path), []*sessionRecord{record}, weakTestInspection(started, weak), map[int]bool{}, now.Add(time.Minute)); got[sessionID] != "%0" {
		t.Fatalf("validated restart must restore sticky, got %#v", got)
	}
	if got := applyWeakForTest(newWeakBindingTracker(path), []*sessionRecord{record}, weakTestInspection(started.Add(time.Second), weak), map[int]bool{}, now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("restart with mismatched process start must discard sticky, got %#v", got)
	}
}
