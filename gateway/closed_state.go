package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type closedSessionStore struct {
	mu   sync.RWMutex
	path string
	ids  map[string]bool
}

type persistedClosedSessions struct {
	Version    int      `json:"version"`
	SessionIDs []string `json:"sessionIds"`
}

func newClosedSessionStore(path string) *closedSessionStore {
	store := &closedSessionStore{path: path, ids: map[string]bool{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) || path == "" {
		return store
	}
	if err != nil {
		return store
	}
	var persisted persistedClosedSessions
	if json.Unmarshal(data, &persisted) != nil || persisted.Version != 1 {
		return store
	}
	for _, sessionID := range persisted.SessionIDs {
		if uuidPattern.MatchString(sessionID) {
			store.ids[sessionID] = true
		}
	}
	return store
}

func (store *closedSessionStore) has(sessionID string) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.ids[sessionID]
}

func (store *closedSessionStore) add(sessionID string) error {
	if !uuidPattern.MatchString(sessionID) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.ids[sessionID] {
		return nil
	}
	ids := make([]string, 0, len(store.ids)+1)
	for id := range store.ids {
		ids = append(ids, id)
	}
	ids = append(ids, sessionID)
	sort.Strings(ids)
	if err := store.persist(ids); err != nil {
		return err
	}
	store.ids[sessionID] = true
	return nil
}

func (store *closedSessionStore) flush() error {
	store.mu.RLock()
	ids := make([]string, 0, len(store.ids))
	for id := range store.ids {
		ids = append(ids, id)
	}
	store.mu.RUnlock()
	sort.Strings(ids)
	return store.persist(ids)
}

func (store *closedSessionStore) persist(ids []string) error {
	if store.path == "" {
		return fmt.Errorf("closed session state path is empty")
	}
	dir := filepath.Dir(store.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	data, err := json.MarshalIndent(persistedClosedSessions{Version: 1, SessionIDs: ids}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".closed-sessions-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpPath, store.path)
	}
	if err != nil {
		return err
	}
	return os.Chmod(store.path, 0o600)
}
