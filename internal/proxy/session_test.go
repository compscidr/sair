package proxy

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestSessionProtoShapes(t *testing.T) {
	hello := &pb.ProxyMessage{Msg: &pb.ProxyMessage_Hello{Hello: &pb.ProxyHello{Version: "v1", ProxyId: "host-a"}}}
	if hello.GetHello().GetProxyId() != "host-a" {
		t.Errorf("hello proxy_id round trip failed")
	}
	exp := &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_LockExpired{LockExpired: &pb.LockExpired{LockId: "lock-1"}}}
	if exp.GetLockExpired().GetLockId() != "lock-1" {
		t.Errorf("lock_expired round trip failed")
	}
	req := &pb.AcquireLockRequest{ProxyId: "host-a"}
	if req.GetProxyId() != "host-a" {
		t.Errorf("acquire proxy_id round trip failed")
	}
}

// fakeSessionServer is an in-process orchestrator that only implements Session.
// It records every inbound message, answers hello with welcome, and lets a
// test push messages down or kill the stream.
type fakeSessionServer struct {
	pb.UnimplementedOrchestratorServer
	mu          sync.Mutex
	received    []*pb.ProxyMessage
	apiKeys     []string
	streams     int
	attempts    int // incremented on every Session() call, even one refused as Unimplemented
	refuseFirst int // the first refuseFirst attempts are refused with Unavailable, to force backoff to climb
	unimplement bool
	push        chan *pb.OrchestratorMessage
	kill        chan struct{}
	lastRelease *pb.ReleaseLockRequest
}

// ReleaseLock is the unary handler used by tests that check what a client
// drained and sent on release (e.g. entries requeued by a failed flush).
func (f *fakeSessionServer) ReleaseLock(ctx context.Context, in *pb.ReleaseLockRequest) (*pb.ReleaseLockResponse, error) {
	f.mu.Lock()
	f.lastRelease = in
	f.mu.Unlock()
	return &pb.ReleaseLockResponse{Released: true}, nil
}

func (f *fakeSessionServer) release() *pb.ReleaseLockRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRelease
}

func newFakeSessionServer() *fakeSessionServer {
	return &fakeSessionServer{push: make(chan *pb.OrchestratorMessage, 8), kill: make(chan struct{}, 1)}
}

func (f *fakeSessionServer) Session(stream grpc.BidiStreamingServer[pb.ProxyMessage, pb.OrchestratorMessage]) error {
	f.mu.Lock()
	f.attempts++
	refuse := f.attempts <= f.refuseFirst
	f.mu.Unlock()
	if refuse {
		return status.Error(codes.Unavailable, "not yet")
	}
	if f.unimplement {
		return status.Error(codes.Unimplemented, "no Session here")
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.streams++
	f.apiKeys = append(f.apiKeys, md.Get("x-api-key")...)
	f.mu.Unlock()

	inbound := make(chan *pb.ProxyMessage)
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			inbound <- m
		}
	}()
	for {
		select {
		case m := <-inbound:
			f.mu.Lock()
			f.received = append(f.received, m)
			f.mu.Unlock()
			if m.GetHello() != nil {
				if err := stream.Send(&pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_Welcome{Welcome: &pb.OrchestratorWelcome{Version: "orch-test", TenantId: "user-1"}}}); err != nil {
					return err
				}
			}
		case m := <-f.push:
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-f.kill:
			return status.Error(codes.Unavailable, "killed by test")
		case err := <-errc:
			return err
		}
	}
}

func (f *fakeSessionServer) snapshot() (msgs []*pb.ProxyMessage, streams int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.ProxyMessage(nil), f.received...), f.streams
}

func (f *fakeSessionServer) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// startFakeOrchestrator serves the fake over bufconn and returns a client to it.
func startFakeOrchestrator(t *testing.T, f *fakeSessionServer) pb.OrchestratorClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterOrchestratorServer(srv, f)
	go srv.Serve(lis)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { conn.Close(); srv.Stop() })
	return pb.NewOrchestratorClient(conn)
}

// waitFor polls cond every 10ms until it is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionSendsHelloFirstWithAuth(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	s := newSession(client, "key-1", "host-a", "v9.9.9", 60, nil, nil)
	s.start()
	defer s.stop()

	waitFor(t, "hello", func() bool { msgs, _ := f.snapshot(); return len(msgs) >= 1 })
	msgs, _ := f.snapshot()
	h := msgs[0].GetHello()
	if h == nil || h.ProxyId != "host-a" || h.Version != "v9.9.9" {
		t.Fatalf("first message is not the expected hello: %+v", msgs[0])
	}
	if h.DeviceReportIntervalS != 10 || h.LockHeartbeatIntervalS != 60 {
		t.Errorf("hello intervals not filled: %+v", h)
	}
	f.mu.Lock()
	keys := f.apiKeys
	f.mu.Unlock()
	if len(keys) != 1 || keys[0] != "key-1" {
		t.Errorf("api key not on stream metadata: %v", keys)
	}
	waitFor(t, "connected", s.connected)
}

func TestSessionReconnectsAndResendsHello(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	connections := 0
	var mu sync.Mutex
	s := newSession(client, "key-1", "host-a", "v1", 60, nil, func() { mu.Lock(); connections++; mu.Unlock() })
	s.backoffMin, s.backoffMax = 10*time.Millisecond, 20*time.Millisecond
	s.start()
	defer s.stop()

	waitFor(t, "first stream", func() bool { _, n := f.snapshot(); return n == 1 })
	f.kill <- struct{}{}
	waitFor(t, "second stream", func() bool { _, n := f.snapshot(); return n == 2 })
	waitFor(t, "second hello", func() bool {
		msgs, _ := f.snapshot()
		hellos := 0
		for _, m := range msgs {
			if m.GetHello() != nil {
				hellos++
			}
		}
		return hellos == 2
	})
	waitFor(t, "onConnected twice", func() bool { mu.Lock(); defer mu.Unlock(); return connections == 2 })
}

func TestSessionFallsBackToUnaryOnUnimplemented(t *testing.T) {
	f := newFakeSessionServer()
	f.unimplement = true
	client := startFakeOrchestrator(t, f)
	s := newSession(client, "key-1", "host-a", "v1", 60, nil, nil)
	s.backoffMin, s.backoffMax = 10*time.Millisecond, 20*time.Millisecond
	s.start()
	defer s.stop()

	waitFor(t, "fallback flag", s.unaryFallback)
	if err := s.send(&pb.ProxyMessage{}); err != errSessionDown {
		t.Errorf("send while down should report errSessionDown, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.attemptCount(); n != 1 {
		t.Errorf("fallback must stop reconnecting; saw %d Session() attempts", n)
	}
}

// TestSessionBackoffResetsAfterGoodConnection guards against the backoff
// growing forever once a session has been healthy: a session that reached
// welcome and later dropped should reconnect near backoffMin, not near
// whatever the backoff had grown to.
//
// To make the assertion actually discriminate the fix, the fake refuses the
// first 4 attempts outright so the client's backoff climbs 20, 40, 80, then
// 160ms before the 5th attempt is accepted. That leaves the in-loop backoff
// variable sitting well above backoffMin at the moment the session goes
// healthy. Only after that do we kill the stream and time the reconnect:
// with the reset it lands near backoffMin (~20ms); without it, it lands
// near the climbed value (~320ms) capped by backoffMax.
func TestSessionBackoffResetsAfterGoodConnection(t *testing.T) {
	f := newFakeSessionServer()
	f.refuseFirst = 4
	client := startFakeOrchestrator(t, f)
	s := newSession(client, "key-1", "host-a", "v1", 60, nil, nil)
	s.backoffMin, s.backoffMax = 20*time.Millisecond, 400*time.Millisecond
	s.start()
	defer s.stop()

	waitFor(t, "connected after climbing backoff", s.connected)

	kill := time.Now()
	f.kill <- struct{}{}
	waitFor(t, "disconnected", func() bool { return !s.connected() })
	waitFor(t, "reconnected", s.connected)
	if elapsed := time.Since(kill); elapsed > 150*time.Millisecond {
		t.Errorf("reconnect after a healthy session took %v; backoff should have reset near backoffMin", elapsed)
	}
}

func TestSessionDispatchesLockExpired(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	var got []string
	var mu sync.Mutex
	s := newSession(client, "key-1", "host-a", "v1", 60, func(id string) { mu.Lock(); got = append(got, id); mu.Unlock() }, nil)
	s.start()
	defer s.stop()
	waitFor(t, "connected", s.connected)

	f.push <- &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_LockExpired{LockExpired: &pb.LockExpired{LockId: "lock-9"}}}
	waitFor(t, "expired callback", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 && got[0] == "lock-9" })
}

func TestSessionRecvLoopIsNotBlockedByOnExpired(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	release := make(chan struct{})
	var got []string
	var mu sync.Mutex
	s := newSession(client, "key-1", "host-a", "v1", 60, func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
		if id == "slow" {
			<-release // a callback that blocks (e.g. CloseScopedPort waiting on a stuck send)
		}
	}, nil)
	s.start()
	defer s.stop()
	defer close(release)
	waitFor(t, "connected", s.connected)

	f.push <- &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_LockExpired{LockExpired: &pb.LockExpired{LockId: "slow"}}}
	f.push <- &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_LockExpired{LockExpired: &pb.LockExpired{LockId: "next"}}}
	// The second push must reach its callback while the first callback is still blocked.
	waitFor(t, "second expiry delivered despite the first callback blocking", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})
}

// stubEOFStream is a stub grpc.BidiStreamingClient[pb.ProxyMessage,
// pb.OrchestratorMessage] whose Send reports io.EOF, the way grpc-go does
// when the server already terminated the stream (e.g. a trailers-only
// Unimplemented) before Send ran: the real status is only available from
// Recv. Used to exercise serveOnce's EOF-then-Recv fallback deterministically,
// without racing a real server's trailers against a real client's Send.
type stubEOFStream struct {
	grpc.ClientStream
}

func (stubEOFStream) Send(*pb.ProxyMessage) error { return io.EOF }
func (stubEOFStream) Recv() (*pb.OrchestratorMessage, error) {
	return nil, status.Error(codes.Unimplemented, "x")
}

// stubEOFClient is a stub pb.OrchestratorClient whose Session returns
// stubEOFStream. serveOnce never calls the other OrchestratorClient methods,
// so they are left unimplemented (nil embedded interface).
type stubEOFClient struct {
	pb.OrchestratorClient
}

func (stubEOFClient) Session(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[pb.ProxyMessage, pb.OrchestratorMessage], error) {
	return stubEOFStream{}, nil
}

func TestSessionFallbackWhenHelloSendSeesEOF(t *testing.T) {
	s := newSession(stubEOFClient{}, "key-1", "host-a", "v1", 60, nil, nil)

	if _, err := s.serveOnce(context.Background()); status.Code(err) != codes.Unimplemented {
		t.Fatalf("serveOnce error = %v, want status code Unimplemented", err)
	}

	s.start()
	defer s.stop()
	waitFor(t, "fallback flag set after EOF-then-Recv", s.unaryFallback)
}

func TestSessionSendReturnsErrorWhenDown(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	s := newSession(client, "key-1", "host-a", "v1", 60, nil, nil)
	// not started
	if err := s.send(&pb.ProxyMessage{}); err != errSessionDown {
		t.Errorf("expected errSessionDown, got %v", err)
	}
	_ = client
}

func newRouterWithSession(t *testing.T, f *fakeSessionServer) *CommandRouter {
	t.Helper()
	client := startFakeOrchestrator(t, f)
	r := &CommandRouter{orchClient: client, apiKey: "key-1", proxyID: "host-a"}
	r.StartSession("v1", 60, nil, nil)
	t.Cleanup(r.sess.stop)
	if !f.unimplement {
		waitFor(t, "connected", r.sess.connected)
	} else {
		waitFor(t, "fallback", r.sess.unaryFallback)
	}
	return r
}

func TestRouterSendsReportsAndHeartbeatsOnStream(t *testing.T) {
	f := newFakeSessionServer()
	r := newRouterWithSession(t, f)

	if err := r.ReportDevices([]*pb.DeviceInfo{{Serial: "DEV1"}}); err != nil {
		t.Fatalf("ReportDevices: %v", err)
	}
	alive, unsent, err := r.LockHeartbeat("lock-1", nil)
	if err != nil || !alive || len(unsent) != 0 {
		t.Fatalf("LockHeartbeat on stream: alive=%v unsent=%v err=%v", alive, unsent, err)
	}
	if err := r.SendLockLog("lock-1", []*pb.LockLogEntry{{Service: "shell:ls"}}); err != nil {
		t.Fatalf("SendLockLog: %v", err)
	}
	waitFor(t, "three messages after hello", func() bool { msgs, _ := f.snapshot(); return len(msgs) == 4 })
	msgs, _ := f.snapshot()
	if msgs[1].GetDevices() == nil || msgs[1].GetDevices().Devices[0].Serial != "DEV1" {
		t.Errorf("devices not on stream: %+v", msgs[1])
	}
	if msgs[2].GetHeartbeat() == nil || msgs[2].GetHeartbeat().LockId != "lock-1" || len(msgs[2].GetHeartbeat().Log) != 0 {
		t.Errorf("heartbeat not on stream or carries a log: %+v", msgs[2])
	}
	if l := msgs[3].GetLockLog(); l == nil || l.LockId != "lock-1" || l.Entries[0].Service != "shell:ls" {
		t.Errorf("lock log not on stream: %+v", msgs[3])
	}
}

// TestRouterHeartbeatOnStreamHandsEntriesBack guards against the two-step
// send that could duplicate entries (ship a LockLog, then fail to report the
// heartbeat that followed it): LockHeartbeat on the stream must make exactly
// one send — the heartbeat, carrying no log — and hand the log argument
// straight back as unsent, leaving it to the caller to decide what happens
// to it.
func TestRouterHeartbeatOnStreamHandsEntriesBack(t *testing.T) {
	f := newFakeSessionServer()
	r := newRouterWithSession(t, f)

	in := []*pb.LockLogEntry{{Service: "shell:late"}}
	alive, unsent, err := r.LockHeartbeat("lock-1", in)
	if err != nil || !alive {
		t.Fatalf("LockHeartbeat with log: alive=%v err=%v", alive, err)
	}
	if len(unsent) != 1 || unsent[0] != in[0] {
		t.Errorf("stream heartbeat must hand the log back unsent unchanged, got %+v", unsent)
	}
	waitFor(t, "heartbeat on stream", func() bool { msgs, _ := f.snapshot(); return len(msgs) == 2 })
	msgs, _ := f.snapshot()
	if h := msgs[1].GetHeartbeat(); h == nil || h.LockId != "lock-1" || len(h.Log) != 0 {
		t.Errorf("heartbeat wrong or carries a log: %+v", msgs[1])
	}
	// No LockLog should ever be sent by LockHeartbeat on the stream.
	time.Sleep(30 * time.Millisecond)
	msgs, _ = f.snapshot()
	for _, msg := range msgs {
		if msg.GetLockLog() != nil {
			t.Errorf("unexpected LockLog on the stream from LockHeartbeat: %+v", msg)
		}
	}
}

// unaryRecorder is the fake used when the orchestrator predates Session: it
// answers the unary calls and refuses the stream.
type unaryRecorder struct {
	fakeSessionServer
	mu       sync.Mutex
	reports  int
	beats    int
	lastBeat *pb.LockHeartbeatRequest
}

func (u *unaryRecorder) ReportDevices(ctx context.Context, in *pb.ReportDevicesRequest) (*pb.ReportDevicesResponse, error) {
	u.mu.Lock()
	u.reports++
	u.mu.Unlock()
	return &pb.ReportDevicesResponse{}, nil
}

func (u *unaryRecorder) LockHeartbeat(ctx context.Context, in *pb.LockHeartbeatRequest) (*pb.LockHeartbeatResponse, error) {
	u.mu.Lock()
	u.beats++
	u.lastBeat = in
	u.mu.Unlock()
	return &pb.LockHeartbeatResponse{Alive: true}, nil
}

func (u *unaryRecorder) getLastBeat() *pb.LockHeartbeatRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastBeat
}

func TestRouterUsesUnaryInFallbackMode(t *testing.T) {
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
	r.StartSession("v1", 60, nil, nil)
	t.Cleanup(r.sess.stop)
	waitFor(t, "fallback", r.sess.unaryFallback)

	if err := r.ReportDevices(nil); err != nil {
		t.Fatalf("unary ReportDevices: %v", err)
	}
	if alive, unsent, err := r.LockHeartbeat("lock-1", nil); err != nil || !alive || len(unsent) != 0 {
		t.Fatalf("unary LockHeartbeat: alive=%v unsent=%v err=%v", alive, unsent, err)
	}
	if err := r.SendLockLog("lock-1", nil); err != errSessionDown {
		t.Errorf("SendLockLog in fallback should be errSessionDown (logs ride the unary heartbeat), got %v", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reports != 1 || u.beats != 1 {
		t.Errorf("unary calls not made: reports=%d beats=%d", u.reports, u.beats)
	}
}

func TestRouterDropsReportAndHoldsHeartbeatWhileReconnecting(t *testing.T) {
	f := newFakeSessionServer()
	r := newRouterWithSession(t, f)
	r.sess.backoffMin, r.sess.backoffMax = 200*time.Millisecond, 200*time.Millisecond
	f.kill <- struct{}{}
	waitFor(t, "stream down", func() bool { return !r.sess.connected() })

	if err := r.ReportDevices(nil); err != nil {
		t.Errorf("a report while reconnecting is dropped silently, got %v", err)
	}
	if _, unsent, err := r.LockHeartbeat("lock-1", nil); err != errSessionDown || len(unsent) != 0 {
		t.Errorf("a heartbeat while reconnecting must say the session is down, got unsent=%v err=%v", unsent, err)
	}
}

func TestSessionHelloCarriesConfiguredHeartbeatInterval(t *testing.T) {
	f := newFakeSessionServer()
	client := startFakeOrchestrator(t, f)
	r := &CommandRouter{orchClient: client, apiKey: "key-1", proxyID: "host-a"}
	r.StartSession("v1", 45, nil, nil)
	t.Cleanup(r.sess.stop)
	waitFor(t, "hello", func() bool { msgs, _ := f.snapshot(); return len(msgs) >= 1 })
	msgs, _ := f.snapshot()
	if got := msgs[0].GetHello().GetLockHeartbeatIntervalS(); got != 45 {
		t.Errorf("hello heartbeat interval = %d, want 45", got)
	}
}

func TestDeviceTrackerReportNowSendsCurrentList(t *testing.T) {
	f := newFakeSessionServer()
	r := newRouterWithSession(t, f)
	tr := NewDeviceListTracker(r)
	tr.UpdateDevices("10.0.0.5:8080", []*pb.DeviceInfo{{Serial: "DEV7"}})
	tr.ReportNow()
	waitFor(t, "devices on stream", func() bool {
		msgs, _ := f.snapshot()
		for _, m := range msgs {
			if d := m.GetDevices(); d != nil && len(d.Devices) == 1 && d.Devices[0].Serial == "DEV7" {
				return true
			}
		}
		return false
	})
}
