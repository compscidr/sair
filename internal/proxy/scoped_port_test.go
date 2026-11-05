package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// newManagerOnSession returns a manager whose router streams to f, with the
// flush interval shortened so tests finish quickly.
func newManagerOnSession(t *testing.T, f *fakeSessionServer, heartbeatSecs int64) (*ScopedPortManager, *CommandRouter) {
	t.Helper()
	r := newRouterWithSession(t, f)
	m := NewScopedPortManager(r, NewDeviceListTracker(r), heartbeatSecs)
	m.flushInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		for _, sp := range m.GetAllScopedPorts() {
			m.CloseScopedPort(sp.LockID)
		}
	})
	return m, r
}

func countKinds(msgs []*pb.ProxyMessage) (logs, beats int) {
	for _, m := range msgs {
		if m.GetLockLog() != nil {
			logs++
		}
		if m.GetHeartbeat() != nil {
			beats++
		}
	}
	return
}

func TestScopedPortFlushesLogWithinInterval(t *testing.T) {
	f := newFakeSessionServer()
	m, _ := newManagerOnSession(t, f, 3600)
	sp, err := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)
	if err != nil {
		t.Fatalf("CreateScopedPort: %v", err)
	}
	sp.Log.Record(&pb.LockLogEntry{Service: "shell:ls", Serial: "DEV1"})
	waitFor(t, "lock log on stream", func() bool { msgs, _ := f.snapshot(); l, _ := countKinds(msgs); return l == 1 })
	msgs, _ := f.snapshot()
	var got *pb.LockLog
	for _, msg := range msgs {
		if msg.GetLockLog() != nil {
			got = msg.GetLockLog()
		}
	}
	if got.LockId != "lock-1" || len(got.Entries) != 1 || got.Entries[0].Service != "shell:ls" {
		t.Errorf("flushed log wrong: %+v", got)
	}
	// Nothing new: no further log messages, no heartbeat this early.
	time.Sleep(60 * time.Millisecond)
	msgs, _ = f.snapshot()
	if l, b := countKinds(msgs); l != 1 || b != 0 {
		t.Errorf("idle port kept sending: logs=%d beats=%d", l, b)
	}
}

func TestScopedPortRequeuesWhileSessionDownAndDeliversAfterReconnect(t *testing.T) {
	f := newFakeSessionServer()
	m, r := newManagerOnSession(t, f, 3600)
	r.sess.backoffMin, r.sess.backoffMax = 20*time.Millisecond, 20*time.Millisecond
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)

	f.kill <- struct{}{}
	waitFor(t, "stream down", func() bool { return !r.sess.connected() })
	sp.Log.Record(&pb.LockLogEntry{Service: "shell:while-down"})
	time.Sleep(50 * time.Millisecond) // a few flush ticks with no stream

	waitFor(t, "reconnected", r.sess.connected)
	waitFor(t, "entry delivered after reconnect", func() bool {
		msgs, _ := f.snapshot()
		for _, msg := range msgs {
			if l := msg.GetLockLog(); l != nil && len(l.Entries) == 1 && l.Entries[0].Service == "shell:while-down" {
				return true
			}
		}
		return false
	})
}

// TestScopedPortReleaseWaitsForInFlightFlushBeforeDraining guards against
// Release/ShutdownAll stranding entries: CloseScopedPort must wait for
// runKeepalive to stop touching the log before Release drains it. This pins
// the race deterministically with the beforeSendLockLog test hook instead of
// hoping a sleep lands the drain after a background failure: the flush
// goroutine is parked mid-SendLockLog, holding the drained entry, while
// Release is given every chance to (wrongly) drain an empty log first.
func TestScopedPortReleaseWaitsForInFlightFlushBeforeDraining(t *testing.T) {
	f := newFakeSessionServer()
	m, r := newManagerOnSession(t, f, 3600)
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)

	flushStarted := make(chan struct{})
	releaseCalled := make(chan struct{})
	var once sync.Once
	r.beforeSendLockLog = func() {
		once.Do(func() { close(flushStarted) })
		<-releaseCalled
	}

	sp.Log.Record(&pb.LockLogEntry{Service: "shell:in-flight"})
	<-flushStarted // the flush goroutine is now mid-SendLockLog, holding the drained entry

	releaseDone := make(chan bool, 1)
	go func() { releaseDone <- m.Release("lock-1", "success") }()
	time.Sleep(20 * time.Millisecond)
	if rel := f.release(); rel != nil {
		t.Fatalf("Release drained before the in-flight flush finished: %+v", rel)
	}

	f.kill <- struct{}{}
	waitFor(t, "stream down", func() bool { return !r.sess.connected() })
	close(releaseCalled) // the hook returns; the pending Send now fails against the killed stream

	select {
	case ok := <-releaseDone:
		if !ok {
			t.Fatalf("Release reported failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Release never returned")
	}

	rel := f.release()
	if rel == nil || rel.LockId != "lock-1" || len(rel.Log) != 1 || rel.Log[0].Service != "shell:in-flight" {
		t.Errorf("release did not carry the entry stranded by the in-flight flush: %+v", rel)
	}
}

func TestScopedPortHeartbeatsOnlyWhenNoLogWasFlushed(t *testing.T) {
	f := newFakeSessionServer()
	m, _ := newManagerOnSession(t, f, 1) // 1 s heartbeat for the test
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)

	// Keep the log busy for ~1.2s: every flush carries an entry, so no heartbeat should fire.
	stop := time.After(1200 * time.Millisecond)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			sp.Log.Record(&pb.LockLogEntry{Service: "shell:busy"})
		case <-stop:
			break loop
		}
	}
	msgs, _ := f.snapshot()
	if _, b := countKinds(msgs); b != 0 {
		t.Errorf("heartbeat fired although logs were flowing: beats=%d", b)
	}
	// Now idle: the next heartbeat tick must send one.
	waitFor(t, "heartbeat when idle", func() bool { msgs, _ := f.snapshot(); _, b := countKinds(msgs); return b >= 1 })
}

// TestScopedPortBeatDoesNotConsumePendingEntry guards against the beat
// branch draining (and thereby losing or duplicating) an entry that is
// simply waiting for its next flush tick: on the stream, a beat that fires
// while an entry is buffered must neither ship it (no LockLog) nor carry it
// on the heartbeat (empty Log) nor remove it from the buffer — it must stay
// put for the flush. flushInterval is set far longer than the test so the
// flush cannot fire and mask a bug in the beat branch.
func TestScopedPortBeatDoesNotConsumePendingEntry(t *testing.T) {
	f := newFakeSessionServer()
	r := newRouterWithSession(t, f)
	m := NewScopedPortManager(r, NewDeviceListTracker(r), 1) // 1 s heartbeat
	m.flushInterval = 10 * time.Second                       // flush must not fire during the test
	t.Cleanup(func() {
		for _, sp := range m.GetAllScopedPorts() {
			m.CloseScopedPort(sp.LockID)
		}
	})
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)

	sp.Log.Record(&pb.LockLogEntry{Service: "shell:pending"})
	waitFor(t, "a heartbeat reaches the fake", func() bool {
		msgs, _ := f.snapshot()
		_, b := countKinds(msgs)
		return b >= 1
	})

	msgs, _ := f.snapshot()
	for _, msg := range msgs {
		if hb := msg.GetHeartbeat(); hb != nil && len(hb.Log) != 0 {
			t.Errorf("heartbeat carried a log while an entry was pending: %+v", hb)
		}
		if msg.GetLockLog() != nil {
			t.Errorf("unexpected LockLog while the flush is parked: %+v", msg)
		}
	}

	drained := sp.Log.Drain()
	if len(drained) != 1 || drained[0].Service != "shell:pending" {
		t.Errorf("beat must leave the pending entry for the flush, got %+v", drained)
	}
}

// TestScopedPortFallbackHeartbeatCarriesLog guards the unary-fallback path:
// with no stream ever up, the flush ticker can never ship a log (fix #2 makes
// it skip draining entirely), so the drained entries must ride the unary
// heartbeat instead of being lost.
func TestScopedPortFallbackHeartbeatCarriesLog(t *testing.T) {
	u := &unaryRecorder{fakeSessionServer: *newFakeSessionServer()}
	u.unimplement = true
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterOrchestratorServer(srv, u)
	go srv.Serve(lis)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(); srv.Stop() })
	r := &CommandRouter{orchClient: pb.NewOrchestratorClient(conn), apiKey: "key-1", proxyID: "host-a"}
	r.StartSession("v1", 60, nil, nil, nil)
	t.Cleanup(r.sess.stop)
	waitFor(t, "fallback", r.sess.unaryFallback)

	m := NewScopedPortManager(r, NewDeviceListTracker(r), 1) // 1s heartbeat
	sp, err := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)
	if err != nil {
		t.Fatalf("CreateScopedPort: %v", err)
	}
	t.Cleanup(func() { m.CloseScopedPort(sp.LockID) })

	sp.Log.Record(&pb.LockLogEntry{Service: "shell:fallback-entry"})
	waitFor(t, "heartbeat carries log", func() bool { return u.getLastBeat() != nil })

	beat := u.getLastBeat()
	if len(beat.Log) != 1 || beat.Log[0].Service != "shell:fallback-entry" {
		t.Errorf("fallback heartbeat did not carry the drained log: %+v", beat)
	}
	// No stream ever exists in fallback, so LockLog can never be sent; the
	// only Session() call is the single initial attempt that got Unimplemented.
	if n := u.attemptCount(); n != 1 {
		t.Errorf("fallback must not attempt the stream again; saw %d Session() attempts", n)
	}
}

func TestScopedPortClosesOnLockExpiredFromStream(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	r := &CommandRouter{orchClient: client, apiKey: "key-1", proxyID: "host-a"}
	m := NewScopedPortManager(r, NewDeviceListTracker(r), 3600)
	r.StartSession("v1", 3600, m.OnLockExpired, nil, nil)
	t.Cleanup(r.sess.stop)
	waitFor(t, "connected", r.sess.connected)
	if _, err := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil); err != nil {
		t.Fatalf("CreateScopedPort: %v", err)
	}

	f.push <- &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_LockExpired{LockExpired: &pb.LockExpired{LockId: "lock-1"}}}
	waitFor(t, "port closed", func() bool { return len(m.GetAllScopedPorts()) == 0 })
}

func TestScopedPortFlushLogsUnexpectedSendErrors(t *testing.T) {
	f := newFakeSessionServer()
	m, r := newManagerOnSession(t, f, 3600)
	sp, err := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}, nil)
	if err != nil {
		t.Fatalf("CreateScopedPort: %v", err)
	}
	// Make the next send fail with something that is not a dead stream: a plain
	// error the transport would not produce, so it must surface as a warning.
	r.beforeSendLockLog = func() {
		r.sess.mu.Lock()
		r.sess.stream = stubErrStream{err: errors.New("marshal: message too large")}
		r.sess.mu.Unlock()
	}
	var buf logBuffer
	restore := captureSlog(&buf)
	defer restore()
	sp.Log.Record(&pb.LockLogEntry{Service: "shell:ls"})
	waitFor(t, "warning logged", func() bool { return buf.contains("lock log flush failed") })
	if !buf.contains("lock-1") {
		t.Errorf("warning does not name the lock: %s", buf.String())
	}
}

// logBuffer collects slog output so a test can assert on what was logged.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) contains(s string) bool { return strings.Contains(l.String(), s) }

// captureSlog routes the default logger into buf until the returned func is called.
func captureSlog(buf *logBuffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}

// Acquire puts every local device in the requested Wi-Fi state before
// returning: ON with --wifi, OFF with --no-wifi (sair#86), never "whatever the
// last release left". A device that cannot get there costs the lock.
func TestAcquireVerifiesWifiStateEitherWay(t *testing.T) {
	for _, wifi := range []bool{true, false} {
		fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
		m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
		var states []dspb.WifiRequest_State
		m.setWifi = func(_ string, state dspb.WifiRequest_State, _ string) error {
			states = append(states, state)
			return nil
		}
		m.removeReverse = func(string) error { return nil }
		if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, wifi, "", false, "", ""); err != nil {
			t.Fatal(err)
		}
		want := dspb.WifiRequest_OFF
		if wifi {
			want = dspb.WifiRequest_ON
		}
		if len(states) != 1 || states[0] != want {
			t.Errorf("wifi=%v: states = %v, want [%v]", wifi, states, want)
		}
		m.CloseScopedPort("L1")

		m.setWifi = func(string, dspb.WifiRequest_State, string) error {
			return status.Error(codes.DeadlineExceeded, "still on Wi-Fi")
		}
		if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, wifi, "", false, "", ""); status.Code(err) != codes.DeadlineExceeded {
			t.Errorf("wifi=%v: err = %v, want DeadlineExceeded", wifi, err)
		}
		if fake.lastRelease == nil || fake.lastRelease.LockId != "L1" {
			t.Errorf("wifi=%v: lock not released after the Wi-Fi failure", wifi)
		}
	}
}

// Acquire hands out an awake, unlocked phone (sair#92): a keyguard the
// device source cannot dismiss costs the lock, like a bad Wi-Fi state.
func TestAcquireFailsWhenPhoneStaysLocked(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	m.setWifi = func(string, dspb.WifiRequest_State, string) error { return nil }
	m.removeReverse = func(string) error { return nil }
	woke := 0
	m.wake = func(string) error {
		woke++
		return status.Error(codes.FailedPrecondition, "A: screen locked")
	}
	if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, true, "", false, "", ""); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
	if fake.lastRelease == nil || fake.lastRelease.LockId != "L1" {
		t.Error("lock not released after the wake failure")
	}
	if woke == 0 {
		t.Error("wake never called on acquire")
	}
}

// Release resets in this order: packages, leaked UiAutomation, uptime
// reboot, Wi-Fi reset, wake (sair#128, sair#93, sair#92), with no Bluetooth
// cycle after a reboot. The UiAutomation cleanup and the reboot are written
// to the lock log.
func TestReleaseRebootsStalePhoneThenResetsAndWakes(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	var steps []string
	m.removeReverse = func(string) error { return nil }
	m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) { steps = append(steps, "packages"); return nil, nil, nil }
	m.clearUiAutomation = func(string) (string, error) {
		steps = append(steps, "uiautomation")
		return "killed 2190 app_process", nil
	}
	m.rebootIfStale = func(string) (bool, time.Duration, error) {
		steps = append(steps, "reboot")
		return true, 30 * time.Hour, nil
	}
	m.setWifi = func(_ string, st dspb.WifiRequest_State, _ string) error {
		steps = append(steps, "wifi "+st.String())
		return nil
	}
	m.wake = func(string) error { steps = append(steps, "wake"); return nil }
	m.resetBluetooth = func(string) error { steps = append(steps, "bluetooth"); return nil }
	if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	steps = nil
	m.Release("L1", "ok")
	want := []string{"packages", "uiautomation", "reboot", "wifi RESET", "wake", "wifi ON"}
	if !reflect.DeepEqual(steps, want) {
		t.Errorf("release steps = %q, want %q", steps, want)
	}
	found, uia := false, false
	for _, e := range fake.lastRelease.GetLog() {
		if strings.Contains(e.Service, "rebooted after release (uptime 30h0m0s)") {
			found = true
		}
		uia = uia || e.Service == "sair: cleared a leaked UiAutomation registration (killed 2190 app_process)"
	}
	if !uia {
		t.Errorf("lock log has no UiAutomation line: %v", fake.lastRelease.GetLog())
	}
	if !found {
		t.Errorf("lock log has no reboot line: %v", fake.lastRelease.GetLog())
	}
}

// The Play Store is disabled for the lease and enabled again as the last
// release step (sair#151); a phone that refuses says so in the lock log but
// keeps the lock.
func TestPlayStoreHeldForTheLease(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	var steps []string
	m.removeReverse = func(string) error { return nil }
	m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) { return nil, nil, nil }
	m.rebootIfStale = func(string) (bool, time.Duration, error) { return false, time.Hour, nil }
	m.setWifi = func(_ string, st dspb.WifiRequest_State, _ string) error { steps = append(steps, "wifi "+st.String()); return nil }
	m.wake = func(string) error { steps = append(steps, "wake"); return nil }
	m.resetBluetooth = func(string) error { steps = append(steps, "bluetooth"); return nil }
	m.holdUpdates = func(_ string, hold bool) error {
		steps = append(steps, "hold "+strconv.FormatBool(hold))
		if hold {
			return status.Error(codes.FailedPrecondition, "A: pm disable-user com.android.vending: java.lang.SecurityException: Cannot disable system packages.")
		}
		return nil
	}
	if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", ""); err != nil {
		t.Fatalf("a refused hold cost the lock: %v", err)
	}
	if want := []string{"bluetooth", "hold true", "wake", "wifi OFF"}; !reflect.DeepEqual(steps, want) {
		t.Errorf("acquire steps = %q, want %q", steps, want)
	}
	steps = nil
	m.Release("L1", "ok")
	if want := []string{"wifi RESET", "bluetooth", "wake", "hold false", "wifi ON"}; !reflect.DeepEqual(steps, want) {
		t.Errorf("release steps = %q, want %q", steps, want)
	}
	var lines []string
	for _, e := range fake.lastRelease.GetLog() {
		if strings.Contains(e.Service, "Play Store") {
			lines = append(lines, e.Service)
		}
	}
	if want := []string{"sair: Play Store not disabled, an auto-update can stop the app mid-job: A: pm disable-user com.android.vending: java.lang.SecurityException: Cannot disable system packages."}; !reflect.DeepEqual(lines, want) {
		t.Errorf("lock log Play Store lines = %q, want %q", lines, want)
	}
}

// Bluetooth is cycled on release after the Wi-Fi reset (sair#127), and on
// acquire only when no release reset did it since: the first acquire after
// a proxy start, not the one after a clean release. A failure is in the
// lock log but does not cost the lock.
func TestBluetoothCycledOnReleaseAndOnAcquireOnlyWhenNeeded(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	var steps []string
	m.removeReverse = func(string) error { return nil }
	m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) { return nil, nil, nil }
	m.rebootIfStale = func(string) (bool, time.Duration, error) { return false, time.Hour, nil }
	m.setWifi = func(_ string, st dspb.WifiRequest_State, _ string) error { steps = append(steps, "wifi "+st.String()); return nil }
	m.wake = func(string) error { steps = append(steps, "wake"); return nil }
	btErr := status.Error(codes.DeadlineExceeded, "A Bluetooth not back on within 30s: adapter is OFF")
	m.resetBluetooth = func(string) error { steps = append(steps, "bluetooth"); return btErr }

	if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", ""); err != nil {
		t.Fatalf("a Bluetooth failure cost the lock: %v", err)
	}
	if want := []string{"bluetooth", "wake", "wifi OFF"}; !reflect.DeepEqual(steps, want) {
		t.Errorf("first acquire steps = %q, want %q", steps, want)
	}
	m.resetBluetooth = func(string) error { steps = append(steps, "bluetooth"); return nil }
	steps = nil
	m.Release("L1", "ok")
	if want := []string{"wifi RESET", "bluetooth", "wake", "wifi ON"}; !reflect.DeepEqual(steps, want) {
		t.Errorf("release steps = %q, want %q", steps, want)
	}
	var lines []string
	for _, e := range fake.lastRelease.GetLog() {
		if strings.Contains(e.Service, "Bluetooth") {
			lines = append(lines, e.Service)
		}
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "reset failed: A Bluetooth not back on") || !strings.Contains(lines[1], "cycled off and on") {
		t.Errorf("lock log Bluetooth lines = %q", lines)
	}
	steps = nil
	if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"wake", "wifi OFF"}; !reflect.DeepEqual(steps, want) {
		t.Errorf("acquire after a clean release steps = %q, want %q", steps, want)
	}
}

// TestScopedPortReleasesLockWithNoClient guards sair#106: a job killed with
// its lock held (runner lost) must not have the heartbeat keep the lock alive
// forever. A connected client, even a silent one, keeps it.
func TestScopedPortReleasesLockWithNoClient(t *testing.T) {
	f := newFakeSessionServer()
	m, _ := newManagerOnSession(t, f, 1)
	m.IdleTimeout = 1500 * time.Millisecond
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{}, nil)

	conn, err := net.Dial("tcp", sp.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if f.release() != nil {
		t.Fatal("released while a client was connected")
	}
	conn.Close()
	waitFor(t, "idle release", func() bool { return f.release() != nil })
	rel := f.release()
	if rel.LockId != "lock-1" || rel.Status != "cancelled" || len(rel.Log) == 0 || !strings.Contains(rel.Log[len(rel.Log)-1].Service, "no adb client") {
		t.Errorf("release = %+v", rel)
	}
}

// A radio reset that fails on a wedged radio reboots the phone and runs
// again (sair#139); the lock log says so. After a Wi-Fi reboot the
// Bluetooth stack is fresh, so it is not cycled.
func TestReleaseRebootsAWedgedRadioAndResetsAgain(t *testing.T) {
	for _, radio := range []string{"wifi", "bluetooth"} {
		fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
		m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
		var steps []string
		m.removeReverse = func(string) error { return nil }
		m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) { return nil, nil, nil }
		m.rebootIfStale = func(string) (bool, time.Duration, error) { return false, time.Hour, nil }
		m.wake = func(string) error { return nil }
		wedged := true
		hang := status.Error(codes.DeadlineExceeded, "radio not back")
		m.setWifi = func(_ string, st dspb.WifiRequest_State, _ string) error {
			steps = append(steps, "wifi "+st.String())
			if radio == "wifi" && st == dspb.WifiRequest_RESET && wedged {
				return hang
			}
			return nil
		}
		m.resetBluetooth = func(string) error {
			steps = append(steps, "bluetooth")
			if radio == "bluetooth" && wedged {
				return hang
			}
			return nil
		}
		m.rebootIfWedged = func(_, r string) (bool, error) {
			steps = append(steps, "unwedge "+r)
			wedged = false
			return true, nil
		}
		if _, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", ""); err != nil {
			t.Fatal(err)
		}
		steps, wedged = nil, true
		m.Release("L1", "ok")
		want := map[string][]string{
			"wifi":      {"wifi RESET", "unwedge wifi", "wifi RESET", "wifi ON"},
			"bluetooth": {"wifi RESET", "bluetooth", "unwedge bluetooth", "bluetooth", "wifi ON"},
		}[radio]
		if !reflect.DeepEqual(steps, want) {
			t.Errorf("%s: release steps = %q, want %q", radio, steps, want)
		}
		found := false
		for _, e := range fake.lastRelease.GetLog() {
			found = found || e.Service == "sair: radio wedged ("+radio+"), rebooted"
		}
		if !found {
			t.Errorf("%s: lock log has no wedged line: %v", radio, fake.lastRelease.GetLog())
		}
	}
}

// A proxy shutting down releases its locks as "proxy_restart" (sair#137), so
// the lock list tells it apart from a cancelled job.
func TestShutdownAllReleasesAsProxyRestart(t *testing.T) {
	f := newFakeSessionServer()
	m, _ := newManagerOnSession(t, f, 1)
	if _, err := m.CreateScopedPort("lock-1", map[string]struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	m.ShutdownAll()
	waitFor(t, "shutdown release", func() bool { return f.release() != nil })
	if rel := f.release(); rel.LockId != "lock-1" || rel.Status != "proxy_restart" {
		t.Errorf("release = %+v", rel)
	}
}

// Ending a lock ends its connections, before the reset: a client still
// talking through the port must not be driving the phone while it resets.
func TestCloseScopedPortClosesOpenConnections(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	m.setWifi = func(string, dspb.WifiRequest_State, string) error { return nil }
	sp, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(sp.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitFor(t, "connection accepted", func() bool { return sp.openConns.Load() == 1 })

	var readErr error
	m.removeReverse = func(string) error { // the reset's first step
		client.SetReadDeadline(time.Now().Add(time.Second))
		_, readErr = client.Read(make([]byte, 1))
		return nil
	}
	m.CloseScopedPort("L1")
	if !errors.Is(readErr, io.EOF) {
		t.Errorf("client read during the reset: err = %v, want EOF", readErr)
	}
}

// oneConnListener hands out one connection, then reports itself closed.
type oneConnListener struct {
	net.Listener
	conn net.Conn
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if c := l.conn; c != nil {
		l.conn = nil
		return c, nil
	}
	return nil, net.ErrClosed
}

// A connection Accept returns just as the port closes missed the close's
// sweep, so the accept loop has to close it itself.
func TestConnectionAcceptedAsPortClosesIsClosed(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	client, server := net.Pipe()
	defer client.Close()
	sp := &ScopedPort{LockID: "L1", Port: 1, listener: &oneConnListener{conn: server}, stopCh: make(chan struct{})}
	close(sp.stopCh)
	sp.closeConns()
	m.runAcceptLoop(sp)

	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("client read: err = %v, want EOF", err)
	}
}
