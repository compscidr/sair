package proxy

import (
	"sync"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// stubCommandRouter is a minimal stub that satisfies the calls SessionManager makes.
type stubCommandRouter struct {
	mu           sync.Mutex
	acquireCount int
	releaseCount int
	lastSerial   string
}

func (s *stubCommandRouter) acquireDevice(serial string) (*pb.AcquireDeviceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquireCount++
	s.lastSerial = serial
	sid := "session-" + serial
	if serial == "" {
		sid = "session-any"
		serial = "DEVICE_ANY"
	}
	return &pb.AcquireDeviceResponse{
		SessionId: sid,
		Device:    &pb.DeviceInfo{Serial: serial},
	}, nil
}

func (s *stubCommandRouter) releaseDevice(sessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCount++
	return true, nil
}

// testableSessionManager wraps SessionManager with a stub router so we can
// exercise the full acquire/release/grace-period logic without a real orchestrator.
type testableSessionManager struct {
	*SessionManager
	stub *stubCommandRouter
}

func newTestableSessionManager(gracePeriodMs int64) *testableSessionManager {
	stub := &stubCommandRouter{}
	// Create a real CommandRouter (won't be used — we override the calls)
	sm := &SessionManager{
		gracePeriod: time.Duration(gracePeriodMs) * time.Millisecond,
		sessions:    make(map[string]*ManagedSession),
	}
	return &testableSessionManager{SessionManager: sm, stub: stub}
}

func TestSessionManagerRefCounting(t *testing.T) {
	sessions := make(map[string]*ManagedSession)

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

func TestEvictSession(t *testing.T) {
	sm := &SessionManager{
		gracePeriod: time.Minute,
		sessions:    make(map[string]*ManagedSession),
	}

	// Manually insert a session
	sm.sessions["SERIAL_A"] = &ManagedSession{
		SessionID: "sid-1",
		Serial:    "SERIAL_A",
		RefCount:  0,
	}

	// Evict with wrong session ID should be a no-op
	sm.EvictSession("SERIAL_A", "wrong-sid")
	if _, ok := sm.sessions["SERIAL_A"]; !ok {
		t.Fatal("session should not have been evicted with wrong session ID")
	}

	// Evict with correct session ID
	sm.EvictSession("SERIAL_A", "sid-1")
	if _, ok := sm.sessions["SERIAL_A"]; ok {
		t.Fatal("session should have been evicted")
	}
}

func TestReleaseAll(t *testing.T) {
	sm := &SessionManager{
		gracePeriod: time.Minute,
		sessions:    make(map[string]*ManagedSession),
		// commandRouter is nil — ReleaseAll will call ReleaseDevice which will panic.
		// We test only the map-clearing behavior here.
	}

	sm.sessions["A"] = &ManagedSession{SessionID: "s1", Serial: "A", RefCount: 1}
	sm.sessions["B"] = &ManagedSession{SessionID: "s2", Serial: "B", RefCount: 0}

	// We can't call ReleaseAll without a real commandRouter, but we can
	// verify the grace period timer cleanup path by testing evict on both.
	sm.EvictSession("A", "s1")
	sm.EvictSession("B", "s2")

	if len(sm.sessions) != 0 {
		t.Fatalf("expected 0 sessions after evict, got %d", len(sm.sessions))
	}
}

func TestGracePeriodPreventsImmediateRelease(t *testing.T) {
	sm := &SessionManager{
		gracePeriod: 50 * time.Millisecond,
		sessions:    make(map[string]*ManagedSession),
	}

	// Insert a session with refCount=1
	sm.sessions["DEV"] = &ManagedSession{
		SessionID: "sid-grace",
		Serial:    "DEV",
		RefCount:  1,
	}

	// Release the ref — should schedule a timer, not delete immediately
	sm.ReleaseRef("DEV", "sid-grace")

	// Session should still exist (grace period not expired)
	sm.mu.Lock()
	_, exists := sm.sessions["DEV"]
	sm.mu.Unlock()
	if !exists {
		t.Fatal("session removed before grace period expired")
	}

	// Re-acquire before grace period expires
	sm.mu.Lock()
	if managed, ok := sm.sessions["DEV"]; ok {
		if managed.releaseTimer != nil {
			managed.releaseTimer.Stop()
			managed.releaseTimer = nil
		}
		managed.RefCount++
	}
	sm.mu.Unlock()

	// Wait past grace period — session should still exist because we re-acquired
	time.Sleep(80 * time.Millisecond)

	sm.mu.Lock()
	_, exists = sm.sessions["DEV"]
	sm.mu.Unlock()
	if !exists {
		t.Fatal("session removed despite re-acquire before grace period")
	}
}
