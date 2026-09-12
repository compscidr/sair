package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
