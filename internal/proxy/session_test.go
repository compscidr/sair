package proxy

import (
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
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
