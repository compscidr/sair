package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/compscidr/sair/internal/updater"
	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
	// waitReset blocks while a released lock's reset still runs on serial.
	// Set by NewScopedPortManager, which owns the per-serial lock.
	waitReset func(serial string)
	// relayLocks maps a serial to the *relayRecord of the latest lock ServeRelay
	// relayed it under, for the phone's reset when that lock ends; an earlier
	// lock's end then leaves it alone, the phone is the next job's.
	// ponytail: an older orchestrator never pushes the owner LockExpired, so an
	// entry can outlive its lock; the ceiling is one per serial ever relayed.
	relayLocks sync.Map

	// events queues bench events for the session (see Event); nil until
	// StartSession, which drops them.
	events chan *pb.BenchEvent

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

// SetWifi sets a device's Wi-Fi through the device source it is attached to.
// band is WifiRequest.band. NotFound for a device this proxy has no source
// for (e.g. a relayed one).
func (r *CommandRouter) SetWifi(serial string, state dspb.WifiRequest_State, band string) error {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return err
	}
	// The ON wait is bounded by the device source (60s by default); this only
	// guards against a source that never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err = client.SetWifi(ctx, &dspb.WifiRequest{Serial: serial, State: state, Band: band})
	return err
}

// AddReverseForward runs `adb reverse tcp:<port> tcp:<port>` on a local device
// through its device source: the phone's 127.0.0.1:<port> then reaches
// <port> on the device source's host. NotFound for a device this proxy has
// no source for (e.g. a relayed one).
func (r *CommandRouter) AddReverseForward(serial string, port int) error {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, server := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(30 * time.Second))
	go func() {
		r.ForwardToDevice(addr, serial, fmt.Sprintf("reverse:forward:tcp:%d;tcp:%d", port, port), server)
		server.Close()
	}()
	reply, err := io.ReadAll(client)
	if err != nil {
		return err
	}
	// The adb server's OKAY for the stream, then adbd's OKAY for the forward.
	if !strings.HasPrefix(string(reply), "OKAYOKAY") {
		return fmt.Errorf("reverse tcp:%d on %s: %q", port, serial, reply)
	}
	return nil
}

// ResetPackages uninstalls what a job installed on a local device, through its
// device source, and returns the packages and the Wi-Fi suggestions that went
// with them. It keeps since if given, else the device's baseline. NotFound for a device this proxy has no source for; Unimplemented
// from a device source that predates it.
func (r *CommandRouter) ResetPackages(serial string, since *dspb.PackageSnapshot) (removed, suggestions []string, err error) {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return nil, nil, status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := client.ResetPackages(ctx, &dspb.ResetPackagesRequest{Serial: serial, Since: since})
	if err != nil {
		return nil, nil, err
	}
	return resp.Removed, resp.SuggestionsRemoved, nil
}

// Wake wakes a local device and dismisses its keyguard, verified, through
// its device source (sair#92). NotFound for a device this proxy has no
// source for; Unimplemented from a device source that predates it.
func (r *CommandRouter) Wake(serial string) error {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err = client.Wake(ctx, &dspb.WakeRequest{Serial: serial})
	return err
}

// ResetBluetooth cycles a local device's Bluetooth through its device
// source (sair#127). NotFound for a device this proxy has no source for.
func (r *CommandRouter) ResetBluetooth(serial string) error {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return err
	}
	// Two 30s waits on the device source; this guards against one that never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err = client.ResetBluetooth(ctx, &dspb.ResetBluetoothRequest{Serial: serial})
	return err
}

// HoldUpdates disables (hold) or re-enables a local device's Play Store
// through its device source (sair#151). NotFound for a device this proxy
// has no source for.
func (r *CommandRouter) HoldUpdates(serial string, hold bool) error {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = client.HoldUpdates(ctx, &dspb.HoldUpdatesRequest{Serial: serial, Hold: hold})
	return err
}

// RecentDeviceSourceLog returns the device source's recent log lines about
// a local device, for a failure's lock log excerpt.
func (r *CommandRouter) RecentDeviceSourceLog(serial string) ([]string, error) {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return nil, status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.RecentLog(ctx, &dspb.RecentLogRequest{Serial: serial})
	if err != nil {
		return nil, err
	}
	return resp.Lines, nil
}

// RebootIfStale reboots a local device whose uptime is over the device
// source's limit and returns once it is back (sair#93). NotFound for a device
// this proxy has no source for; Unimplemented from an older device source.
func (r *CommandRouter) RebootIfStale(serial string) (rebooted bool, uptime time.Duration, err error) {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return false, 0, status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return false, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := client.RebootIfStale(ctx, &dspb.RebootRequest{Serial: serial})
	if err != nil {
		return false, 0, err
	}
	return resp.Rebooted, time.Duration(resp.UptimeSeconds) * time.Second, nil
}

// RebootIfWedged reboots a local device whose radio ("wifi" or "bluetooth")
// reset just failed if that radio's service no longer answers, and returns
// once it is back (sair#139). NotFound for a device this proxy has no source
// for; Unimplemented from an older device source.
func (r *CommandRouter) RebootIfWedged(serial, radio string) (rebooted bool, err error) {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return false, status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := client.RebootIfWedged(ctx, &dspb.WedgedRadioRequest{Serial: serial, Radio: radio})
	if err != nil {
		return false, err
	}
	return resp.Rebooted, nil
}

// ClearUiAutomation clears a UiAutomation registration a dead session leaked
// on a local device (sair#128) and says how: "" when there was none, "killed
// ..." or "rebooted". NotFound for a device this proxy has no source for;
// Unimplemented from an older device source.
func (r *CommandRouter) ClearUiAutomation(serial string) (string, error) {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return "", status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, err := r.getOrCreateDSClient(addr)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := client.ClearUiAutomation(ctx, &dspb.UiAutomationRequest{Serial: serial})
	if err != nil {
		return "", err
	}
	return resp.Cleared, nil
}

// RemoveReverseForwards runs `adb reverse --remove-all` on a local device, so
// a job that died before its own cleanup leaves no tunnel for the next one.
// NotFound for a device this proxy has no source for (e.g. a relayed one).
func (r *CommandRouter) RemoveReverseForwards(serial string) error {
	addr := ""
	if r.ResolveSource != nil {
		addr = r.ResolveSource(serial)
	}
	if addr == "" {
		return status.Errorf(codes.NotFound, "device %s is not attached to this proxy", serial)
	}
	client, server := net.Pipe()
	defer client.Close()
	// ponytail: a device that never answers leaks the stream goroutine; give
	// ForwardToDevice a context if that ever shows up.
	client.SetDeadline(time.Now().Add(30 * time.Second))
	go func() {
		r.ForwardToDevice(addr, serial, "reverse:killforward-all", server)
		server.Close()
	}()
	reply, err := io.ReadAll(client)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(reply), "OKAY") {
		return fmt.Errorf("reverse --remove-all on %s: %q", serial, reply)
	}
	return nil
}

// wifiUnmanaged reports whether a SetWifi error means the bench does not
// manage Wi-Fi (no WIFI_SSID, or a device source that predates SetWifi).
func wifiUnmanaged(err error) bool {
	c := status.Code(err)
	return c == codes.FailedPrecondition || c == codes.Unimplemented
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

// How often, and how far apart, AcquireLock re-asks after a connection reset:
// about half a minute, enough for a reconnect or an orchestrator restart.
var (
	acquireRetries    = 10
	acquireRetryDelay = 3 * time.Second
)

// AcquireLock asks the orchestrator for a lock. Pass serials to request
// specific devices, or count to request that many arbitrary free devices.
// Passing neither locks every device in the tenant's pool. priority orders
// the wait when the devices are busy: higher first, FIFO within a value.
// sameProxy asks for every device from a single proxy. tenantKey, when set,
// is a guest key the lock is attributed to; empty means the owner's own lock.
// clientIP, when set, is the caller's address as the proxy sees it; the
// orchestrator records it as where the key was last used from
// (sair-orchestrator#705). Older orchestrators ignore it. job carries what
// the lock is for (repo, run URL, CI job, labels) and may be nil; its other
// fields are ignored.
//
// Cancelling ctx (the runner hung up while queued) cancels the call, and the
// orchestrator drops the waiter instead of granting it a lock (sair#133).
func (r *CommandRouter) AcquireLock(parent context.Context, serials map[string]struct{}, count int32, deadlineMinutes int64, job *pb.AcquireLockRequest, priority int32, sameProxy bool, tenantKey, clientIP string) (*LockResult, error) {
	if count < 0 {
		return nil, fmt.Errorf("count must not be negative: %d", count)
	}
	if count > 0 && len(serials) > 0 {
		return nil, fmt.Errorf("count and serials are mutually exclusive")
	}
	if deadlineMinutes <= 0 {
		deadlineMinutes = 30
	}
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(parent, metadata.Pairs("x-api-key", r.apiKey)), time.Duration(deadlineMinutes)*time.Minute)
	defer cancel()
	if clientIP != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-client-ip", clientIP)
	}

	req := &pb.AcquireLockRequest{
		Repo: job.GetRepo(), RunUrl: job.GetRunUrl(), Job: job.GetJob(), RunAttempt: job.GetRunAttempt(),
		Sha: job.GetSha(), Branch: job.GetBranch(), Workflow: job.GetWorkflow(), Stage: job.GetStage(), Labels: job.GetLabels(),
		Count: count, ProxyId: r.proxyID, Priority: priority, SameProxy: sameProxy, TenantApiKey: tenantKey,
	}
	for s := range serials {
		req.Serials = append(req.Serials, s)
	}
	// A reset of the orchestrator connection ends a queued acquire with
	// Unavailable although nothing is wrong with the devices (#658 in
	// sair-orchestrator): ask again once the connection is back. The retry
	// rejoins the queue at the back of its priority.
	// ponytail: a reset after the grant but before the reply leaves that lock
	// orphaned until the orchestrator's 5 min inactivity reaper frees it.
	var resp *pb.AcquireLockResponse
	var err error
	for attempt := 0; ; attempt++ {
		resp, err = r.orchClient.AcquireLock(ctx, req)
		if status.Code(err) != codes.Unavailable || attempt == acquireRetries {
			break
		}
		slog.Warn("lock request lost its orchestrator connection; retrying", "attempt", attempt+1, "error", err)
		select {
		case <-time.After(acquireRetryDelay):
		case <-ctx.Done():
			return nil, err
		}
	}
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

// CheckKey asks the orchestrator whether key belongs to this proxy's own
// account (sair#120) or to the account of one of guestKeys (sair#131).
// Unimplemented from an orchestrator that predates it is returned as is, so
// the caller can fall back to its local lists.
func (r *CommandRouter) CheckKey(key string, guestKeys []string) (bool, error) {
	ctx, cancel := r.ctxWithTimeout(10 * time.Second)
	defer cancel()
	resp, err := r.orchClient.CheckKey(ctx, &pb.CheckKeyRequest{ApiKey: key, GuestApiKeys: guestKeys})
	if err != nil {
		return false, err
	}
	return resp.SameTenant, nil
}

// AddLockLabels labels a held lock or its devices. Unimplemented from an
// orchestrator that predates it is returned as is.
func (r *CommandRouter) AddLockLabels(lockID string, labels []*pb.Label) error {
	ctx, cancel := r.ctxWithTimeout(10 * time.Second)
	defer cancel()
	_, err := r.orchClient.AddLockLabels(ctx, &pb.AddLockLabelsRequest{LockId: lockID, Labels: labels})
	return err
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
// (re)connect so the caller can resend the device list; onRelayOpen when the
// orchestrator wants this proxy to serve a relayed connection.
func (r *CommandRouter) StartSession(version string, heartbeatIntervalS int64, onExpired func(lockID string), onConnected func(), onRelayOpen func(*pb.RelayOpen)) {
	r.sess = newSession(r.orchClient, r.apiKey, r.proxyID, version, heartbeatIntervalS, onExpired, onConnected, onRelayOpen)
	r.events = make(chan *pb.BenchEvent, maxQueuedEvents)
	go r.sendEvents()
	r.sess.start()
	start := &pb.BenchEvent{Kind: pb.BenchEventKind_BENCH_EVENT_KIND_PROXY_START, Outcome: "ok", Detail: version}
	if updater.Updated {
		start.Outcome = "updated"
	}
	r.Event(start)
}

// maxQueuedEvents bounds the bench events kept while the session is down.
const maxQueuedEvents = 256

// Event queues a bench event for the orchestrator (sair#97). Never blocks:
// past maxQueuedEvents, or before StartSession, the event is dropped.
func (r *CommandRouter) Event(e *pb.BenchEvent) {
	if r == nil {
		return
	}
	if e.TimestampMs == 0 {
		e.TimestampMs = time.Now().UnixMilli()
	}
	select {
	case r.events <- e:
	default:
		slog.Debug("bench event dropped", "kind", e.Kind, "serial", e.Serial)
	}
}

// sendEvents ships queued events in order, waiting out a session that is
// down. Against an orchestrator without Session there is nowhere to send
// them; they are dropped.
func (r *CommandRouter) sendEvents() {
	for e := range r.events {
		for !r.sess.unaryFallback() {
			err := r.sess.send(&pb.ProxyMessage{Msg: &pb.ProxyMessage_Event{Event: e}})
			if err == nil {
				break
			}
			if !errors.Is(err, errSessionDown) {
				slog.Debug("bench event not sent", "kind", e.Kind, "error", err)
				break
			}
			time.Sleep(time.Second)
		}
	}
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
