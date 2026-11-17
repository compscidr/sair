package proxy

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeOrchClient records the last AcquireLock request and returns a canned
// response. The embedded interface leaves the unused RPCs unimplemented.
type fakeOrchClient struct {
	pb.OrchestratorClient
	lastAcquire *pb.AcquireLockRequest
	lastRelease *pb.ReleaseLockRequest
	releaseErr  error // ReleaseLock's answer when set
	lastLabels  *pb.AddLockLabelsRequest
	resp        *pb.AcquireLockResponse
	err         error
	unavailable int // AcquireLock calls that fail with Unavailable before resp/err
	acquires    int
	clientIP    []string // x-client-ip metadata on the last AcquireLock
	queue       bool     // AcquireLock waits, like a queued request, until ctx ends
}

// CheckKey answers true for "sibling-key", as an orchestrator would for a
// second key on the proxy's own account, and for "guest-sibling" when
// "guest-a" is among the guest keys, as for a second key on a guest's account.
func (f *fakeOrchClient) CheckKey(ctx context.Context, in *pb.CheckKeyRequest, opts ...grpc.CallOption) (*pb.CheckKeyResponse, error) {
	same := in.ApiKey == "sibling-key" || in.ApiKey == "guest-sibling" && slices.Contains(in.GuestApiKeys, "guest-a")
	return &pb.CheckKeyResponse{SameTenant: same}, nil
}

func TestCheckKeyAsksTheOrchestrator(t *testing.T) {
	router := &CommandRouter{orchClient: &fakeOrchClient{}, apiKey: "test-key"}
	tests := []struct {
		key    string
		guests []string
		want   bool
	}{
		{"sibling-key", nil, true},
		{"stranger", []string{"guest-a"}, false},
		{"guest-sibling", []string{"guest-a"}, true},
		{"guest-sibling", nil, false},
	}
	for _, tt := range tests {
		if ok, err := router.CheckKey(tt.key, tt.guests); ok != tt.want || err != nil {
			t.Errorf("%s with guests %v: got (%v, %v), want (%v, nil)", tt.key, tt.guests, ok, err, tt.want)
		}
	}
}

func (f *fakeOrchClient) ReleaseLock(ctx context.Context, in *pb.ReleaseLockRequest, opts ...grpc.CallOption) (*pb.ReleaseLockResponse, error) {
	f.lastRelease = in
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	return &pb.ReleaseLockResponse{Released: true}, nil
}

func TestLockRequestsCarryRunURLAndStatus(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "lock-1", Serials: []string{"DEVICE_A"}}}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

	if _, err := router.AcquireLock(context.Background(), nil, 1, 30, &pb.AcquireLockRequest{Repo: "compscidr/icmp", RunUrl: "https://github.com/compscidr/icmp/actions/runs/1"}, -1, true, "", ""); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if got := fake.lastAcquire.RunUrl; got != "https://github.com/compscidr/icmp/actions/runs/1" {
		t.Errorf("run_url not forwarded, got %q", got)
	}
	if got := fake.lastAcquire.Priority; got != -1 {
		t.Errorf("priority not forwarded, got %d", got)
	}
	if !fake.lastAcquire.SameProxy {
		t.Error("same_proxy not forwarded")
	}
	if len(fake.clientIP) != 0 {
		t.Errorf("no client: got x-client-ip %v", fake.clientIP)
	}
	if _, err := router.AcquireLock(context.Background(), nil, 1, 30, nil, 0, false, "", "192.168.1.20"); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if len(fake.clientIP) != 1 || fake.clientIP[0] != "192.168.1.20" {
		t.Errorf("client address not forwarded, got x-client-ip %v", fake.clientIP)
	}
	if _, err := router.ReleaseLock("lock-1", "failure", []*pb.LockLogEntry{{Service: "shell,v2,raw:ls"}}); err != nil {
		t.Fatalf("ReleaseLock returned error: %v", err)
	}
	if fake.lastRelease == nil || fake.lastRelease.LockId != "lock-1" || fake.lastRelease.Status != "failure" {
		t.Errorf("release not forwarded with status, got %+v", fake.lastRelease)
	}
	if len(fake.lastRelease.GetLog()) != 1 || fake.lastRelease.Log[0].Service != "shell,v2,raw:ls" {
		t.Errorf("release did not carry the log, got %+v", fake.lastRelease.GetLog())
	}
}

func (f *fakeOrchClient) AddLockLabels(ctx context.Context, in *pb.AddLockLabelsRequest, opts ...grpc.CallOption) (*pb.AddLockLabelsResponse, error) {
	f.lastLabels = in
	return &pb.AddLockLabelsResponse{}, nil
}

func (f *fakeOrchClient) AcquireLock(ctx context.Context, in *pb.AcquireLockRequest, opts ...grpc.CallOption) (*pb.AcquireLockResponse, error) {
	f.lastAcquire = in
	f.acquires++
	md, _ := metadata.FromOutgoingContext(ctx)
	f.clientIP = md.Get("x-client-ip")
	if f.acquires <= f.unavailable {
		return nil, status.Error(codes.Unavailable, "error reading from server: connection reset by peer")
	}
	if f.queue {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return f.resp, f.err
}

// A runner that hangs up while queued cancels the orchestrator call, which
// takes it out of the queue (sair#133).
func TestAcquireLockEndsWhenTheCallerGivesUp(t *testing.T) {
	router := &CommandRouter{orchClient: &fakeOrchClient{queue: true}, apiKey: "test-key"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, err := router.AcquireLock(ctx, nil, 1, 30, nil, 0, false, "", ""); status.Code(err) != codes.Canceled {
		t.Fatalf("got %v, want Canceled", err)
	}
}

func TestAcquireLockRetriesAConnectionReset(t *testing.T) {
	defer func(d time.Duration) { acquireRetryDelay = d }(acquireRetryDelay)
	acquireRetryDelay = time.Millisecond
	fake := &fakeOrchClient{unavailable: 2, resp: &pb.AcquireLockResponse{LockId: "lock-1", Serials: []string{"DEVICE_A"}}}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

	result, err := router.AcquireLock(context.Background(), nil, 1, 30, nil, 0, true, "", "")
	if err != nil || result.LockID != "lock-1" || fake.acquires != 3 {
		t.Fatalf("got %+v, %v after %d calls; want lock-1 on the third", result, err, fake.acquires)
	}

	// Anything but Unavailable is the orchestrator's answer: no retry.
	fake = &fakeOrchClient{err: status.Error(codes.InvalidArgument, "cannot satisfy request")}
	router.orchClient = fake
	if _, err := router.AcquireLock(context.Background(), nil, 2, 30, nil, 0, true, "", ""); status.Code(err) != codes.InvalidArgument || fake.acquires != 1 {
		t.Fatalf("got %v after %d calls; want InvalidArgument after 1", err, fake.acquires)
	}
}

func TestAcquireLockSendsCount(t *testing.T) {
	fake := &fakeOrchClient{
		resp: &pb.AcquireLockResponse{LockId: "lock-1", Serials: []string{"DEVICE_A", "DEVICE_B"}},
	}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

	result, err := router.AcquireLock(context.Background(), nil, 2, 30, &pb.AcquireLockRequest{Repo: "compscidr/hello-kotlin-android"}, 0, false, "", "")
	if err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}

	if fake.lastAcquire.Count != 2 {
		t.Errorf("got count %d, want 2", fake.lastAcquire.Count)
	}
	if len(fake.lastAcquire.Serials) != 0 {
		t.Errorf("got serials %v, want none", fake.lastAcquire.Serials)
	}
	if result.LockID != "lock-1" {
		t.Errorf("got lock id %q, want %q", result.LockID, "lock-1")
	}
	if len(result.Serials) != 2 {
		t.Errorf("got %d serials, want 2", len(result.Serials))
	}
}

func TestAcquireLockWithSerialsSendsNoCount(t *testing.T) {
	fake := &fakeOrchClient{
		resp: &pb.AcquireLockResponse{LockId: "lock-2", Serials: []string{"DEVICE_A"}},
	}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

	if _, err := router.AcquireLock(context.Background(), map[string]struct{}{"DEVICE_A": {}}, 0, 30, nil, 0, false, "", ""); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}

	if fake.lastAcquire.Count != 0 {
		t.Errorf("got count %d, want 0", fake.lastAcquire.Count)
	}
	if len(fake.lastAcquire.Serials) != 1 || fake.lastAcquire.Serials[0] != "DEVICE_A" {
		t.Errorf("got serials %v, want [DEVICE_A]", fake.lastAcquire.Serials)
	}
}

func TestAcquireLockRejectsInvalidCount(t *testing.T) {
	tests := []struct {
		name    string
		serials map[string]struct{}
		count   int32
	}{
		{"negative count", nil, -1},
		{"count with serials", map[string]struct{}{"DEVICE_A": {}}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeOrchClient{}
			router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

			if _, err := router.AcquireLock(context.Background(), tt.serials, tt.count, 30, nil, 0, false, "", ""); err == nil {
				t.Error("expected an error")
			}
			if fake.lastAcquire != nil {
				t.Error("invalid request was sent to the orchestrator")
			}
		})
	}
}

func TestAcquireLockCarriesProxyID(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "lock-1"}}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key", proxyID: "host-a"}
	if _, err := router.AcquireLock(context.Background(), nil, 1, 30, nil, 0, false, "", ""); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if got := fake.lastAcquire.GetProxyId(); got != "host-a" {
		t.Errorf("proxy_id not sent, got %q", got)
	}
}

func TestProxyIDPrefersEnvThenHostname(t *testing.T) {
	t.Setenv("PROXY_ID", " lab-box ")
	if got := ProxyID(); got != "lab-box" {
		t.Errorf("env override not used (trimmed), got %q", got)
	}
	t.Setenv("PROXY_ID", "")
	host, _ := os.Hostname()
	if got := ProxyID(); got != host || got == "" {
		t.Errorf("hostname fallback wrong, got %q want %q", got, host)
	}
}
