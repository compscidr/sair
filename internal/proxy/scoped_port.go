package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// ScopedPort represents a scoped ADB listener port for a specific lock.
type ScopedPort struct {
	LockID     string
	Serials    map[string]struct{}
	Port       int
	listener   net.Listener
	CreatedAt  time.Time
	stopCh     chan struct{}
	heartbeatStop chan struct{}
}

// ScopedPortManager manages scoped ADB listener ports for per-runner device isolation.
//
// Each lock gets a dedicated TCP port that only exposes the locked devices.
// The bare port (5037) shows no devices; runners must acquire a scoped port
// via the proxy HTTP API to access any device.
type ScopedPortManager struct {
	mu                     sync.Mutex
	commandRouter          *CommandRouter
	sessionManager         *SessionManager
	deviceListTracker      *DeviceListTracker
	heartbeatIntervalSecs  int64
	scopedPorts            map[string]*ScopedPort
}

func NewScopedPortManager(
	commandRouter *CommandRouter,
	sessionManager *SessionManager,
	deviceListTracker *DeviceListTracker,
	heartbeatIntervalSecs int64,
) *ScopedPortManager {
	if heartbeatIntervalSecs <= 0 {
		heartbeatIntervalSecs = 60
	}
	return &ScopedPortManager{
		commandRouter:         commandRouter,
		sessionManager:        sessionManager,
		deviceListTracker:     deviceListTracker,
		heartbeatIntervalSecs: heartbeatIntervalSecs,
		scopedPorts:           make(map[string]*ScopedPort),
	}
}

// Acquire acquires a lock from the orchestrator (via gRPC) and opens a scoped ADB port.
// Blocks until the orchestrator grants the lock.
func (m *ScopedPortManager) Acquire(requestedSerials map[string]struct{}) (*ScopedPort, error) {
	result, err := m.commandRouter.AcquireLock(requestedSerials, 30)
	if err != nil {
		return nil, err
	}
	sp, err := m.CreateScopedPort(result.LockID, result.Serials)
	if err != nil {
		// Clean up: release the lock if we can't create the port
		if _, releaseErr := m.commandRouter.ReleaseLock(result.LockID); releaseErr != nil {
			slog.Warn("failed to release lock after scoped port creation failure",
				"lockId", result.LockID, "error", releaseErr)
		}
		return nil, err
	}
	return sp, nil
}

// CreateScopedPort creates a scoped port for an already-acquired lock.
func (m *ScopedPortManager) CreateScopedPort(lockID string, serials map[string]struct{}) (*ScopedPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.scopedPorts[lockID]; exists {
		return nil, fmt.Errorf("scoped port already exists for lock %s", lockID)
	}

	// Open ephemeral port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port

	sp := &ScopedPort{
		LockID:        lockID,
		Serials:       serials,
		Port:          port,
		listener:      listener,
		CreatedAt:     time.Now(),
		stopCh:        make(chan struct{}),
		heartbeatStop: make(chan struct{}),
	}

	// Start accept loop
	go m.runAcceptLoop(sp)

	// Start heartbeat
	go m.runHeartbeat(sp)

	m.scopedPorts[lockID] = sp
	slog.Info("opened scoped port", "port", port, "lockId", lockID, "serials", serials)
	return sp, nil
}

// Release closes a scoped port and releases the lock on the orchestrator.
func (m *ScopedPortManager) Release(lockID string) bool {
	closed := m.CloseScopedPort(lockID)
	released, err := m.commandRouter.ReleaseLock(lockID)
	if err != nil {
		slog.Warn("failed to release lock on orchestrator", "lockId", lockID, "error", err)
		return closed
	}
	return closed || released
}

// CloseScopedPort closes a scoped port without releasing the orchestrator lock.
func (m *ScopedPortManager) CloseScopedPort(lockID string) bool {
	m.mu.Lock()
	sp, exists := m.scopedPorts[lockID]
	if !exists {
		m.mu.Unlock()
		return false
	}
	delete(m.scopedPorts, lockID)
	m.mu.Unlock()

	close(sp.heartbeatStop)
	close(sp.stopCh)
	sp.listener.Close()

	slog.Info("closed scoped port", "port", sp.Port, "lockId", lockID)
	return true
}

// GetAllScopedPorts returns a snapshot of all active scoped ports.
func (m *ScopedPortManager) GetAllScopedPorts() []*ScopedPort {
	m.mu.Lock()
	defer m.mu.Unlock()

	ports := make([]*ScopedPort, 0, len(m.scopedPorts))
	for _, sp := range m.scopedPorts {
		ports = append(ports, sp)
	}
	return ports
}

// ShutdownAll closes all scoped ports and releases all locks.
func (m *ScopedPortManager) ShutdownAll() {
	m.mu.Lock()
	lockIDs := make([]string, 0, len(m.scopedPorts))
	for id := range m.scopedPorts {
		lockIDs = append(lockIDs, id)
	}
	m.mu.Unlock()

	for _, lockID := range lockIDs {
		m.CloseScopedPort(lockID)
		if _, err := m.commandRouter.ReleaseLock(lockID); err != nil {
			slog.Warn("failed to release lock during shutdown", "lockId", lockID, "error", err)
		}
	}
}

func (m *ScopedPortManager) runAcceptLoop(sp *ScopedPort) {
	slog.Debug("accept loop started", "lockId", sp.LockID, "port", sp.Port)
	for {
		conn, err := sp.listener.Accept()
		if err != nil {
			select {
			case <-sp.stopCh:
				return
			default:
				slog.Debug("accept error on scoped port", "lockId", sp.LockID, "error", err)
			}
			return
		}
		// Disable Nagle's algorithm so ADB protocol messages are sent immediately
		if tc, ok := conn.(*net.TCPConn); ok {
			if err := tc.SetNoDelay(true); err != nil {
				slog.Warn("failed to set TCP_NODELAY", "remote", conn.RemoteAddr(), "error", err)
			}
		}
		adbConn := NewAdbConnection(conn, m.commandRouter, m.sessionManager, m.deviceListTracker, sp.Serials)
		go adbConn.Handle()
	}
}

func (m *ScopedPortManager) runHeartbeat(sp *ScopedPort) {
	ticker := time.NewTicker(time.Duration(m.heartbeatIntervalSecs) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			alive, err := m.commandRouter.LockHeartbeat(sp.LockID)
			if err != nil {
				slog.Warn("heartbeat failed", "lockId", sp.LockID, "error", err)
				continue
			}
			if !alive {
				slog.Warn("lock expired on orchestrator — closing scoped port",
					"lockId", sp.LockID, "port", sp.Port)
				m.CloseScopedPort(sp.LockID)
				return
			}
		case <-sp.heartbeatStop:
			return
		}
	}
}
