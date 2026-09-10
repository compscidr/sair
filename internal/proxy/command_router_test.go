package proxy

import (
	"context"
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
)

// fakeOrchClient records the last AcquireLock request and returns a canned
// response. The embedded interface leaves the unused RPCs unimplemented.
type fakeOrchClient struct {
	pb.OrchestratorClient
	lastAcquire *pb.AcquireLockRequest
	lastRelease *pb.ReleaseLockRequest
	resp        *pb.AcquireLockResponse
}

func (f *fakeOrchClient) ReleaseLock(ctx context.Context, in *pb.ReleaseLockRequest, opts ...grpc.CallOption) (*pb.ReleaseLockResponse, error) {
	f.lastRelease = in
	return &pb.ReleaseLockResponse{Released: true}, nil
}

func TestLockRequestsCarryRunURLAndStatus(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "lock-1", Serials: []string{"DEVICE_A"}}}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

	if _, err := router.AcquireLock(nil, 1, 30, "compscidr/icmp", "https://github.com/compscidr/icmp/actions/runs/1"); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if got := fake.lastAcquire.RunUrl; got != "https://github.com/compscidr/icmp/actions/runs/1" {
		t.Errorf("run_url not forwarded, got %q", got)
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

func (f *fakeOrchClient) AcquireLock(ctx context.Context, in *pb.AcquireLockRequest, opts ...grpc.CallOption) (*pb.AcquireLockResponse, error) {
	f.lastAcquire = in
	return f.resp, nil
}

func TestAcquireLockSendsCount(t *testing.T) {
	fake := &fakeOrchClient{
		resp: &pb.AcquireLockResponse{LockId: "lock-1", Serials: []string{"DEVICE_A", "DEVICE_B"}},
	}
	router := &CommandRouter{orchClient: fake, apiKey: "test-key"}

	result, err := router.AcquireLock(nil, 2, 30, "compscidr/hello-kotlin-android", "")
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

	if _, err := router.AcquireLock(map[string]struct{}{"DEVICE_A": {}}, 0, 30, "", ""); err != nil {
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

			if _, err := router.AcquireLock(tt.serials, tt.count, 30, "", ""); err == nil {
				t.Error("expected an error")
			}
			if fake.lastAcquire != nil {
				t.Error("invalid request was sent to the orchestrator")
			}
		})
	}
}
