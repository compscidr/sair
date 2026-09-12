package proxy

import (
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

func TestRelayProtoShapes(t *testing.T) {
	setup := &pb.TunnelData{Payload: &pb.TunnelData_Setup{Setup: &pb.TunnelSetup{
		TunnelId: "t1", Side: pb.TunnelSide_TUNNEL_SIDE_HOLDER, Serial: "DEV1", InitialCommand: "sync:",
	}}}
	if setup.GetSetup().GetSide() != pb.TunnelSide_TUNNEL_SIDE_HOLDER || setup.GetSetup().GetSerial() != "DEV1" {
		t.Errorf("tunnel setup round trip failed")
	}
	data := &pb.TunnelData{Payload: &pb.TunnelData_Data{Data: []byte("OKAY")}}
	if string(data.GetData()) != "OKAY" {
		t.Errorf("tunnel data round trip failed")
	}
	open := &pb.OrchestratorMessage{Msg: &pb.OrchestratorMessage_RelayOpen{RelayOpen: &pb.RelayOpen{TunnelId: "t1", Serial: "DEV1", LockId: "l1"}}}
	if open.GetRelayOpen().GetLockId() != "l1" {
		t.Errorf("relay_open round trip failed")
	}
	failed := &pb.ProxyMessage{Msg: &pb.ProxyMessage_RelayFailed{RelayFailed: &pb.RelayFailed{TunnelId: "t1", Error: "no such device"}}}
	if failed.GetRelayFailed().GetError() != "no such device" {
		t.Errorf("relay_failed round trip failed")
	}
	resp := &pb.AcquireLockResponse{RemoteDevices: []*pb.DeviceInfo{{Serial: "DEV1", Model: "Pixel"}}}
	if len(resp.GetRemoteDevices()) != 1 {
		t.Errorf("remote_devices round trip failed")
	}
}
