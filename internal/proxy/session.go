package proxy

import (
	"context"
	"errors"
	"io"
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
	heartbeatIntervalS int64                 // reported in hello; informational for the orchestrator
	onExpired          func(lockID string)   // orchestrator says a lock is gone
	onConnected        func()                // a (re)connect completed the hello; resend state
	onRelayOpen        func(o *pb.RelayOpen) // orchestrator wants this proxy to serve a relayed connection

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

func newSession(client pb.OrchestratorClient, apiKey, proxyID, version string, heartbeatIntervalS int64, onExpired func(string), onConnected func(), onRelayOpen func(*pb.RelayOpen)) *session {
	return &session{
		client: client, apiKey: apiKey, proxyID: proxyID, version: version, heartbeatIntervalS: heartbeatIntervalS,
		onExpired: onExpired, onConnected: onConnected, onRelayOpen: onRelayOpen,
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
	err := stream.Send(msg)
	s.sendMu.Unlock()
	if err == nil {
		return nil
	}
	if isStreamDead(err) {
		// The transport closed under us: the Recv loop will see it and reconnect.
		// Drop the stream now so callers stop hitting it, and report "down" so they
		// requeue quietly rather than warn once per reconnect.
		s.mu.Lock()
		if s.stream == stream {
			s.stream = nil
		}
		s.mu.Unlock()
		return errSessionDown
	}
	return err
}

// sendRelayFailed tells the orchestrator this proxy could not open a device for
// a tunnel, so the holder's ADB client gets a FAIL instead of a hang. Best effort.
func (s *session) sendRelayFailed(tunnelID, msg string) {
	if err := s.send(&pb.ProxyMessage{Msg: &pb.ProxyMessage_RelayFailed{RelayFailed: &pb.RelayFailed{TunnelId: tunnelID, Error: msg}}}); err != nil {
		slog.Debug("relay_failed not sent", "tunnelId", tunnelID, "error", err)
	}
}

// isStreamDead reports whether a Send error means the stream itself is gone
// (as opposed to a problem with this one message). grpc-go returns io.EOF from
// Send once the stream has terminated and the real status is only on Recv.
func isStreamDead(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded, codes.Aborted:
		return true
	}
	return false
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
	// Per-attempt context: if this attempt returns before the stream is
	// handed off to run() (e.g. no welcome), cancel aborts it here instead of
	// leaving it open on the long-lived ctx for the rest of the process.
	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	md := metadata.Pairs("x-api-key", s.apiKey)
	stream, err := s.client.Session(metadata.NewOutgoingContext(actx, md))
	if err != nil {
		return false, err
	}
	hello := &pb.ProxyMessage{Msg: &pb.ProxyMessage_Hello{Hello: &pb.ProxyHello{
		Version: s.version, ProxyId: s.proxyID, DeviceReportIntervalS: 10, LockHeartbeatIntervalS: s.heartbeatIntervalS,
	}}}
	if err := stream.Send(hello); err != nil {
		// grpc-go's SendMsg contract: if the server already terminated the
		// stream (e.g. a trailers-only Unimplemented), Send may return io.EOF
		// before the real status is available — only Recv surfaces it. Fall
		// through to Recv so the status code (and the Unimplemented fallback
		// decision in run()) isn't lost to a race on which side observes the
		// termination first.
		if errors.Is(err, io.EOF) {
			if _, recvErr := stream.Recv(); recvErr != nil {
				return false, recvErr
			}
		}
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
			// Off the Recv loop: closing a scoped port waits for its keepalive
			// goroutine, which may itself be inside a stalled Send.
			go s.onExpired(e.LockId)
		}
		if o := msg.GetRelayOpen(); o != nil && s.onRelayOpen != nil {
			go s.onRelayOpen(o) // serving a relay blocks for the connection's lifetime
		}
	}
}
