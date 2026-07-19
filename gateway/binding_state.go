package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	activeWriteWindow  = 30 * time.Second
	maxWriteBindingAge = 5 * time.Second
)

type fileWritePoint struct {
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	MtimeNano int64  `json:"mtimeNano"`
	Size      int64  `json:"size"`
}

type writeObservation struct {
	point       fileWritePoint
	lastAdvance time.Time
	abnormal    bool
	growthCount int
}

type stickyBinding struct {
	PanePID        int            `json:"panePid"`
	ProcessStarted int64          `json:"processStarted"`
	Socket         string         `json:"socket"`
	TmuxID         string         `json:"tmuxId"`
	Kind           string         `json:"kind"`
	Cwd            string         `json:"cwd"`
	SessionID      string         `json:"sessionId"`
	SessionFile    string         `json:"sessionFile"`
	Evidence       string         `json:"evidence,omitempty"`
	Quarantined    bool           `json:"quarantined,omitempty"`
	LastWritePoint fileWritePoint `json:"lastWritePoint"`
	ConfirmedAt    int64          `json:"confirmedAt"`
}

type pendingSwitch struct {
	fingerprint string
	rounds      int
}

type weakBindingResult struct {
	paneReasons   map[int]string
	recordReasons map[string]string
	readOnly      map[int]stickyBinding
}

type weakBindingTracker struct {
	mu       sync.Mutex
	path     string
	sticky   map[string]stickyBinding
	observed map[string]writeObservation
	pending  map[string]pendingSwitch
}

type persistedWeakBindings struct {
	Version int             `json:"version"`
	Entries []stickyBinding `json:"entries"`
}

func newWeakBindingTracker(path string) *weakBindingTracker {
	tracker := &weakBindingTracker{
		path:     path,
		sticky:   map[string]stickyBinding{},
		observed: map[string]writeObservation{},
		pending:  map[string]pendingSwitch{},
	}
	if path == "" {
		return tracker
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("binding state load failed: %v", err)
		}
		return tracker
	}
	var stored persistedWeakBindings
	if json.Unmarshal(data, &stored) != nil || stored.Version != 1 {
		log.Printf("binding state ignored: invalid format in %s", path)
		return tracker
	}
	for _, entry := range stored.Entries {
		if entry.PanePID > 0 && entry.ProcessStarted != 0 && entry.SessionID != "" {
			if entry.Evidence == "" {
				entry.Evidence = "active_writer"
			}
			tracker.sticky[stickyKey(entry.PanePID, entry.ProcessStarted)] = entry
		}
	}
	return tracker
}

func stickyKey(panePID int, processStarted int64) string {
	return fmt.Sprintf("%d:%d", panePID, processStarted)
}

func recordWritePoint(record *sessionRecord) fileWritePoint {
	return fileWritePoint{Device: record.device, Inode: record.inode, MtimeNano: record.mtime.UnixNano(), Size: record.size}
}

func sameFileIdentity(a, b fileWritePoint) bool {
	return a.Device == b.Device && a.Inode == b.Inode
}

func writePointAdvanced(previous, current fileWritePoint) bool {
	return sameFileIdentity(previous, current) && current.Size >= previous.Size &&
		(current.MtimeNano > previous.MtimeNano || current.Size > previous.Size)
}

func (tracker *weakBindingTracker) observe(records []*sessionRecord, now time.Time) {
	present := make(map[string]bool, len(records))
	for _, record := range records {
		key := record.Kind + "\x00" + record.SessionID
		present[key] = true
		current := recordWritePoint(record)
		observation, ok := tracker.observed[key]
		if !ok {
			tracker.observed[key] = writeObservation{point: current}
			continue
		}
		if !sameFileIdentity(observation.point, current) || current.Size < observation.point.Size {
			observation.point = current
			observation.lastAdvance = time.Time{}
			observation.abnormal = true
			observation.growthCount = 0
			tracker.observed[key] = observation
			continue
		}
		if writePointAdvanced(observation.point, current) {
			observation.point = current
			if observation.abnormal {
				observation.growthCount++
				if observation.growthCount >= 2 {
					observation.abnormal = false
					observation.lastAdvance = now
				}
			} else {
				observation.lastAdvance = now
			}
		}
		tracker.observed[key] = observation
	}
	for key := range tracker.observed {
		if !present[key] {
			delete(tracker.observed, key)
		}
	}
}

func (tracker *weakBindingTracker) isActive(record *sessionRecord, now time.Time) bool {
	observation := tracker.observed[record.Kind+"\x00"+record.SessionID]
	if observation.abnormal {
		return false
	}
	if !record.mtime.Before(now.Add(-activeWriteWindow)) {
		return true
	}
	return !observation.lastAdvance.IsZero() && now.Sub(observation.lastAdvance) <= activeWriteWindow
}

func (tracker *weakBindingTracker) isAbnormal(record *sessionRecord) bool {
	return tracker.observed[record.Kind+"\x00"+record.SessionID].abnormal
}

func paneStickyIdentity(pane *paneBinding, files processFiles, snap *procSnapshot) (string, string, time.Time) {
	cwd, started := fallbackPaneIdentity(pane, files, snap)
	if pane.PanePID <= 0 || cwd == "" || started.IsZero() {
		return "", cwd, started
	}
	return stickyKey(pane.PanePID, started.UnixNano()), cwd, started
}

func (tracker *weakBindingTracker) paneStickyIdentity(pane *paneBinding, files processFiles, snap *procSnapshot) (string, string, time.Time) {
	key, cwd, started := paneStickyIdentity(pane, files, snap)
	if key != "" {
		return key, cwd, started
	}
	for storedKey, entry := range tracker.sticky {
		if entry.Socket != pane.Socket || entry.TmuxID != pane.TmuxID || entry.PanePID != pane.PanePID || entry.Kind != pane.Kind {
			continue
		}
		if cwd != "" && cwd != entry.Cwd {
			return "", cwd, started
		}
		if !started.IsZero() && started.UnixNano() != entry.ProcessStarted {
			return "", cwd, started
		}
		if cwd == "" {
			cwd = entry.Cwd
		}
		if started.IsZero() {
			started = time.Unix(0, entry.ProcessStarted)
		}
		return storedKey, cwd, started
	}
	return "", cwd, started
}

func validSticky(entry stickyBinding, pane *paneBinding, cwd string, started time.Time, record *sessionRecord) bool {
	return entry.PanePID == pane.PanePID && entry.ProcessStarted == started.UnixNano() &&
		entry.Socket == pane.Socket && entry.TmuxID == pane.TmuxID && entry.Kind == pane.Kind &&
		entry.Cwd == cwd && record != nil && entry.SessionID == record.SessionID &&
		entry.SessionFile == record.SessionFile && record.cwdHistory[cwd]
}

func makeSticky(pane *paneBinding, cwd string, started time.Time, record *sessionRecord, evidence string, now time.Time) stickyBinding {
	return stickyBinding{
		PanePID: pane.PanePID, ProcessStarted: started.UnixNano(), Socket: pane.Socket, TmuxID: pane.TmuxID,
		Kind: pane.Kind, Cwd: cwd, SessionID: record.SessionID, SessionFile: record.SessionFile, Evidence: evidence,
		LastWritePoint: recordWritePoint(record), ConfirmedAt: now.UnixNano(),
	}
}

func (tracker *weakBindingTracker) apply(records []*sessionRecord, panes []paneBinding, reserved map[int]bool, files processFiles, snap *procSnapshot, bound map[string]bool, history map[string]claudeHistoryEntry, bind func(*paneBinding, string, string) bool, now time.Time, recoveryID string) weakBindingResult {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.observe(records, now)
	result := weakBindingResult{paneReasons: map[int]string{}, recordReasons: map[string]string{}, readOnly: map[int]stickyBinding{}}

	recordByID := make(map[string]*sessionRecord, len(records))
	for _, record := range records {
		recordByID[record.Kind+"\x00"+record.SessionID] = record
	}
	present := map[string]bool{}
	groups := map[string][]int{}
	changed := false
	for i := range panes {
		pane := &panes[i]
		key, cwd, _ := tracker.paneStickyIdentity(pane, files, snap)
		if key == "" {
			continue
		}
		present[key] = true
		if reserved[i] {
			// A strong observation is arbitrated before apply. Keep the prior
			// sticky entry during its first conflicting round so a transient
			// argv/env observation cannot destroy a stable binding.
			continue
		}
		if pane.Kind == "claude" {
			groups[cwd] = append(groups[cwd], i)
		}
	}
	for key := range tracker.sticky {
		if !present[key] {
			delete(tracker.sticky, key)
			delete(tracker.pending, key)
			changed = true
		}
	}

	cwds := make([]string, 0, len(groups))
	for cwd := range groups {
		cwds = append(cwds, cwd)
	}
	sort.Strings(cwds)
	for _, cwd := range cwds {
		indexes := groups[cwd]
		if len(indexes) != 1 {
			for _, index := range indexes {
				result.paneReasons[index] = "multi_pane_ambiguous"
				if key, _, started := tracker.paneStickyIdentity(&panes[index], files, snap); key != "" {
					entry, hasSticky := tracker.sticky[key]
					record := recordByID[entry.Kind+"\x00"+entry.SessionID]
					if hasSticky && validSticky(entry, &panes[index], cwd, started, record) && entry.Quarantined && stableSessionID(entry.SessionID) != recoveryID {
						result.readOnly[index] = entry
						result.recordReasons[entry.Kind+"\x00"+entry.SessionID] = "delivery_reconciliation_failed"
					} else if hasSticky && validSticky(entry, &panes[index], cwd, started, record) && !entry.Quarantined {
						// Another pane with the same cwd does not contradict this pane's
						// validated process identity. Only stronger pane-specific evidence
						// may displace an established sticky attachment.
						delete(tracker.pending, key)
						bind(&panes[index], entry.SessionID, entry.Evidence)
					}
				}
			}
			for _, record := range records {
				if record.Kind == "claude" && record.cwdHistory[cwd] && !bound[record.Kind+"\x00"+record.SessionID] {
					result.recordReasons[record.Kind+"\x00"+record.SessionID] = "multi_pane_ambiguous"
				}
			}
			continue
		}
		pane := &panes[indexes[0]]
		key, _, started := tracker.paneStickyIdentity(pane, files, snap)
		entry, hasSticky := tracker.sticky[key]
		stickyRecord := recordByID[entry.Kind+"\x00"+entry.SessionID]
		if hasSticky && !validSticky(entry, pane, cwd, started, stickyRecord) {
			delete(tracker.sticky, key)
			delete(tracker.pending, key)
			hasSticky = false
			stickyRecord = nil
			changed = true
		}
		recovering := hasSticky && entry.Quarantined && stableSessionID(entry.SessionID) == recoveryID
		if hasSticky && entry.Quarantined && !recovering {
			delete(tracker.pending, key)
			result.paneReasons[indexes[0]] = "delivery_reconciliation_failed"
			result.recordReasons[entry.Kind+"\x00"+entry.SessionID] = "delivery_reconciliation_failed"
			result.readOnly[indexes[0]] = entry
			continue
		}
		if recovering {
			// Explicit recovery must prove the binding again from fresh evidence;
			// the quarantined sticky entry alone is not sufficient.
			hasSticky = false
			stickyRecord = nil
		}

		var active []*sessionRecord
		for _, record := range records {
			recordKey := record.Kind + "\x00" + record.SessionID
			if record.Kind != "claude" || bound[recordKey] || !record.cwdHistory[cwd] || !record.mtime.After(started) {
				continue
			}
			if tracker.isActive(record, now) {
				active = append(active, record)
			}
		}
		sort.Slice(active, func(i, j int) bool { return active[i].SessionID < active[j].SessionID })
		if len(active) > 1 {
			result.paneReasons[indexes[0]] = "multi_candidate_ambiguous"
			for _, record := range active {
				result.recordReasons[record.Kind+"\x00"+record.SessionID] = "multi_candidate_ambiguous"
			}
			ids := make([]string, 0, len(active))
			for _, record := range active {
				ids = append(ids, record.SessionID)
			}
			if hasSticky && !tracker.conflictConfirmed(key, "multi_candidate:"+strings.Join(ids, ",")) {
				bind(pane, stickyRecord.SessionID, entry.Evidence)
			} else if hasSticky {
				delete(tracker.sticky, key)
				changed = true
			}
			continue
		}
		if len(active) == 0 {
			delete(tracker.pending, key)
			if hasSticky && !tracker.isAbnormal(stickyRecord) {
				bind(pane, stickyRecord.SessionID, entry.Evidence)
				continue
			}
			historyEntry, ok := history[cwd]
			if !ok || historyEntry.Ambiguous || historyEntry.SessionID == "" {
				result.paneReasons[indexes[0]] = "no_session_attribution"
				continue
			}
			historyRecord := recordByID["claude\x00"+historyEntry.SessionID]
			if !historyEntry.At.After(started) {
				result.paneReasons[indexes[0]] = "evidence_stale"
				if historyRecord != nil {
					result.recordReasons["claude\x00"+historyEntry.SessionID] = "evidence_stale"
				}
				continue
			}
			if historyRecord == nil || historyRecord.Cwd != cwd || bound["claude\x00"+historyEntry.SessionID] {
				result.paneReasons[indexes[0]] = "no_session_attribution"
				continue
			}
			if _, err := os.Stat(historyRecord.SessionFile); err != nil {
				result.paneReasons[indexes[0]] = "evidence_stale"
				result.recordReasons["claude\x00"+historyEntry.SessionID] = "evidence_stale"
				continue
			}
			if bind(pane, historyRecord.SessionID, "history") {
				tracker.sticky[key] = makeSticky(pane, cwd, started, historyRecord, "history", now)
				changed = true
			}
			continue
		}

		candidate := active[0]
		if !hasSticky {
			if bind(pane, candidate.SessionID, "active_writer") {
				tracker.sticky[key] = makeSticky(pane, cwd, started, candidate, "active_writer", now)
				delete(tracker.pending, key)
				changed = true
			}
			continue
		}
		if entry.SessionID == candidate.SessionID {
			delete(tracker.pending, key)
			if bind(pane, candidate.SessionID, entry.Evidence) {
				point := recordWritePoint(candidate)
				if point != entry.LastWritePoint {
					entry.LastWritePoint = point
					tracker.sticky[key] = entry
					changed = true
				}
			}
			continue
		}

		if tracker.conflictConfirmed(key, "switch:"+candidate.SessionID) {
			if bind(pane, candidate.SessionID, "active_writer") {
				tracker.sticky[key] = makeSticky(pane, cwd, started, candidate, "active_writer", now)
				delete(tracker.pending, key)
				changed = true
			}
		}
	}
	if changed {
		tracker.persistLocked()
	}
	return result
}

func (tracker *weakBindingTracker) conflictConfirmed(key, fingerprint string) bool {
	pending := tracker.pending[key]
	if pending.fingerprint != fingerprint {
		tracker.pending[key] = pendingSwitch{fingerprint: fingerprint, rounds: 1}
		return false
	}
	pending.rounds++
	tracker.pending[key] = pending
	return pending.rounds >= 2
}

func (tracker *weakBindingTracker) allowStrong(pane *paneBinding, matchID string, files processFiles, snap *procSnapshot, recoveryID string) bool {
	key, cwd, started := paneStickyIdentity(pane, files, snap)
	if key == "" {
		return true
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	entry, ok := tracker.sticky[key]
	if !ok {
		return true
	}
	if entry.PanePID != pane.PanePID || entry.ProcessStarted != started.UnixNano() || entry.Socket != pane.Socket || entry.TmuxID != pane.TmuxID || entry.Kind != pane.Kind || entry.Cwd != cwd {
		delete(tracker.sticky, key)
		delete(tracker.pending, key)
		tracker.persistLocked()
		return true
	}
	if entry.SessionID == matchID {
		delete(tracker.pending, key)
		if entry.Quarantined {
			if stableSessionID(entry.SessionID) != recoveryID {
				return false
			}
			entry.Quarantined = false
			tracker.sticky[key] = entry
			tracker.persistLocked()
		}
		return true
	}
	if !tracker.conflictConfirmed(key, "strong:"+matchID) {
		return false
	}
	delete(tracker.sticky, key)
	delete(tracker.pending, key)
	tracker.persistLocked()
	return true
}

func (tracker *weakBindingTracker) conflictingSticky(claimant *paneBinding, sessionID string, panes []paneBinding, files processFiles, snap *procSnapshot) (stickyBinding, bool) {
	claimantKey, _, _ := paneStickyIdentity(claimant, files, snap)
	live := make(map[string]bool, len(panes))
	for index := range panes {
		if key, _, _ := paneStickyIdentity(&panes[index], files, snap); key != "" {
			live[key] = true
		}
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for key, entry := range tracker.sticky {
		if key != claimantKey && live[key] && !entry.Quarantined && entry.Kind == claimant.Kind && entry.SessionID == sessionID {
			return entry, true
		}
	}
	return stickyBinding{}, false
}

func (tracker *weakBindingTracker) rememberStrong(pane *paneBinding, record *sessionRecord, files processFiles, snap *procSnapshot, now time.Time) {
	key, cwd, started := paneStickyIdentity(pane, files, snap)
	if key == "" || record == nil || !record.cwdHistory[cwd] {
		return
	}
	next := makeSticky(pane, cwd, started, record, "strong", now)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	changed := false
	for otherKey, entry := range tracker.sticky {
		if otherKey == key || entry.Kind != record.Kind || entry.SessionID != record.SessionID || entry.Quarantined {
			continue
		}
		delete(tracker.sticky, otherKey)
		delete(tracker.pending, otherKey)
		changed = true
	}
	if current, ok := tracker.sticky[key]; ok && current.Quarantined {
		if changed {
			tracker.persistLocked()
		}
		return
	} else if ok {
		next.ConfirmedAt = current.ConfirmedAt
	}
	if current, ok := tracker.sticky[key]; !ok || current != next {
		tracker.sticky[key] = next
		changed = true
	}
	delete(tracker.pending, key)
	if changed {
		tracker.persistLocked()
	}
}

func (tracker *weakBindingTracker) isQuarantined(kind, sessionID string) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for _, entry := range tracker.sticky {
		if entry.Kind == kind && entry.SessionID == sessionID && entry.Quarantined {
			return true
		}
	}
	return false
}

func (tracker *weakBindingTracker) quarantinedEntry(pane *paneBinding, record *sessionRecord, files processFiles, snap *procSnapshot) (stickyBinding, bool) {
	key, cwd, started := paneStickyIdentity(pane, files, snap)
	if key == "" || record == nil {
		return stickyBinding{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	entry, ok := tracker.sticky[key]
	if !ok || !entry.Quarantined || !validSticky(entry, pane, cwd, started, record) {
		return stickyBinding{}, false
	}
	return entry, true
}

func (tracker *weakBindingTracker) quarantine(pane *paneBinding, record *sessionRecord) {
	if pane == nil || record == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for key, entry := range tracker.sticky {
		if entry.PanePID == pane.PanePID && entry.Socket == pane.Socket && entry.TmuxID == pane.TmuxID && entry.SessionID == record.SessionID {
			entry.Quarantined = true
			tracker.sticky[key] = entry
			delete(tracker.pending, key)
			tracker.persistLocked()
			return
		}
	}
}

func (tracker *weakBindingTracker) hintKeys() map[string]bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	hints := make(map[string]bool, len(tracker.sticky))
	for _, entry := range tracker.sticky {
		hints[entry.Kind+"\x00"+entry.SessionID] = true
	}
	return hints
}

func (tracker *weakBindingTracker) persistLocked() {
	if tracker.path == "" {
		return
	}
	dir := filepath.Dir(tracker.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("binding state mkdir failed: %v", err)
		return
	}
	_ = os.Chmod(dir, 0o700)
	stored := persistedWeakBindings{Version: 1}
	for _, entry := range tracker.sticky {
		stored.Entries = append(stored.Entries, entry)
	}
	sort.Slice(stored.Entries, func(i, j int) bool {
		if stored.Entries[i].PanePID == stored.Entries[j].PanePID {
			return stored.Entries[i].ProcessStarted < stored.Entries[j].ProcessStarted
		}
		return stored.Entries[i].PanePID < stored.Entries[j].PanePID
	})
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".bindings-*")
	if err != nil {
		log.Printf("binding state temp failed: %v", err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_ = tmp.Chmod(0o600)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpPath, tracker.path)
	}
	if err != nil {
		log.Printf("binding state persist failed: %v", err)
		return
	}
	_ = os.Chmod(tracker.path, 0o600)
}
