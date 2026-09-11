package proxy

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// errSessionDown is returned by send when no stream is currently open. Callers
// that must not lose data requeue and try again on the next tick.
var errSessionDown = errors.New("orchestrator session is down")

// session keeps one Session stream open to the orchestrator for the life of
// the proxy: hello first, reconnect with backoff on any error, and a permanent
// switch to unary mode if the orchestrator does not implement Session.
type session struct {
	client             pb.OrchestratorClient
	apiKey             string
	proxyID            string
	version            string
	heartbeatIntervalS int64               // reported in hello; informational for the orchestrator
	onExpired          func(lockID string) // orchestrator says a lock is gone
	onConnected        func()              // a (re)connect completed the hello; resend state

	backoffMin, backoffMax time.Duration

	mu       sync.Mutex
	stream   grpc.BidiStreamingClient[pb.ProxyMessage, pb.OrchestratorMessage]
	fallback bool
	cancel   context.CancelFunc
	done     chan struct{}

	// sendMu serialises calls to stream.Send, independent of mu. A blocked
	// Send (e.g. flow-control window exhausted) must never hold mu, or
	// stop()/connected()/unaryFallback() would stall behind it.
	sendMu sync.Mutex
}

func newSession(client pb.OrchestratorClient, apiKey, proxyID, version string, heartbeatIntervalS int64, onExpired func(string), onConnected func()) *session {
	return &session{
		client: client, apiKey: apiKey, proxyID: proxyID, version: version, heartbeatIntervalS: heartbeatIntervalS,
		onExpired: onExpired, onConnected: onConnected,
		backoffMin: time.Second, backoffMax: 30 * time.Second,
	}
}

func (s *session) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()
	go s.run(ctx)
}

func (s *session) stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *session) connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream != nil
}

func (s *session) unaryFallback() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fallback
}

// send writes one message on the current stream. sendMu serialises the
// actual Send call, because gRPC streams allow only one concurrent Send; mu
// is only held long enough to snapshot the stream, so a Send blocked on
// flow control never blocks stop(), connected() or unaryFallback().
func (s *session) send(msg *pb.ProxyMessage) error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return errSessionDown
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return stream.Send(msg)
}

func (s *session) run(ctx context.Context) {
	defer close(s.done)
	backoff := s.backoffMin
	for {
		connected, err := s.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if status.Code(err) == codes.Unimplemented {
			s.mu.Lock()
			s.fallback = true
			s.mu.Unlock()
			slog.Warn("orchestrator has no Session stream; using unary heartbeats and reports", "error", err)
			return
		}
		// A session that reached welcome was healthy; reconnect promptly
		// instead of paying whatever backoff had grown to before it dropped.
		if connected {
			backoff = s.backoffMin
		}
		slog.Warn("orchestrator session ended; reconnecting", "error", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > s.backoffMax {
			backoff = s.backoffMax
		}
	}
}

// serveOnce opens a stream, sends hello, waits for welcome, then reads until
// the stream fails. Returns whether it ever reached welcome (so run can
// reset its backoff) and the error that ended it.
func (s *session) serveOnce(ctx context.Context) (connected bool, err error) {
	md := metadata.Pairs("x-api-key", s.apiKey)
	stream, err := s.client.Session(metadata.NewOutgoingContext(ctx, md))
	if err != nil {
		return false, err
	}
	hello := &pb.ProxyMessage{Msg: &pb.ProxyMessage_Hello{Hello: &pb.ProxyHello{
		Version: s.version, ProxyId: s.proxyID, DeviceReportIntervalS: 10, LockHeartbeatIntervalS: s.heartbeatIntervalS,
	}}}
	if err := stream.Send(hello); err != nil {
		return false, err
	}
	first, err := stream.Recv()
	if err != nil {
		return false, err
	}
	w := first.GetWelcome()
	if w == nil {
		return false, errors.New("orchestrator did not answer hello with welcome")
	}
	slog.Info("orchestrator session open", "tenant", w.TenantId, "orchestrator", w.Version, "proxyId", s.proxyID)

	s.mu.Lock()
	s.stream = stream
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stream = nil
		s.mu.Unlock()
	}()
	if s.onConnected != nil {
		s.onConnected()
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return true, err
		}
		if e := msg.GetLockExpired(); e != nil && s.onExpired != nil {
			s.onExpired(e.LockId)
		}
	}
}
