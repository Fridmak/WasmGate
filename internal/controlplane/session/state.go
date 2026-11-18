package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"wasmgate/internal/controlplane/release"
)

// appliedState is the release the data plane currently runs. It is persisted so
// Hello and Heartbeat can report it immediately after a restart.
type AppliedState = release.AppliedState
type appliedState = release.AppliedState

// StateStore guards the applied state shared with the heartbeat goroutine and
// knows where the state is persisted.
type StateStore struct {
	mu  sync.RWMutex
	dir string
	st  appliedState
}

// NewStateStore creates a state store initialized with the last persisted value.
// dir may be empty for callers that only need an in-memory state.
func NewStateStore(dir string, st AppliedState) *StateStore {
	return &StateStore{dir: dir, st: st}
}

// get returns a copy of the current state.
func (s *StateStore) Get() AppliedState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st
}

// set replaces the current state in memory. It does not touch the file system;
// callers persist it through Save after a successful apply.
func (s *StateStore) Set(st AppliedState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = st
}

// Save persists the current state when the store has a directory. An in-memory
// store is a no-op.
func (s *StateStore) Save() error {
	if s.dir == "" {
		return nil
	}
	return SaveApplied(s.dir, s.Get())
}

// appliedStateFile is the file name used to persist appliedState.
const appliedStateFile = "applied.json"

// loadApplied reads the persisted state, returning the zero value when it is
// missing or unreadable: a fresh agent simply has no active release yet.
func LoadApplied(dir string) AppliedState {
	var st appliedState
	raw, err := os.ReadFile(filepath.Join(dir, appliedStateFile)) // #nosec G304
	if err != nil {
		return st
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return appliedState{}
	}
	return st
}

// saveApplied persists the state for the next start.
func SaveApplied(dir string, st AppliedState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("control plane: marshal applied state: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("control plane: create state dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, appliedStateFile), raw, 0o644); err != nil { // #nosec G306 -- applied state is not secret
		return fmt.Errorf("control plane: write applied state: %w", err)
	}
	return nil
}
