package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compscidr/sair/internal/logging"
	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// DeviceToken lets a locked phone toggle its own Wi-Fi through POST /wifi
	// (sair#83). Random per lock, dies with it, good for nothing else.
	DeviceToken string
	// DeviceAPIPort is the port on each local phone that reaches the proxy's
	// HTTP API (an adb reverse set up on acquire); 0 when none was set up.
	DeviceAPIPort int
	// openConns and lastClient (unix nanos of the last client connect or
	// disconnect) tell whether anyone is still using the lock (sair#106).
	openConns  atomic.Int32
	lastClient atomic.Int64
	// Stable transport ids for RemoteDevices, disjoint from the tracker's ids
	// (which start at 1). One mutex guards both directions so they can never
	// disagree: an id is only ever visible together with its reverse entry.
	remoteMu       sync.Mutex
	remoteBySerial map[string]int
	remoteByID     map[int]string
	// relayed marks the stand-in for a lock this proxy only relayed phones for.
	relayed bool
	// conns are the accepted connections still open, closed along with the
	// port. Once connsClosed is set, a late accept is closed on arrival.
	connMu      sync.Mutex
	conns       map[net.Conn]struct{}
	connsClosed bool
}

// trackConn records an accepted connection, or closes it and returns false
// when the port has closed since it was accepted.
func (sp *ScopedPort) trackConn(conn net.Conn) bool {
	sp.connMu.Lock()
	defer sp.connMu.Unlock()
	if sp.connsClosed {
		conn.Close()
		return false
	}
	sp.conns[conn] = struct{}{}
	return true
}

func (sp *ScopedPort) untrackConn(conn net.Conn) {
	sp.connMu.Lock()
	defer sp.connMu.Unlock()
	delete(sp.conns, conn)
}

// closeConns ends every connection on the port, and any accepted from now on.
func (sp *ScopedPort) closeConns() {
	sp.connMu.Lock()
	defer sp.connMu.Unlock()
	sp.connsClosed = true
	for conn := range sp.conns {
		conn.Close()
	}
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
	// setWifi is commandRouter.SetWifi; tests swap it.
	setWifi func(serial string, state dspb.WifiRequest_State, band string) error
	// removeReverse is commandRouter.RemoveReverseForwards; tests swap it.
	removeReverse func(serial string) error
	// addReverse is commandRouter.AddReverseForward; tests swap it.
	addReverse func(serial string, port int) error
	// resetPackages is commandRouter.ResetPackages; tests swap it.
	resetPackages func(serial string, since *dspb.PackageSnapshot) (removed, suggestions []string, err error)
	// clearUiAutomation is commandRouter.ClearUiAutomation; tests swap it.
	clearUiAutomation func(serial string) (string, error)
	// wake is commandRouter.Wake; tests swap it.
	wake func(serial string) error
	// resetBluetooth is commandRouter.ResetBluetooth; tests swap it.
	resetBluetooth func(serial string) error
	// holdUpdates is commandRouter.HoldUpdates; tests swap it.
	holdUpdates func(serial string, hold bool) error
	// btFresh holds the serials whose release reset left Bluetooth freshly
	// cycled (or rebooted), so the next acquire need not cycle it again.
	btFresh sync.Map
	// rebootIfStale is commandRouter.RebootIfStale; tests swap it.
	rebootIfStale func(serial string) (rebooted bool, uptime time.Duration, err error)
	// rebootIfWedged is commandRouter.RebootIfWedged; tests swap it.
	rebootIfWedged func(serial, radio string) (rebooted bool, err error)
	// event is commandRouter.Event; tests swap it.
	event func(e *pb.BenchEvent)
	// recentSourceLog is commandRouter.RecentDeviceSourceLog; tests swap it.
	recentSourceLog func(serial string) ([]string, error)
	// DeviceAPIPort is the proxy's HTTP API port, reverse-forwarded onto each
	// local phone on acquire so a test can call POST /wifi with the lock's
	// device token. 0 disables it. The forward lands on the device source's
	// host, so the proxy must serve its HTTP API there (they share a bench).
	DeviceAPIPort int
	// IdleTimeout releases a lock whose scoped port has had no client
	// connected for this long: its job is gone (runner lost, container
	// killed) and would otherwise be kept alive by the heartbeat forever
	// (sair#106). 0 disables it.
	IdleTimeout time.Duration
	// wifiMu holds one *sync.Mutex per serial. It orders Wi-Fi changes on a
	// device so a released lock's reset can never land after the next lock's
	// connect, while one phone's ON wait never blocks another phone's OFF.
	// ponytail: entries are never dropped; one per serial ever seen is fine.
	wifiMu sync.Map
}

func (m *ScopedPortManager) wifiLock(serial string) *sync.Mutex {
	mu, _ := m.wifiMu.LoadOrStore(serial, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func NewScopedPortManager(
	commandRouter *CommandRouter,
	deviceListTracker *DeviceListTracker,
	heartbeatIntervalSecs int64,
) *ScopedPortManager {
	if heartbeatIntervalSecs <= 0 {
		heartbeatIntervalSecs = 60
	}
	m := &ScopedPortManager{
		commandRouter:         commandRouter,
		deviceListTracker:     deviceListTracker,
		heartbeatIntervalSecs: heartbeatIntervalSecs,
		IdleTimeout:           20 * time.Minute,
		flushInterval:         time.Second,
		scopedPorts:           make(map[string]*ScopedPort),
		setWifi:               commandRouter.SetWifi,
		removeReverse:         commandRouter.RemoveReverseForwards,
		addReverse:            commandRouter.AddReverseForward,
		resetPackages:         commandRouter.ResetPackages,
		clearUiAutomation:     commandRouter.ClearUiAutomation,
		wake:                  commandRouter.Wake,
		resetBluetooth:        commandRouter.ResetBluetooth,
		holdUpdates:           commandRouter.HoldUpdates,
		rebootIfStale:         commandRouter.RebootIfStale,
		rebootIfWedged:        commandRouter.RebootIfWedged,
		event:                 commandRouter.Event,
		recentSourceLog:       commandRouter.RecentDeviceSourceLog,
	}
	commandRouter.waitReset = func(serial string) {
		mu := m.wifiLock(serial)
		mu.Lock()
		mu.Unlock()
	}
	return m
}

// Acquire acquires a lock from the orchestrator (via gRPC) and opens a scoped ADB port.
// Blocks until the orchestrator grants the lock.
//
// Pass requestedSerials for specific devices, or count for that many arbitrary
// free devices. Passing neither locks every device in the tenant's pool.
//
// With wifi, every local device is joined to the bench Wi-Fi and online on it
// before Acquire returns; without it, every local device is verified offline
// first rather than trusting the previous release's reset (sair#86). Either
// way a device that cannot be put in that state releases the lock and
// returns the error. A bench without managed Wi-Fi skips this. wifiBand
// (with wifi) is SetWifi's band.
//
// Every local device is also awake with its keyguard dismissed, verified
// (sair#92); one that stays locked costs the lock the same way.
//
// With sameProxy, the orchestrator grants every device from a single proxy.
// clientIP is the runner's address as this proxy sees it; ctx ends the wait
// when the runner gives up; see AcquireLock.
func (m *ScopedPortManager) Acquire(ctx context.Context, requestedSerials map[string]struct{}, count int32, job *pb.AcquireLockRequest, priority int32, wifi bool, wifiBand string, sameProxy bool, tenantKey, clientIP string) (*ScopedPort, error) {
	asked := time.Now()
	result, err := m.commandRouter.AcquireLock(ctx, requestedSerials, count, 30, job, priority, sameProxy, tenantKey, clientIP)
	if err != nil {
		return nil, err
	}
	granted := time.Now()
	sp, err := m.CreateScopedPort(result.LockID, result.Serials, result.RemoteDevices)
	if err != nil {
		// Clean up: release the lock if we can't create the port
		if _, releaseErr := m.commandRouter.ReleaseLock(result.LockID, "error", nil); releaseErr != nil {
			slog.Warn("failed to release lock after scoped port creation failure",
				"lockId", result.LockID, "error", releaseErr)
		}
		return nil, err
	}
	// A job that never released (no if: always() step, a dead runner, a
	// proxy that was down when the lock expired) left its reverse forwards
	// and packages behind; the next job cleans up before it starts, and the
	// line in its lock log says what it found.
	for serial := range sp.Serials {
		if _, remote := sp.RemoteDevices[serial]; remote {
			continue
		}
		mu := m.wifiLock(serial)
		mu.Lock()
		m.cleanDevice(sp, serial, " (left by an earlier job)", nil)
		if _, fresh := m.btFresh.LoadAndDelete(serial); !fresh {
			m.cycleBluetooth(sp, serial, " (no release reset since the last job)", true)
		}
		m.setUpdates(sp, serial, true)
		err := m.wakeDevice(sp.LockID, serial)
		mu.Unlock()
		if wakeFailed(err) {
			m.acquireFailed(sp, "wake_failed", err, asked, granted)
			return nil, err
		}
	}
	m.setupDeviceAPI(sp)
	if err := m.SetWifi(sp.LockID, nil, wifi, wifiBand); err != nil && !wifiUnmanaged(err) {
		m.acquireFailed(sp, "wifi_failed", err, asked, granted)
		return nil, err
	}
	m.acquireEvents(sp, "granted", nil, asked, granted)
	return sp, nil
}

// acquireFailed records why a granted lock could not be handed over, with
// the logs that led up to it, and releases it.
func (m *ScopedPortManager) acquireFailed(sp *ScopedPort, outcome string, err error, asked, granted time.Time) {
	m.acquireEvents(sp, outcome, err, asked, granted)
	m.logExcerpt(sp, sp.localSerials())
	m.Release(sp.LockID, "error")
}

// acquireEvents sends one ACQUIRE event per local device of sp.
func (m *ScopedPortManager) acquireEvents(sp *ScopedPort, outcome string, err error, asked, granted time.Time) {
	for _, serial := range sp.localSerials() {
		m.event(&pb.BenchEvent{
			Kind: pb.BenchEventKind_BENCH_EVENT_KIND_ACQUIRE, Serial: serial, LockId: sp.LockID, Outcome: outcome, Detail: errText(err),
			WaitMs: granted.Sub(asked).Milliseconds(), DurationMs: time.Since(granted).Milliseconds(),
		})
	}
}

// localSerials is sp's devices attached to this proxy, sorted.
func (sp *ScopedPort) localSerials() []string {
	var out []string
	for serial := range sp.Serials {
		if _, remote := sp.RemoteDevices[serial]; !remote {
			out = append(out, serial)
		}
	}
	slices.Sort(out)
	return out
}

// maxExcerptLines caps each log excerpt a failure adds to a lock log.
const maxExcerptLines = 100

// logExcerpt records in sp's lock log the proxy's and the device source's
// recent log lines about serials, so a failure's context sits next to the
// red job in the portal (sair#97). Lines are filtered to the lock and its
// phones: a guest's lock log must not carry the owner's other locks.
func (m *ScopedPortManager) logExcerpt(sp *ScopedPort, serials []string) {
	now := time.Now().UnixMilli()
	add := func(who string, lines []string) {
		if len(lines) == 0 {
			return
		}
		lines = lines[max(0, len(lines)-maxExcerptLines):]
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: now, Service: "sair: " + who + " log before the failure"})
		sp.Log.Record(&pb.LockLogEntry{Output: strings.Join(lines, "\n") + "\n"})
	}
	add("proxy", logging.Recent.Lines(append([]string{sp.LockID}, serials...)...))
	for _, serial := range serials {
		lines, err := m.recentSourceLog(serial)
		if err != nil {
			slog.Debug("no device source log for the excerpt", "serial", serial, "error", err)
			continue
		}
		add("device source ("+serial+")", lines)
	}
}

// errText is err's message, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

// wakeFailed reports whether a Wake error is real, not a relayed device or
// a device source that predates Wake.
func wakeFailed(err error) bool {
	return err != nil && status.Code(err) != codes.Unimplemented && status.Code(err) != codes.NotFound
}

// wakeDevice is wake plus a WAKE event when it fails.
func (m *ScopedPortManager) wakeDevice(lockID, serial string) error {
	err := m.wake(serial)
	if wakeFailed(err) {
		m.event(&pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_WAKE, Serial: serial, LockId: lockID, Outcome: "failed", Detail: errText(err)})
	}
	return err
}

var wifiEventKinds = map[dspb.WifiRequest_State]pb.BenchEventKind{
	dspb.WifiRequest_ON:    pb.BenchEventKind_BENCH_EVENT_KIND_WIFI_ON,
	dspb.WifiRequest_OFF:   pb.BenchEventKind_BENCH_EVENT_KIND_WIFI_OFF,
	dspb.WifiRequest_RESET: pb.BenchEventKind_BENCH_EVENT_KIND_WIFI_RESET,
}

// wifiDevice is setWifi plus its WIFI_* event, on a bench that manages Wi-Fi.
func (m *ScopedPortManager) wifiDevice(lockID, serial string, state dspb.WifiRequest_State, band string) error {
	start := time.Now()
	err := m.setWifi(serial, state, band)
	if !wifiUnmanaged(err) {
		m.event(&pb.BenchEvent{
			Kind: wifiEventKinds[state], Serial: serial, LockId: lockID, Outcome: wifiOutcome(err), Detail: errText(err),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}
	return err
}

// wifiFailures maps the device source's Wi-Fi check messages (wifi.go) to
// event outcomes, first match wins, so the portal can count failures by
// cause: a phone that never associates wants moving, one that associates
// but never validates wants a longer timeout or a better uplink (sair#96).
// ponytail: matches the device source's text; a typed reason in the status
// details if these strings start drifting.
var wifiFailures = []struct{ text, outcome string }{
	{"not associated to any network", "not_associated"},
	{"still associated to", "still_associated"},
	{"associated to", "other_ssid"},
	{"not VALIDATED", "not_validated"},
	{"no Wi-Fi network in dumpsys", "not_validated"},
	{"default network is cellular", "cellular_default"},
	{"cannot reach", "no_route"},
	{"still reaches", "still_online"},
	{"radio is", "radio_off"},
}

func wifiOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	msg := errText(err)
	for _, f := range wifiFailures {
		if strings.Contains(msg, f.text) {
			return f.outcome
		}
	}
	if status.Code(err) == codes.DeadlineExceeded {
		return "timeout"
	}
	return "error"
}

// setupDeviceAPI reverse-forwards the proxy's HTTP API onto every local phone
// of the lock. Best effort: a phone that refuses the forward only costs the
// job the device-side control, not the lock. Nothing to do for a relayed
// device: its adb reverse would end on the other proxy's host.
func (m *ScopedPortManager) setupDeviceAPI(sp *ScopedPort) {
	if m.DeviceAPIPort <= 0 {
		return
	}
	for serial := range sp.Serials {
		if _, remote := sp.RemoteDevices[serial]; remote {
			continue
		}
		if err := m.addReverse(serial, m.DeviceAPIPort); err != nil {
			slog.Warn("device API reverse forward failed; device-side Wi-Fi control unavailable", "serial", serial, "lockId", sp.LockID, "error", err)
			return
		}
	}
	sp.DeviceAPIPort = m.DeviceAPIPort
}

// DeviceTokenValid reports whether token is the live device token of lockID.
func (m *ScopedPortManager) DeviceTokenValid(lockID, token string) bool {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	return sp != nil && token != "" && subtle.ConstantTimeCompare([]byte(sp.DeviceToken), []byte(token)) == 1
}

var (
	errUnknownLock = errors.New("unknown lock_id")
	errNotInLock   = errors.New("serial is not part of this lock")
)

// AddLabels forwards labels for a lock this proxy holds; a per-serial label
// must name one of the lock's devices.
func (m *ScopedPortManager) AddLabels(lockID string, labels []*pb.Label) error {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	if sp == nil {
		return errUnknownLock
	}
	for _, l := range labels {
		if _, ok := sp.Serials[l.Serial]; l.Serial != "" && !ok {
			return fmt.Errorf("%w: %s", errNotInLock, l.Serial)
		}
	}
	return m.commandRouter.AddLockLabels(lockID, labels)
}

// SetWifi turns Wi-Fi on or off for serials of a lock, or for all of its
// local devices when serials is empty. Devices relayed from another proxy are
// not supported: naming one is an error, and "all" skips them. band ("2.4",
// "5", "6", or "" for any) pins the GHz band a phone joins on (sair#143).
func (m *ScopedPortManager) SetWifi(lockID string, serials []string, on bool, band string) error {
	_, serials, err := m.lockDevices(lockID, serials)
	if err != nil {
		return err
	}
	state := dspb.WifiRequest_OFF
	if on {
		state = dspb.WifiRequest_ON
	}
	// Fan out: a lock's three phones take one join time, not three, and two
	// phones asking for opposite states at once (sair#84) do not queue.
	errs := make([]error, len(serials))
	var wg sync.WaitGroup
	for i, s := range serials {
		wg.Go(func() {
			mu := m.wifiLock(s)
			mu.Lock()
			defer mu.Unlock()
			errs[i] = m.wifiDevice(lockID, s, state, band)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// lockDevices is lockID's port and the serials of it a mid-job call acts
// on: serials as given, or every local device when empty. Naming a device
// outside the lock is errNotInLock; relayed devices are never "all".
func (m *ScopedPortManager) lockDevices(lockID string, serials []string) (*ScopedPort, []string, error) {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	if sp == nil {
		return nil, nil, errUnknownLock
	}
	if len(serials) == 0 {
		for s := range sp.Serials {
			if _, remote := sp.RemoteDevices[s]; !remote {
				serials = append(serials, s)
			}
		}
	}
	for _, s := range serials {
		if _, ok := sp.Serials[s]; !ok {
			return nil, nil, fmt.Errorf("%w: %s", errNotInLock, s)
		}
	}
	return sp, serials, nil
}

// RestartBluetooth cycles Bluetooth off and on, the same way the release
// reset does, on serials of a lock (or all of its local devices) mid-job
// (sair#156): a recovery test gets a stack restart with sair's wait for OFF
// then ON instead of force-stopping com.android.bluetooth itself, which
// races the bench's own cycle. The job keeps the lock either way; the
// outcome is in its lock log. Unlike the reset, a wedged radio is not
// rebooted mid-job: that is the release reset's call.
func (m *ScopedPortManager) RestartBluetooth(lockID string, serials []string) error {
	sp, serials, err := m.lockDevices(lockID, serials)
	if err != nil {
		return err
	}
	errs := make([]error, len(serials))
	var wg sync.WaitGroup
	for i, s := range serials {
		wg.Go(func() {
			mu := m.wifiLock(s)
			mu.Lock()
			defer mu.Unlock()
			errs[i] = m.cycleBluetooth(sp, s, " (requested by the job)", false)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// resetDevices returns a closed port's devices to baseline: no reverse
// forwards, the job's packages gone, rebooted if its uptime is over the
// bench's limit, Wi-Fi reset, Bluetooth cycled, awake and unlocked, Play Store
// enabled again. A device a newer lock
// already holds is skipped: its job owns the state now.
func (m *ScopedPortManager) resetDevices(sp *ScopedPort) {
	for serial := range sp.Serials {
		if _, remote := sp.RemoteDevices[serial]; remote {
			continue
		}
		m.resetDevice(sp, serial)
	}
}

func (m *ScopedPortManager) resetDevice(sp *ScopedPort, serial string) {
	lockID := sp.LockID
	mu := m.wifiLock(serial)
	mu.Lock()
	defer mu.Unlock()
	// This reset settles any relay record naming this lock, so a phone local to
	// the holder's proxy is not reset again when the lock expires. For a relayed
	// phone, a record naming another lock means a later relay: it is the next
	// job's now.
	rec := m.commandRouter.takeRelayRecord(serial, lockID)
	if sp.relayed && rec == nil {
		return
	}
	// A local lock resets to the baseline, even if it was also relayed. Waited
	// for before the check below: the phone can be locked anew meanwhile.
	var since *dspb.PackageSnapshot
	if sp.relayed {
		since = rec.settledSnapshot()
	}
	// Checked under the lock: an expired lock's reset can queue behind the
	// next lock's ON on this phone, and must not then undo it.
	if m.heldByAnyPort(serial) {
		return
	}
	// Packages before Wi-Fi: a suggestion a removed app registered for the
	// bench network goes with the app, so the reset's offline wait is not
	// spent on a phone that keeps re-joining (sair#76).
	failed := m.cleanDevice(sp, serial, "", since)
	// Reboot before the Wi-Fi reset: a fresh boot has nothing to forget, so
	// the offline wait is not spent twice (sair#93). The serial lock is held
	// throughout, so the next acquire of this phone waits for the boot.
	start := time.Now()
	rebooted, uptime, err := m.rebootIfStale(serial)
	reboot := &pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_REBOOT, Serial: serial, LockId: lockID, Outcome: "ok"}
	switch c := status.Code(err); {
	case err == nil && rebooted:
		slog.Info("rebooted on release: uptime was over the bench limit", "serial", serial, "lockId", lockID, "uptime", uptime)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: fmt.Sprintf("sair: rebooted after release (uptime %s)", uptime.Round(time.Minute))})
		m.waitForSource(serial)
		reboot.Detail = "uptime " + uptime.Round(time.Minute).String()
		reboot.DurationMs = time.Since(start).Milliseconds()
		m.event(reboot)
	case err == nil, c == codes.NotFound, c == codes.Unimplemented:
	default:
		slog.Warn("uptime reboot failed; the phone may be mid-boot", "serial", serial, "lockId", lockID, "error", err)
		reboot.Outcome, reboot.Detail = "failed", errText(err)
		m.event(reboot)
		failed = true
	}
	err = m.wifiDevice(lockID, serial, dspb.WifiRequest_RESET, "")
	resetFailed := func() bool { return err != nil && !wifiUnmanaged(err) && status.Code(err) != codes.NotFound }
	if resetFailed() && m.unwedge(sp, serial, "wifi", "") {
		rebooted = true
		err = m.wifiDevice(lockID, serial, dspb.WifiRequest_RESET, "")
	}
	if resetFailed() {
		slog.Warn("Wi-Fi reset failed", "serial", serial, "lockId", lockID, "error", err)
		failed = true
	}
	// A fresh boot has a fresh Bluetooth stack.
	if rebooted || m.cycleBluetooth(sp, serial, "", true) == nil {
		m.btFresh.Store(serial, true)
	} else {
		failed = true
	}
	// Awake and unlocked for the next tenant (sair#92). The next acquire
	// verifies again and fails loudly; this just makes that the rare path.
	if err := m.wakeDevice(lockID, serial); wakeFailed(err) {
		slog.Warn("wake after release failed; the next acquire of this phone will fail until it is fixed", "serial", serial, "lockId", lockID, "error", err)
		failed = true
	}
	m.setUpdates(sp, serial, false)
	// Back online between leases, or the Play Store is only ever on while
	// the phone has no network and nothing updates (sair#151). The reset
	// above already dropped the job's networks; the next acquire turns this
	// one off again for --no-wifi.
	if err == nil {
		if err := m.wifiDevice(lockID, serial, dspb.WifiRequest_ON, ""); err != nil {
			slog.Warn("rejoining the bench Wi-Fi after release failed; the phone stays offline until its next lease", "serial", serial, "lockId", lockID, "error", err)
		}
	}
	if failed {
		m.logExcerpt(sp, []string{serial})
	}
}

// setUpdates holds (on acquire) or resumes (on release) a phone's Play
// Store auto-updates (sair#151) and says so in sp's lock log. A failure
// costs nothing but the line: the job may still get through. A relayed
// device, a phone without the Play Store and an older device source are
// skipped. The caller holds the serial's lock.
func (m *ScopedPortManager) setUpdates(sp *ScopedPort, serial string, hold bool) {
	err := m.holdUpdates(serial, hold)
	switch c := status.Code(err); {
	case c == codes.NotFound, c == codes.Unimplemented:
	case err == nil && hold:
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: Play Store disabled for the lease (auto-updates resume on release)"})
	case err == nil:
	case hold:
		slog.Warn("could not disable the Play Store; an auto-update can kill the app under test", "serial", serial, "lockId", sp.LockID, "error", err)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: Play Store not disabled, an auto-update can stop the app mid-job: " + errText(err)})
	default:
		slog.Warn("could not re-enable the Play Store; the phone gets no app updates until it is", "serial", serial, "lockId", sp.LockID, "error", err)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: Play Store not re-enabled: " + errText(err)})
	}
}

// waitForSource waits, briefly, until the device list names serial again:
// a rebooted phone drops out of `adb devices` and so out of the device
// report, and the next acquire's reset would get NotFound for it until the
// report after the boot. Bounded; a phone that stays gone is not this
// function's problem.
func (m *ScopedPortManager) waitForSource(serial string) {
	if m.commandRouter == nil || m.commandRouter.ResolveSource == nil {
		return
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if m.commandRouter.ResolveSource(serial) != "" {
			return
		}
	}
	slog.Warn("rebooted phone is not back in the device list yet", "serial", serial)
}

// cleanDevice removes a phone's reverse forwards, the packages a job
// installed and a UiAutomation registration it leaked, recording what went
// in sp's lock log with note appended. The caller holds the serial's lock.
// Runs on release and again on acquire, so a job that never released is
// cleaned up by the next one. Returns whether a reset step failed.
//
// A relayed phone keeps since, what it had before the lock. With no since its
// packages are left alone: failing to clean is recoverable, deleting its
// owner's apps is not. That is reported as a failed step, whose only effect in
// resetDevice is the log excerpt: only a failed Wi-Fi reset can reboot a phone.
func (m *ScopedPortManager) cleanDevice(sp *ScopedPort, serial, note string, since *dspb.PackageSnapshot) (failed bool) {
	if err := m.removeReverse(serial); err != nil && status.Code(err) != codes.NotFound {
		slog.Warn("removing reverse forwards failed", "serial", serial, "lockId", sp.LockID, "error", err)
	}
	if sp.relayed && since == nil {
		slog.Warn("no package snapshot for this relayed lock; leaving the phone's packages alone", "serial", serial, "lockId", sp.LockID)
		m.event(&pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_PACKAGE_RESET, Serial: serial, LockId: sp.LockID, Outcome: "failed", Detail: "no package snapshot, nothing uninstalled"})
		failed = true
	} else if m.cleanPackages(sp, serial, note, since) {
		failed = true
	}
	// A leaked UiAutomation fails every later instrumentation (sair#128).
	start := time.Now()
	cleared, err := m.clearUiAutomation(serial)
	switch c := status.Code(err); {
	case err == nil && cleared != "":
		slog.Info("cleared a leaked UiAutomation registration", "serial", serial, "lockId", sp.LockID, "how", cleared)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: cleared a leaked UiAutomation registration (" + cleared + ")" + note})
		if cleared == "rebooted" {
			m.waitForSource(serial)
			m.event(&pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_REBOOT, Serial: serial, LockId: sp.LockID, Outcome: "ok", Detail: "leaked UiAutomation", DurationMs: time.Since(start).Milliseconds()})
		}
	case err == nil, c == codes.NotFound, c == codes.Unimplemented:
	default:
		slog.Warn("clearing a leaked UiAutomation failed; instrumentation on this phone will fail until it reboots", "serial", serial, "lockId", sp.LockID, "error", err)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: leaked UiAutomation registration not cleared: " + errText(err) + note})
		failed = true
	}
	return failed
}

// cleanPackages is cleanDevice's package step; true if it failed.
func (m *ScopedPortManager) cleanPackages(sp *ScopedPort, serial, note string, since *dspb.PackageSnapshot) (failed bool) {
	removed, suggestions, err := m.resetPackages(serial, since)
	event := &pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_PACKAGE_RESET, Serial: serial, LockId: sp.LockID, Outcome: "ok"}
	switch c := status.Code(err); {
	case err == nil && len(removed) > 0:
		slog.Info("uninstalled packages a job left behind", "serial", serial, "lockId", sp.LockID, "packages", removed, "suggestionsRemoved", suggestions)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: packagesRemovedService(removed, suggestions) + note})
		event.Detail = strings.Join(removed, ", ")
		m.event(event)
	case c == codes.NotFound && since != nil:
		// A relayed lock has no next acquire here to clean up after it.
		slog.Warn("phone gone at its relayed lock's reset; a job's APKs may stay on it", "serial", serial, "lockId", sp.LockID, "error", err)
		event.Outcome, event.Detail = "failed", errText(err)
		m.event(event)
		failed = true
	case err == nil, c == codes.NotFound, c == codes.Unimplemented:
		// Nothing left behind, a phone with no source here, or an older device source.
	default:
		slog.Warn("package reset failed; a job's APKs may stay on the phone", "serial", serial, "lockId", sp.LockID, "error", err)
		event.Outcome, event.Detail = "failed", errText(err)
		m.event(event)
		failed = true
	}
	return failed
}

// cycleBluetooth turns a phone's Bluetooth off and back on (sair#127) and
// says so in sp's lock log with note appended. A failure is logged, and is
// the device's warning on the device source, but does not cost the lock:
// most jobs never touch Bluetooth. A relayed device, a phone without the
// adapter dump and an older device source are skipped. The caller holds
// the serial's lock. With mayReboot a wedged radio is rebooted and the
// cycle run again (never mid-job). Returns the failure, nil when skipped.
func (m *ScopedPortManager) cycleBluetooth(sp *ScopedPort, serial, note string, mayReboot bool) error {
	start := time.Now()
	err := m.resetBluetooth(serial)
	if c := status.Code(err); err != nil && c != codes.NotFound && c != codes.Unimplemented && mayReboot && m.unwedge(sp, serial, "bluetooth", note) {
		start = time.Now()
		err = m.resetBluetooth(serial)
	}
	switch c := status.Code(err); {
	case err == nil:
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: fmt.Sprintf("sair: Bluetooth cycled off and on (%s)%s", time.Since(start).Round(100*time.Millisecond), note)})
	case c == codes.NotFound, c == codes.Unimplemented:
		return nil
	default:
		slog.Warn("Bluetooth reset failed; the phone may have Bluetooth off", "serial", serial, "lockId", sp.LockID, "error", err)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: Bluetooth reset failed: " + errText(err) + note})
	}
	return err
}

// unwedge (sair#139) has the device source reboot a phone whose radio
// reset just failed if the radio's service no longer answers, records that
// in sp's lock log with note appended, and returns whether it rebooted, so
// the caller re-runs the reset. The caller holds the serial's lock, so the
// next acquire waits for the boot.
func (m *ScopedPortManager) unwedge(sp *ScopedPort, serial, radio, note string) bool {
	start := time.Now()
	rebooted, err := m.rebootIfWedged(serial, radio)
	switch c := status.Code(err); {
	case err == nil && rebooted:
		slog.Warn("radio wedged; rebooted", "serial", serial, "lockId", sp.LockID, "radio", radio)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: radio wedged (" + radio + "), rebooted" + note})
		m.waitForSource(serial)
		m.event(&pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_REBOOT, Serial: serial, LockId: sp.LockID, Outcome: "ok", Detail: "wedged " + radio, DurationMs: time.Since(start).Milliseconds()})
		return true
	case err == nil, c == codes.NotFound, c == codes.Unimplemented:
	default:
		slog.Warn("radio wedged; not rebooted", "serial", serial, "lockId", sp.LockID, "radio", radio, "error", err)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: "sair: radio wedged (" + radio + "), not rebooted: " + errText(err) + note})
	}
	return false
}

// packagesRemovedService is the lock log line for a cleanup, e.g.
// "sair: uninstalled com.example.app, com.example.app.test (Wi-Fi suggestions removed with them: bench wpa2-psk)".
func packagesRemovedService(removed, suggestions []string) string {
	s := "sair: uninstalled " + strings.Join(removed, ", ")
	if len(suggestions) > 0 {
		s += " (Wi-Fi suggestions removed with them: " + strings.Join(suggestions, "; ") + ")"
	}
	return s
}

func (m *ScopedPortManager) heldByAnyPort(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sp := range m.scopedPorts {
		if _, ok := sp.Serials[serial]; ok {
			return true
		}
	}
	return false
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

	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		listener.Close()
		return nil, err
	}

	sp := &ScopedPort{
		DeviceToken:   hex.EncodeToString(token),
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
		conns:         make(map[net.Conn]struct{}),
	}
	sp.lastClient.Store(sp.CreatedAt.UnixNano())

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
	sp.closeConns()    // nothing may still be driving a phone during its reset
	<-sp.keepaliveDone // wait for runKeepalive to stop touching sp.Log before a caller drains it

	slog.Info("closed scoped port", "port", sp.Port, "lockId", lockID)
	// Every way a lock ends comes through here, so this is where a device is
	// put back to baseline: before Release hands it back to the orchestrator.
	m.resetDevices(sp)
	return true
}

// OnLockExpired is the session's callback: the orchestrator released the lock,
// so the scoped port is dead and any phone relayed under it is reset (see
// resetDevice). Idempotent.
func (m *ScopedPortManager) OnLockExpired(lockID string) {
	if m.CloseScopedPort(lockID) {
		slog.Warn("lock expired on orchestrator; closed scoped port", "lockId", lockID)
	}
	m.resetRelayed(lockID)
}

// resetRelayed resets the phones relayed from here under an ended lock. Its
// holder's job log is on another proxy, so the stand-in port has no log and
// the lock log lines of the reset go nowhere; the events still report the lock.
func (m *ScopedPortManager) resetRelayed(lockID string) {
	sp := &ScopedPort{LockID: lockID, relayed: true}
	var wg sync.WaitGroup
	m.commandRouter.relayLocks.Range(func(serial, id any) bool {
		if id.(*relayRecord).lockID == lockID {
			wg.Go(func() { m.resetDevice(sp, serial.(string)) })
		}
		return true
	})
	wg.Wait()
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

// ShutdownAll closes all scoped ports and releases all locks, as
// "proxy_restart" so the account's lock list (sair-locks) can tell a proxy
// going away from a job that cancelled.
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
		if _, err := m.commandRouter.ReleaseLock(sp.LockID, "proxy_restart", entries); err != nil {
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
		if !sp.trackConn(conn) {
			continue
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
		sp.openConns.Add(1)
		sp.lastClient.Store(time.Now().UnixNano())
		go func() {
			defer func() {
				sp.lastClient.Store(time.Now().UnixNano())
				sp.openConns.Add(-1)
				sp.untrackConn(conn)
			}()
			adbConn.Handle()
		}()
	}
}

// idleFor is how long sp has had no client connected, 0 while one is.
func (sp *ScopedPort) idleFor() time.Duration {
	if sp.openConns.Load() > 0 {
		return 0
	}
	return time.Since(time.Unix(0, sp.lastClient.Load()))
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
			if idle := sp.idleFor(); m.IdleTimeout > 0 && idle >= m.IdleTimeout {
				slog.Warn("no adb client on the scoped port; releasing the abandoned lock", "lockId", sp.LockID, "port", sp.Port, "idle", idle.Round(time.Second))
				sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Service: fmt.Sprintf("sair: released, no adb client for %s (the job is gone)", idle.Round(time.Minute))})
				// Asynchronously, like the expiry below: Release waits on keepaliveDone.
				go m.Release(sp.LockID, "cancelled")
				return
			}
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
