package proxy

import (
	"context"
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
	unimplement bool
	push        chan *pb.OrchestratorMessage
	kill        chan struct{}
}

func newFakeSessionServer() *fakeSessionServer {
	return &fakeSessionServer{push: make(chan *pb.OrchestratorMessage, 8), kill: make(chan struct{}, 1)}
}

func (f *fakeSessionServer) Session(stream grpc.BidiStreamingServer[pb.ProxyMessage, pb.OrchestratorMessage]) error {
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
	if _, n := f.snapshot(); n != 0 {
		t.Errorf("fallback must stop reconnecting; saw %d accepted streams", n)
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
