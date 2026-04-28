package bootopt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State tracks the full autoresearch session.
type State struct {
	SessionID       string             `json:"session_id"`
	StartedAt       time.Time          `json:"started_at"`
	CurrentIteration int               `json:"current_iteration"`
	BaselineMeanMs  int64              `json:"baseline_mean_ms"`
	Results         []HypothesisResult `json:"results"`
	KeptChanges     []KeptChange       `json:"kept_changes"`
}

// KeptChange tracks a change that was kept.
type KeptChange struct {
	Iteration   int       `json:"iteration"`
	Description string    `json:"description"`
	Diff        string    `json:"diff"`
	MeanMs      int64     `json:"mean_ms"`
	SavedMs     int64     `json:"saved_ms"` // vs baseline
	CommittedAt time.Time `json:"committed_at"`
}

// StateManager handles persistence.
type StateManager struct {
	Dir string
}

// NewStateManager creates a state manager.
func NewStateManager(dir string) *StateManager {
	return &StateManager{Dir: dir}
}

// Load loads existing state or creates new.
func (sm *StateManager) Load(sessionID string) (*State, error) {
	path := sm.path(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{
				SessionID:   sessionID,
				StartedAt:   time.Now(),
				KeptChanges: []KeptChange{},
			}, nil
		}
		return nil, err
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &s, nil
}

// Save persists state.
func (sm *StateManager) Save(s *State) error {
	path := sm.path(s.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// path returns the state file path.
func (sm *StateManager) path(sessionID string) string {
	return filepath.Join(sm.Dir, fmt.Sprintf("bootopt-%s.json", sessionID))
}

// Summary returns a human-readable summary.
func (s *State) Summary() string {
	var totalSaved int64
	for _, k := range s.KeptChanges {
		totalSaved += k.SavedMs
	}

	return fmt.Sprintf(
		"Session %s: %d iterations, %d kept changes, %dms total saved vs baseline\n",
		s.SessionID, s.CurrentIteration, len(s.KeptChanges), totalSaved,
	)
}
