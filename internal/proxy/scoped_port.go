package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// ScopedPort represents a scoped ADB listener port for a specific lock.
type ScopedPort struct {
	LockID        string
	Serials       map[string]struct{}
	Port          int
	listener      net.Listener
	CreatedAt     time.Time
	stopCh        chan struct{}
	heartbeatStop chan struct{}
	// keepaliveDone is closed when runKeepalive returns, so CloseScopedPort can
	// wait for it before a caller drains the log (nothing left flushing/requeuing).
	keepaliveDone chan struct{}
	// Log of ADB requests made on this port, shipped to the orchestrator on
	// each heartbeat and on release.
	Log *LockLog
	// RemoteDevices are granted devices reached through the relay, keyed by
	// serial. nil for none.
	RemoteDevices map[string]*pb.DeviceInfo
	// Stable transport ids for RemoteDevices, disjoint from the tracker's ids
	// (which start at 1). One mutex guards both directions so they can never
	// disagree: an id is only ever visible together with its reverse entry.
	remoteMu       sync.Mutex
	remoteBySerial map[string]int
	remoteByID     map[int]string
}

// remoteTransportID returns a transport id for a remote serial, stable for the
// port's lifetime. 0 for a serial that is not one of this port's remote devices.
func (sp *ScopedPort) remoteTransportID(serial string) int {
	if _, ok := sp.RemoteDevices[serial]; !ok {
		return 0
	}
	sp.remoteMu.Lock()
	defer sp.remoteMu.Unlock()
	if id, ok := sp.remoteBySerial[serial]; ok {
		return id
	}
	if sp.remoteBySerial == nil {
		sp.remoteBySerial = map[string]int{}
		sp.remoteByID = map[int]string{}
	}
	id := 1_000_000 + len(sp.remoteBySerial) + 1
	sp.remoteBySerial[serial] = id
	sp.remoteByID[id] = serial
	return id
}

// remoteSerialByTransportID is the reverse of remoteTransportID, for
// host:transport-id: on a remote device's id. Returns "" if unknown.
func (sp *ScopedPort) remoteSerialByTransportID(id int) string {
	sp.remoteMu.Lock()
	defer sp.remoteMu.Unlock()
	return sp.remoteByID[id]
}

// ScopedPortManager manages scoped ADB listener ports for per-runner device isolation.
//
// Each lock gets a dedicated TCP port that only exposes the locked devices.
// The bare port (5037) shows no devices; runners must acquire a scoped port
// via the proxy HTTP API to access any device.
type ScopedPortManager struct {
	mu                    sync.Mutex
	commandRouter         *CommandRouter
	deviceListTracker     *DeviceListTracker
	heartbeatIntervalSecs int64
	// How often buffered log entries are shipped on the session. Tests shorten it.
	flushInterval time.Duration
	scopedPorts   map[string]*ScopedPort
}

func NewScopedPortManager(
	commandRouter *CommandRouter,
	deviceListTracker *DeviceListTracker,
	heartbeatIntervalSecs int64,
) *ScopedPortManager {
	if heartbeatIntervalSecs <= 0 {
		heartbeatIntervalSecs = 60
	}
	return &ScopedPortManager{
		commandRouter:         commandRouter,
		deviceListTracker:     deviceListTracker,
		heartbeatIntervalSecs: heartbeatIntervalSecs,
		flushInterval:         time.Second,
		scopedPorts:           make(map[string]*ScopedPort),
	}
}

// Acquire acquires a lock from the orchestrator (via gRPC) and opens a scoped ADB port.
// Blocks until the orchestrator grants the lock.
//
// Pass requestedSerials for specific devices, or count for that many arbitrary
// free devices. Passing neither locks every device in the tenant's pool.
func (m *ScopedPortManager) Acquire(requestedSerials map[string]struct{}, count int32, repo, runURL string) (*ScopedPort, error) {
	result, err := m.commandRouter.AcquireLock(requestedSerials, count, 30, repo, runURL)
	if err != nil {
		return nil, err
	}
	sp, err := m.CreateScopedPort(result.LockID, result.Serials, result.RemoteDevices)
	if err != nil {
		// Clean up: release the lock if we can't create the port
		if _, releaseErr := m.commandRouter.ReleaseLock(result.LockID, "error", nil); releaseErr != nil {
			slog.Warn("failed to release lock after scoped port creation failure",
				"lockId", result.LockID, "error", releaseErr)
		}
		return nil, err
	}
	return sp, nil
}

// CreateScopedPort creates a scoped port for an already-acquired lock.
func (m *ScopedPortManager) CreateScopedPort(lockID string, serials map[string]struct{}, remoteDevices map[string]*pb.DeviceInfo) (*ScopedPort, error) {
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
		keepaliveDone: make(chan struct{}),
		Log:           &LockLog{},
		RemoteDevices: remoteDevices,
	}

	// Start accept loop
	go m.runAcceptLoop(sp)

	// Ship logs promptly and keep the lock alive when they are quiet.
	go m.runKeepalive(sp)

	m.scopedPorts[lockID] = sp
	slog.Info("opened scoped port", "port", port, "lockId", lockID, "serials", serials)
	return sp, nil
}

// Release closes a scoped port and releases the lock on the orchestrator.
func (m *ScopedPortManager) Release(lockID, status string) bool {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	// Close first: it stops the heartbeat (no concurrent drain) and the
	// listener (no new tunnels), so the drain below sees everything recorded.
	closed := m.CloseScopedPort(lockID)
	var entries []*pb.LockLogEntry
	if sp != nil {
		entries = sp.Log.DrainAll()
	}
	released, err := m.commandRouter.ReleaseLock(lockID, status, entries)
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
	<-sp.keepaliveDone // wait for runKeepalive to stop touching sp.Log before a caller drains it

	slog.Info("closed scoped port", "port", sp.Port, "lockId", lockID)
	return true
}

// OnLockExpired is the session's callback: the orchestrator released the lock,
// so the scoped port is dead. Idempotent.
func (m *ScopedPortManager) OnLockExpired(lockID string) {
	if m.CloseScopedPort(lockID) {
		slog.Warn("lock expired on orchestrator; closed scoped port", "lockId", lockID)
	}
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
	ports := make([]*ScopedPort, 0, len(m.scopedPorts))
	for _, sp := range m.scopedPorts {
		ports = append(ports, sp)
	}
	m.mu.Unlock()

	for _, sp := range ports {
		m.CloseScopedPort(sp.LockID) // stops the heartbeat before we drain
		entries := sp.Log.DrainAll()
		if _, err := m.commandRouter.ReleaseLock(sp.LockID, "cancelled", entries); err != nil {
			slog.Warn("failed to release lock during shutdown", "lockId", sp.LockID, "error", err)
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
		adbConn := NewAdbConnection(conn, m.commandRouter, m.deviceListTracker, sp.Serials, sp.Log, sp.RemoteDevices)
		adbConn.remoteTransportID = sp.remoteTransportID
		adbConn.remoteSerialByTransportID = sp.remoteSerialByTransportID
		go adbConn.Handle()
	}
}

// runKeepalive flushes the ADB log every flushInterval and heartbeats every
// heartbeatIntervalSecs, but only when no log was flushed in that interval:
// on the orchestrator, log traffic counts as activity.
func (m *ScopedPortManager) runKeepalive(sp *ScopedPort) {
	defer close(sp.keepaliveDone)
	flush := time.NewTicker(m.flushInterval)
	defer flush.Stop()
	beat := time.NewTicker(time.Duration(m.heartbeatIntervalSecs) * time.Second)
	defer beat.Stop()
	flushedSinceBeat := false

	for {
		select {
		case <-flush.C:
			if !m.commandRouter.sessionUp() {
				// No stream to send on (reconnecting, or unary fallback):
				// draining here would only clear the truncation flag and
				// requeue, so a job that ever hit the cap grows a fresh
				// "truncated" marker on every tick until the stream is back.
				continue
			}
			entries := sp.Log.Drain()
			if len(entries) == 0 {
				continue
			}
			if err := m.commandRouter.SendLockLog(sp.LockID, entries); err != nil {
				// Keep the entries; the next flush or the unary heartbeat ships them.
				// No stream (reconnecting, or unary fallback) is expected and quiet;
				// anything else is a send that failed on a live stream, worth seeing.
				sp.Log.Requeue(entries)
				if !errors.Is(err, errSessionDown) {
					slog.Warn("lock log flush failed; entries requeued", "lockId", sp.LockID, "entries", len(entries), "error", err)
				}
				continue
			}
			flushedSinceBeat = true
		case <-beat.C:
			if flushedSinceBeat {
				flushedSinceBeat = false
				continue
			}
			// Only drain when about to go unary: that RPC carries the log
			// atomically in the same request as the heartbeat. On the stream,
			// LockHeartbeat makes exactly one send (the heartbeat) and always
			// hands any log argument straight back as unsent, so passing nil
			// here costs nothing — and if the session flips from down to up
			// between this check and the call, LockHeartbeat's own branch still
			// only ever does one send, so there is no way to ship a log and then
			// separately fail to report it (or vice versa).
			var entries []*pb.LockLogEntry
			if !m.commandRouter.sessionUp() {
				entries = sp.Log.Drain()
			}
			alive, unsent, err := m.commandRouter.LockHeartbeat(sp.LockID, entries)
			if len(unsent) > 0 {
				sp.Log.Requeue(unsent)
			}
			if err != nil {
				if err != errSessionDown {
					slog.Warn("heartbeat failed", "lockId", sp.LockID, "error", err)
				}
				continue
			}
			if !alive {
				slog.Warn("lock expired on orchestrator; closing scoped port", "lockId", sp.LockID, "port", sp.Port)
				// Close asynchronously: CloseScopedPort waits on keepaliveDone,
				// which this goroutine only closes on return, right below.
				go m.CloseScopedPort(sp.LockID)
				return
			}
		case <-sp.heartbeatStop:
			return
		}
	}
}
