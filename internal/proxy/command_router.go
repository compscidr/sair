package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// LockResult holds the result of a lock acquisition.
type LockResult struct {
	LockID  string
	Serials map[string]struct{}
	// RemoteDevices are granted devices that live on another proxy (or
	// tenant), reached through the relay, with the info their owner
	// reported. Keyed by serial; nil for none.
	RemoteDevices map[string]*pb.DeviceInfo
}

// CommandRouter is a gRPC client that routes:
//   - ADB commands to device-sources (connections created lazily from reported source addresses)
//   - Lock management to the remote orchestrator
type CommandRouter struct {
	orchConn   *grpc.ClientConn
	orchClient pb.OrchestratorClient
	apiKey     string
	proxyID    string
	sess       *session

	// beforeSendLockLog, when set, runs at the top of SendLockLog. Test-only
	// hook used to pin a send mid-flight and exercise races deterministically.
	beforeSendLockLog func()

	// dsDialer overrides how device-source connections are dialed (tests use bufconn).
	dsDialer func(ctx context.Context, addr string) (net.Conn, error)
	// ResolveSource maps a local serial to its device-source address ("" if unknown).
	// Set by main to the device tracker's GetSourceAddr; exported for that wiring.
	ResolveSource func(serial string) string

	// Device-source connections, created lazily when sources register
	dsMu    sync.Mutex
	dsConns map[string]*grpc.ClientConn          // sourceAddr -> conn
	dsClients map[string]dspb.DeviceSourceClient  // sourceAddr -> client
}

func NewCommandRouter(orchestratorAddr, apiKey string, orchestratorTLS bool) (*CommandRouter, error) {
	// Orchestrator connection (remote, may use TLS)
	var orchOpts []grpc.DialOption
	if orchestratorTLS {
		orchOpts = append(orchOpts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	} else {
		orchOpts = append(orchOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	orchOpts = append(orchOpts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                30 * time.Second,
		Timeout:             10 * time.Second,
		PermitWithoutStream: true,
	}))

	orchConn, err := grpc.NewClient(orchestratorAddr, orchOpts...)
	if err != nil {
		return nil, err
	}

	return &CommandRouter{
		orchConn:   orchConn,
		orchClient: pb.NewOrchestratorClient(orchConn),
		apiKey:     apiKey,
		proxyID:    ProxyID(),
		dsConns:    make(map[string]*grpc.ClientConn),
		dsClients:  make(map[string]dspb.DeviceSourceClient),
	}, nil
}

// getOrCreateDSClient returns a device-source gRPC client for the given address,
// creating a connection lazily if needed.
func (r *CommandRouter) getOrCreateDSClient(sourceAddr string) (dspb.DeviceSourceClient, error) {
	r.dsMu.Lock()
	defer r.dsMu.Unlock()

	if client, ok := r.dsClients[sourceAddr]; ok {
		return client, nil
	}

	slog.Info("creating gRPC connection to device-source", "addr", sourceAddr)
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(64*1024*1024),
			grpc.MaxCallSendMsgSize(64*1024*1024),
		),
		grpc.WithInitialWindowSize(16*1024*1024),
		grpc.WithInitialConnWindowSize(16*1024*1024),
	}
	if r.dsDialer != nil {
		opts = append(opts, grpc.WithContextDialer(r.dsDialer))
	}
	conn, err := grpc.NewClient(sourceAddr, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to device-source %s: %w", sourceAddr, err)
	}

	if r.dsConns == nil {
		r.dsConns = make(map[string]*grpc.ClientConn)
	}
	if r.dsClients == nil {
		r.dsClients = make(map[string]dspb.DeviceSourceClient)
	}

	client := dspb.NewDeviceSourceClient(conn)
	r.dsConns[sourceAddr] = conn
	r.dsClients[sourceAddr] = client
	return client, nil
}

// RemoveDSClient closes and removes a device-source connection (called when a source goes stale).
func (r *CommandRouter) RemoveDSClient(sourceAddr string) {
	r.dsMu.Lock()
	defer r.dsMu.Unlock()

	if conn, ok := r.dsConns[sourceAddr]; ok {
		conn.Close()
		delete(r.dsConns, sourceAddr)
		delete(r.dsClients, sourceAddr)
		slog.Info("removed gRPC connection to device-source", "addr", sourceAddr)
	}
}

func (r *CommandRouter) ctx() context.Context {
	md := metadata.Pairs("x-api-key", r.apiKey)
	return metadata.NewOutgoingContext(context.Background(), md)
}

func (r *CommandRouter) ctxWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	md := metadata.Pairs("x-api-key", r.apiKey)
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	return context.WithTimeout(ctx, d)
}

// ForwardToDevice relays raw bytes between a TCP connection and a device-source's ForwardToDevice gRPC stream.
// The sourceAddr is the gRPC address of the device-source that owns this serial.
func (r *CommandRouter) ForwardToDevice(sourceAddr, serial, command string, conn net.Conn) error {
	client, err := r.getOrCreateDSClient(sourceAddr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.ForwardToDevice(ctx)
	if err != nil {
		return err
	}

	// Send setup message
	err = stream.Send(&dspb.ForwardData{
		Payload: &dspb.ForwardData_Setup{
			Setup: &dspb.ForwardSetup{
				Serial:         serial,
				InitialCommand: command,
			},
		},
	})
	if err != nil {
		return err
	}

	done := make(chan error, 1)

	// TCP → gRPC (request direction — half-close aware)
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				stream.CloseSend()
				if err != io.EOF {
					// Real error — cancel to unblock response direction
					cancel()
				}
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&dspb.ForwardData{
				Payload: &dspb.ForwardData_Data{Data: data},
			}); err != nil {
				cancel()
				return
			}
		}
	}()

	// gRPC → TCP (response direction — drives teardown)
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if data := resp.GetData(); data != nil {
				if _, err := conn.Write(data); err != nil {
					done <- err
					return
				}
			}
		}
	}()

	// Wait for the response direction to finish, then tear down.
	err = <-done
	cancel()
	conn.SetReadDeadline(time.Now())

	if err == io.EOF {
		return nil
	}
	return err
}

// Orchestrator operations — lock management only

func (r *CommandRouter) ListDevices() (*pb.DeviceList, error) {
	return r.orchClient.ListDevices(r.ctx(), &pb.ListDevicesRequest{})
}

// ReportDevices sends the full device list: on the session when it is open,
// unary when the orchestrator predates sessions, and silently dropped while a
// reconnect is in progress (the reconnect resends the list).
func (r *CommandRouter) ReportDevices(devices []*pb.DeviceInfo) error {
	if r.sessionUp() {
		return r.sess.send(&pb.ProxyMessage{Msg: &pb.ProxyMessage_Devices{Devices: &pb.ReportDevicesRequest{Devices: devices}}})
	}
	if r.sess != nil && !r.sess.unaryFallback() {
		slog.Debug("session reconnecting; device report skipped")
		return nil
	}
	ctx, cancel := r.ctxWithTimeout(10 * time.Second)
	defer cancel()
	_, err := r.orchClient.ReportDevices(ctx, &pb.ReportDevicesRequest{Devices: devices})
	return err
}

// AcquireLock asks the orchestrator for a lock. Pass serials to request
// specific devices, or count to request that many arbitrary free devices.
// Passing neither locks every device in the tenant's pool.
func (r *CommandRouter) AcquireLock(serials map[string]struct{}, count int32, deadlineMinutes int64, repo, runURL string) (*LockResult, error) {
	if count < 0 {
		return nil, fmt.Errorf("count must not be negative: %d", count)
	}
	if count > 0 && len(serials) > 0 {
		return nil, fmt.Errorf("count and serials are mutually exclusive")
	}
	if deadlineMinutes <= 0 {
		deadlineMinutes = 30
	}
	ctx, cancel := r.ctxWithTimeout(time.Duration(deadlineMinutes) * time.Minute)
	defer cancel()

	req := &pb.AcquireLockRequest{Repo: repo, Count: count, RunUrl: runURL, ProxyId: r.proxyID}
	for s := range serials {
		req.Serials = append(req.Serials, s)
	}
	resp, err := r.orchClient.AcquireLock(ctx, req)
	if err != nil {
		return nil, err
	}

	resultSerials := make(map[string]struct{})
	for _, s := range resp.Serials {
		resultSerials[s] = struct{}{}
	}
	var remote map[string]*pb.DeviceInfo
	if len(resp.RemoteDevices) > 0 {
		remote = make(map[string]*pb.DeviceInfo, len(resp.RemoteDevices))
		for _, d := range resp.RemoteDevices {
			remote[d.Serial] = d
		}
	}
	return &LockResult{LockID: resp.LockId, Serials: resultSerials, RemoteDevices: remote}, nil
}

// ReleaseLock releases a lock, reporting the job outcome so the orchestrator
// can record it: success, failure or cancelled from the client ("" when it did
// not say), or error/cancelled when the proxy releases on its own behalf.
func (r *CommandRouter) ReleaseLock(lockID, status string, log []*pb.LockLogEntry) (bool, error) {
	ctx, cancel := r.ctxWithTimeout(30 * time.Second)
	defer cancel()
	resp, err := r.orchClient.ReleaseLock(ctx, &pb.ReleaseLockRequest{LockId: lockID, Status: status, Log: log})
	if err != nil {
		return false, err
	}
	return resp.Released, nil
}

// LockHeartbeat keeps the lock alive with exactly one send per call — no
// two-step path that could ship a log and then fail to report it (or vice
// versa report success without the heartbeat landing). On the stream it
// sends only the heartbeat and never touches log: unsent is handed back
// unchanged (whether the send succeeded or not), leaving it entirely to the
// caller to requeue or ship on the next flush. Unary carries log atomically
// in the same request as the heartbeat: unsent is nil on success (delivered)
// or log unchanged on failure. Returns errSessionDown while a reconnect is
// in progress, with unsent == log.
func (r *CommandRouter) LockHeartbeat(lockID string, log []*pb.LockLogEntry) (alive bool, unsent []*pb.LockLogEntry, err error) {
	if r.sessionUp() {
		err := r.sess.send(&pb.ProxyMessage{Msg: &pb.ProxyMessage_Heartbeat{Heartbeat: &pb.LockHeartbeatRequest{LockId: lockID}}})
		return err == nil, log, err
	}
	if r.sess != nil && !r.sess.unaryFallback() {
		return false, log, errSessionDown
	}
	ctx, cancel := r.ctxWithTimeout(30 * time.Second)
	defer cancel()
	resp, err := r.orchClient.LockHeartbeat(ctx, &pb.LockHeartbeatRequest{LockId: lockID, Log: log})
	if err != nil {
		return false, log, err
	}
	return resp.Alive, nil, nil
}

// SendLockLog ships log entries on the session. errSessionDown when there is
// no stream, including unary fallback, where entries ride the heartbeat instead.
func (r *CommandRouter) SendLockLog(lockID string, entries []*pb.LockLogEntry) error {
	if r.beforeSendLockLog != nil {
		r.beforeSendLockLog()
	}
	if !r.sessionUp() {
		return errSessionDown
	}
	return r.sess.send(&pb.ProxyMessage{Msg: &pb.ProxyMessage_LockLog{LockLog: &pb.LockLog{LockId: lockID, Entries: entries}}})
}

// StartSession opens the long-lived stream to the orchestrator. onExpired is
// called when the orchestrator reports a lock gone; onConnected after every
// (re)connect so the caller can resend the device list.
func (r *CommandRouter) StartSession(version string, heartbeatIntervalS int64, onExpired func(lockID string), onConnected func()) {
	r.sess = newSession(r.orchClient, r.apiKey, r.proxyID, version, heartbeatIntervalS, onExpired, onConnected)
	r.sess.start()
}

// sessionUp reports whether periodic traffic should go on the stream.
func (r *CommandRouter) sessionUp() bool {
	return r.sess != nil && r.sess.connected()
}

func (r *CommandRouter) Shutdown() {
	if r.sess != nil {
		r.sess.stop()
	}
	if err := r.orchConn.Close(); err != nil {
		slog.Error("failed to close orchestrator connection", "error", err)
	}
	r.dsMu.Lock()
	for addr, conn := range r.dsConns {
		if err := conn.Close(); err != nil {
			slog.Error("failed to close device-source connection", "addr", addr, "error", err)
		}
	}
	r.dsMu.Unlock()
}
