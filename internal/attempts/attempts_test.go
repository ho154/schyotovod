package attempts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAttemptsManager(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "attempts-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "attempts.json")
	m, err := New(dbPath, time.UTC)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	client := "Test Client"

	// 1. Check initial state
	state := m.GetState(client)
	if state.Count != 0 || state.Ignored || !state.NextAttempt.IsZero() {
		t.Errorf("expected clean initial state, got %+v", state)
	}

	// 2. Record failure
	state = m.RecordAttemptWithMsg(client, false, 3, 10, "msg-123")
	if state.Count != 1 || state.Ignored || state.NextAttempt.IsZero() || state.LastMessageID != "msg-123" {
		t.Errorf("expected 1 failure and non-zero NextAttempt, got %+v", state)
	}

	// 2a. Record failure with new message ID (should reset count)
	state = m.RecordAttemptWithMsg(client, false, 3, 10, "msg-456")
	if state.Count != 1 || state.Ignored || state.NextAttempt.IsZero() || state.LastMessageID != "msg-456" {
		t.Errorf("expected count to reset to 1 due to new message ID, got %+v", state)
	}

	// 2b. Record failure with same message ID (should increment count)
	state = m.RecordAttemptWithMsg(client, false, 3, 10, "msg-456")
	if state.Count != 2 || state.Ignored || state.NextAttempt.IsZero() || state.LastMessageID != "msg-456" {
		t.Errorf("expected count to increment to 2, got %+v", state)
	}

	// 3. Reset single client attempt
	m.ResetAttempt(client)
	state = m.GetState(client)
	if state.Count != 0 || state.Ignored || !state.NextAttempt.IsZero() {
		t.Errorf("expected attempts to be reset, got %+v", state)
	}

	// 4. Record failure again and test ResetAll
	m.RecordAttempt(client, false, 3, 10)
	m.RecordAttempt("Other Client", false, 3, 10)

	m.ResetAll()

	state = m.GetState(client)
	if state.Count != 0 || state.Ignored || !state.NextAttempt.IsZero() {
		t.Errorf("expected client state to be reset by ResetAll, got %+v", state)
	}
	state2 := m.GetState("Other Client")
	if state2.Count != 0 || state2.Ignored || !state2.NextAttempt.IsZero() {
		t.Errorf("expected other client state to be reset by ResetAll, got %+v", state2)
	}
}
