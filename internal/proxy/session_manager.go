package proxy

import (
	"log/slog"
	"sync"
	"time"
)

// ManagedSession tracks an orchestrator session with reference counting.
type ManagedSession struct {
	SessionID   string
	Serial      string
	RefCount    int
	releaseTimer *time.Timer
}

// SessionManager maps device serials to orchestrator sessions with reference counting.
//
// Multiple ADB TCP connections may target the same device serial during a
// single AGP connectedCheck run. The SessionManager ensures they reuse the
// same orchestrator session. When all connections close, a grace period is
// applied before releasing the session.
type SessionManager struct {
	mu            sync.Mutex
	commandRouter *CommandRouter
	gracePeriod   time.Duration
	sessions      map[string]*ManagedSession // serial -> session
}

func NewSessionManager(commandRouter *CommandRouter, gracePeriodMs int64) *SessionManager {
	if gracePeriodMs <= 0 {
		gracePeriodMs = 30000
	}
	return &SessionManager{
		commandRouter: commandRouter,
		gracePeriod:   time.Duration(gracePeriodMs) * time.Millisecond,
		sessions:      make(map[string]*ManagedSession),
	}
}

// AcquireRef acquires a reference to the session for the given serial.
// If no session exists, calls AcquireDevice on the orchestrator.
// Returns (sessionID, serial).
func (sm *SessionManager) AcquireRef(serial string) (string, string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if existing, ok := sm.sessions[serial]; ok {
		if existing.releaseTimer != nil {
			existing.releaseTimer.Stop()
			existing.releaseTimer = nil
		}
		existing.RefCount++
		slog.Debug("reusing session", "session", existing.SessionID, "serial", serial, "refCount", existing.RefCount)
		return existing.SessionID, existing.Serial, nil
	}

	// Acquire new session from orchestrator
	resp, err := sm.commandRouter.AcquireDevice(serial, 60, 300)
	if err != nil {
		return "", "", err
	}

	managed := &ManagedSession{
		SessionID: resp.SessionId,
		Serial:    serial,
		RefCount:  1,
	}
	sm.sessions[serial] = managed
	slog.Info("acquired new session", "session", resp.SessionId, "serial", serial)
	return resp.SessionId, serial, nil
}

// AcquireRefAny acquires a reference for any available device.
func (sm *SessionManager) AcquireRefAny() (string, string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Prefer reusing an existing session
	for _, existing := range sm.sessions {
		if existing.releaseTimer != nil {
			existing.releaseTimer.Stop()
			existing.releaseTimer = nil
		}
		existing.RefCount++
		slog.Debug("reusing session (any)", "session", existing.SessionID, "serial", existing.Serial, "refCount", existing.RefCount)
		return existing.SessionID, existing.Serial, nil
	}

	resp, err := sm.commandRouter.AcquireDevice("", 60, 300)
	if err != nil {
		return "", "", err
	}

	serial := resp.Device.Serial
	managed := &ManagedSession{
		SessionID: resp.SessionId,
		Serial:    serial,
		RefCount:  1,
	}
	sm.sessions[serial] = managed
	slog.Info("acquired new session (any)", "session", resp.SessionId, "serial", serial)
	return resp.SessionId, serial, nil
}

// ReleaseRef releases a reference. When refCount reaches 0, schedules release after grace period.
func (sm *SessionManager) ReleaseRef(serial, sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	managed, ok := sm.sessions[serial]
	if !ok || managed.SessionID != sessionID {
		return
	}

	managed.RefCount--
	slog.Debug("released ref", "session", sessionID, "serial", serial, "refCount", managed.RefCount)

	if managed.RefCount <= 0 {
		managed.RefCount = 0
		managed.releaseTimer = time.AfterFunc(sm.gracePeriod, func() {
			sm.doRelease(serial, sessionID)
		})
	}
}

func (sm *SessionManager) doRelease(serial, sessionID string) {
	sm.mu.Lock()
	managed, ok := sm.sessions[serial]
	if !ok || managed.SessionID != sessionID || managed.RefCount > 0 {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, serial)
	sm.mu.Unlock()

	if _, err := sm.commandRouter.ReleaseDevice(sessionID); err != nil {
		slog.Error("failed to release session", "session", sessionID, "error", err)
	} else {
		slog.Info("released session after grace period", "session", sessionID, "serial", serial)
	}
}

// EvictSession removes a stale session (e.g. orchestrator returned NOT_FOUND).
func (sm *SessionManager) EvictSession(serial, sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	managed, ok := sm.sessions[serial]
	if !ok || managed.SessionID != sessionID {
		return
	}
	if managed.releaseTimer != nil {
		managed.releaseTimer.Stop()
	}
	delete(sm.sessions, serial)
	slog.Info("evicted stale session", "session", sessionID, "serial", serial)
}

// ReleaseAll releases all sessions immediately (for shutdown).
func (sm *SessionManager) ReleaseAll() {
	sm.mu.Lock()
	sessions := make(map[string]*ManagedSession)
	for k, v := range sm.sessions {
		sessions[k] = v
	}
	sm.sessions = make(map[string]*ManagedSession)
	sm.mu.Unlock()

	for serial, managed := range sessions {
		if managed.releaseTimer != nil {
			managed.releaseTimer.Stop()
		}
		if _, err := sm.commandRouter.ReleaseDevice(managed.SessionID); err != nil {
			slog.Error("failed to release session on shutdown", "session", managed.SessionID, "error", err)
		} else {
			slog.Info("shutdown: released session", "session", managed.SessionID, "serial", serial)
		}
	}
}
