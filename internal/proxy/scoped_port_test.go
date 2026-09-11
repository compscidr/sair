package proxy

import (
	"sync"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
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
	sp, err := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}})
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
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}})

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
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}})

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
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}})

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

// TestScopedPortBeatNeverDuplicatesFlushedEntries guards against the beat
// branch re-sending what the flush already delivered: on the stream, the
// beat must never carry log entries (only the unary fallback does), so an
// entry the flush shipped cannot also ride a later heartbeat and be
// double-counted by the orchestrator after a partial-failure requeue.
func TestScopedPortBeatNeverDuplicatesFlushedEntries(t *testing.T) {
	f := newFakeSessionServer()
	m, _ := newManagerOnSession(t, f, 1) // 1 s heartbeat, 20 ms flush interval
	sp, _ := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}})

	sp.Log.Record(&pb.LockLogEntry{Service: "shell:once"})
	waitFor(t, "log flushed", func() bool { msgs, _ := f.snapshot(); l, _ := countKinds(msgs); return l >= 1 })

	// Let a couple of heartbeat ticks pass so the beat had its shot at this entry.
	time.Sleep(2200 * time.Millisecond)

	msgs, _ := f.snapshot()
	occurrences := 0
	for _, msg := range msgs {
		if hb := msg.GetHeartbeat(); hb != nil {
			if len(hb.Log) != 0 {
				t.Errorf("heartbeat carried a log on the stream: %+v", hb)
			}
			for _, e := range hb.Log {
				if e.Service == "shell:once" {
					occurrences++
				}
			}
		}
		if l := msg.GetLockLog(); l != nil {
			for _, e := range l.Entries {
				if e.Service == "shell:once" {
					occurrences++
				}
			}
		}
	}
	if occurrences != 1 {
		t.Errorf("entry appeared %d times across the stream, want exactly 1", occurrences)
	}
}

func TestScopedPortClosesOnLockExpiredFromStream(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	r := &CommandRouter{orchClient: client, apiKey: "key-1", proxyID: "host-a"}
	m := NewScopedPortManager(r, NewDeviceListTracker(r), 3600)
	r.StartSession("v1", 3600, m.OnLockExpired, nil)
	t.Cleanup(r.sess.stop)
	waitFor(t, "connected", r.sess.connected)
	if _, err := m.CreateScopedPort("lock-1", map[string]struct{}{"DEV1": {}}); err != nil {
		t.Fatalf("CreateScopedPort: %v", err)
	}

	f.push <- &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_LockExpired{LockExpired: &pb.LockExpired{LockId: "lock-1"}}}
	waitFor(t, "port closed", func() bool { return len(m.GetAllScopedPorts()) == 0 })
}
