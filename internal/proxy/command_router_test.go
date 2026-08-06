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
	resp        *pb.AcquireLockResponse
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

	result, err := router.AcquireLock(nil, 2, 30, "compscidr/hello-kotlin-android")
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

	if _, err := router.AcquireLock(map[string]struct{}{"DEVICE_A": {}}, 0, 30, ""); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}

	if fake.lastAcquire.Count != 0 {
		t.Errorf("got count %d, want 0", fake.lastAcquire.Count)
	}
	if len(fake.lastAcquire.Serials) != 1 || fake.lastAcquire.Serials[0] != "DEVICE_A" {
		t.Errorf("got serials %v, want [DEVICE_A]", fake.lastAcquire.Serials)
	}
}
