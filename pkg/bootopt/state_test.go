package bootopt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateManager(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	// Load new
	state, err := sm.Load("test-session")
	if err != nil {
		t.Fatalf("load new: %v", err)
	}
	if state.SessionID != "test-session" {
		t.Errorf("session ID = %q, want test-session", state.SessionID)
	}

	// Modify and save
	state.BaselineMeanMs = 5000
	state.CurrentIteration = 3
	state.KeptChanges = []KeptChange{
		{Iteration: 1, Description: "test", SavedMs: 500},
	}
	if err := sm.Save(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Load existing
	state2, err := sm.Load("test-session")
	if err != nil {
		t.Fatalf("load existing: %v", err)
	}
	if state2.BaselineMeanMs != 5000 {
		t.Errorf("baseline = %d, want 5000", state2.BaselineMeanMs)
	}
	if len(state2.KeptChanges) != 1 {
		t.Errorf("kept changes = %d, want 1", len(state2.KeptChanges))
	}

	// Summary
	summary := state2.Summary()
	if !strings.Contains(summary, "3 iterations") {
		t.Errorf("summary missing iteration count: %s", summary)
	}
	if !strings.Contains(summary, "500ms total saved") {
		t.Errorf("summary missing saved time: %s", summary)
	}
}

func TestStateManager_Persistence(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	state := &State{
		SessionID:        "persist-test",
		StartedAt:        time.Now(),
		CurrentIteration: 5,
		BaselineMeanMs:   10000,
		Results: []HypothesisResult{
			{
				Iteration: 1,
				Correct:   true,
				Kept:      true,
				MeanMs:    8000,
			},
		},
	}

	if err := sm.Save(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, "bootopt-persist-test.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	// Load and verify
	loaded, err := sm.Load("persist-test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.CurrentIteration != 5 {
		t.Errorf("iteration = %d, want 5", loaded.CurrentIteration)
	}
	if len(loaded.Results) != 1 {
		t.Errorf("results = %d, want 1", len(loaded.Results))
	}
}


