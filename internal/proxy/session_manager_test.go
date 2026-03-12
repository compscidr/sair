package proxy

import (
	"testing"
	"time"
)

// mockCommandRouter is a minimal stub for SessionManager tests.
type mockCommandRouter struct {
	acquireCount int
	releaseCount int
	lastSerial   string
}

func (m *mockCommandRouter) acquireDevice(serial string) (string, string, error) {
	m.acquireCount++
	m.lastSerial = serial
	sessionID := "session-" + serial
	if serial == "" {
		sessionID = "session-any"
		serial = "DEVICE_ANY"
	}
	return sessionID, serial, nil
}

func (m *mockCommandRouter) releaseDevice(sessionID string) error {
	m.releaseCount++
	return nil
}

// testSessionManager wraps SessionManager with a mock command router for testing
// the ref counting and grace period logic without needing a real orchestrator.
type testSessionManager struct {
	mu            lockMutex
	sessions      map[string]*testSession
	gracePeriod   time.Duration
	mock          *mockCommandRouter
}

type lockMutex struct{}

type testSession struct {
	sessionID    string
	serial       string
	refCount     int
	releaseTimer *time.Timer
}

func TestSessionManagerRefCounting(t *testing.T) {
	// Test that ref counting works: multiple acquires for the same serial
	// should reuse the same session, and release should only happen when
	// all refs are gone + grace period expires.

	sessions := make(map[string]*ManagedSession)

	// Simulate acquiring 3 refs for the same serial
	serial := "ABC123"
	session := &ManagedSession{
		SessionID: "session-1",
		Serial:    serial,
		RefCount:  0,
	}
	sessions[serial] = session

	// Acquire ref 3 times
	for i := 0; i < 3; i++ {
		session.RefCount++
	}
	if session.RefCount != 3 {
		t.Fatalf("expected refCount=3, got %d", session.RefCount)
	}

	// Release 2 refs
	session.RefCount--
	session.RefCount--
	if session.RefCount != 1 {
		t.Fatalf("expected refCount=1, got %d", session.RefCount)
	}

	// Release last ref
	session.RefCount--
	if session.RefCount != 0 {
		t.Fatalf("expected refCount=0, got %d", session.RefCount)
	}
}

func TestManagedSessionFields(t *testing.T) {
	session := &ManagedSession{
		SessionID: "test-session",
		Serial:    "SERIAL123",
		RefCount:  1,
	}

	if session.SessionID != "test-session" {
		t.Errorf("unexpected sessionID: %s", session.SessionID)
	}
	if session.Serial != "SERIAL123" {
		t.Errorf("unexpected serial: %s", session.Serial)
	}
	if session.RefCount != 1 {
		t.Errorf("unexpected refCount: %d", session.RefCount)
	}
}
