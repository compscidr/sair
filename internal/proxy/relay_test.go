package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
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

// fakeRelayOrchestrator implements Tunnel by splicing the HOLDER and OWNER
// calls that name the same tunnel id, like the real orchestrator will. It also
// records setups and lets a test push relay_open on a fakeSessionServer.
type fakeRelayOrchestrator struct {
	fakeSessionServer
	mu     sync.Mutex
	setups []*pb.TunnelSetup
	halves map[string]chan grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]
	refuse *status.Status  // when set, every Tunnel call ends with this status
	ended  map[string]bool // tunnel_id -> splice() has returned (both legs done relaying)
}

func newFakeRelayOrchestrator() *fakeRelayOrchestrator {
	return &fakeRelayOrchestrator{fakeSessionServer: *newFakeSessionServer(), halves: map[string]chan grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]{}}
}

func (f *fakeRelayOrchestrator) Tunnel(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	if f.refuse != nil {
		return f.refuse.Err()
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	setup := first.GetSetup()
	if setup == nil {
		return status.Error(codes.InvalidArgument, "setup first")
	}
	f.mu.Lock()
	f.setups = append(f.setups, setup)
	ch, ok := f.halves[setup.TunnelId]
	if !ok {
		ch = make(chan grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData], 1)
		f.halves[setup.TunnelId] = ch
	}
	f.mu.Unlock()
	select {
	case peer := <-ch: // the other half arrived first: we are second, splice from here
		err := splice(stream, peer)
		f.mu.Lock()
		if f.ended == nil {
			f.ended = map[string]bool{}
		}
		f.ended[setup.TunnelId] = true
		f.mu.Unlock()
		return err
	case ch <- stream: // we are first: the second caller splices
		<-stream.Context().Done()
		return nil
	}
}

// splice pumps x<->y on server-side streams until both end. It forwards both
// data and half_close messages: a bare Recv()-EOF from a client's own
// CloseSend only tells THIS RPC "no more from that side" -- it can't by
// itself unblock the *other* leg's client, since a server can't half-close a
// client's read side without ending the whole RPC (which would cut off a
// reply still in flight). The explicit half_close message is what lets that
// signal cross to the peer leg while both stay open.
func splice(x, y grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	var wg sync.WaitGroup
	cp := func(from, to grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) {
		defer wg.Done()
		for {
			m, err := from.Recv()
			if err != nil {
				return
			}
			switch p := m.Payload.(type) {
			case *pb.TunnelData_Data:
				if err := to.Send(&pb.TunnelData{Payload: &pb.TunnelData_Data{Data: p.Data}}); err != nil {
					return
				}
			case *pb.TunnelData_HalfClose:
				if err := to.Send(&pb.TunnelData{Payload: &pb.TunnelData_HalfClose{HalfClose: true}}); err != nil {
					return
				}
			}
		}
	}
	wg.Add(2)
	go cp(x, y)
	go cp(y, x)
	wg.Wait()
	return nil
}

// fakeDeviceSource answers ForwardToDevice by echoing: it replies "OKAY" to the
// setup, then echoes every byte back upper-cased, and on the client's
// half-close writes "BYE" and ends. That exercises both directions and the
// half-close path the same way a real adb server would.
type fakeDeviceSource struct {
	dspb.UnimplementedDeviceSourceServer
	mu     sync.Mutex
	setups []*dspb.ForwardSetup
	// failAfterSetup, when set, makes ForwardToDevice return this status
	// right after replying OKAY, simulating the device disappearing mid-use.
	failAfterSetup *status.Status
}

func (d *fakeDeviceSource) ForwardToDevice(stream grpc.BidiStreamingServer[dspb.ForwardData, dspb.ForwardData]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.setups = append(d.setups, first.GetSetup())
	d.mu.Unlock()
	if err := stream.Send(&dspb.ForwardData{Payload: &dspb.ForwardData_Data{Data: []byte("OKAY")}}); err != nil {
		return err
	}
	if d.failAfterSetup != nil {
		return d.failAfterSetup.Err()
	}
	for {
		m, err := stream.Recv()
		if err == io.EOF {
			return stream.Send(&dspb.ForwardData{Payload: &dspb.ForwardData_Data{Data: []byte("BYE")}})
		}
		if err != nil {
			return err
		}
		if data := m.GetData(); data != nil {
			up := []byte(strings.ToUpper(string(data)))
			if err := stream.Send(&dspb.ForwardData{Payload: &dspb.ForwardData_Data{Data: up}}); err != nil {
				return err
			}
		}
	}
}

func startFakeDeviceSource(t *testing.T, d *fakeDeviceSource) (addr string, dial func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	dspb.RegisterDeviceSourceServer(srv, d)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	// "passthrough:///" skips real DNS resolution of the fake address; the
	// dialer below is what actually reaches the bufconn listener.
	return "passthrough:///bufconn-ds", func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
}

// relayRouter returns a router whose session is up against the fake
// orchestrator and whose device-source dialer reaches the fake device source.
func relayRouter(t *testing.T, f *fakeRelayOrchestrator, ds *fakeDeviceSource, localSerials map[string]string) *CommandRouter {
	t.Helper()
	client := startFakeOrchestratorFrom(t, f) // registers the outer fake, so Tunnel is served
	r := &CommandRouter{orchClient: client, apiKey: "key-1", proxyID: "host-a"}
	r.StartSession("v1", 60, nil, nil)
	t.Cleanup(r.sess.stop)
	waitFor(t, "connected", r.sess.connected)
	_, dial := startFakeDeviceSource(t, ds)
	r.dsDialer = dial
	r.ResolveSource = func(serial string) string { return localSerials[serial] }
	return r
}

// tcpPair returns a connected loopback pair; the client is a *net.TCPConn so
// tests can CloseWrite it, which is how adb half-closes.
func tcpPair(t *testing.T) (client *net.TCPConn, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { c.Close(); server.Close() })
	return c.(*net.TCPConn), server
}

func TestRelayToDeviceRoundTripsThroughOrchestratorAndForwardsHalfClose(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{}
	// One router plays both roles: holder for "REMOTE1" (not local), owner for it when relay_open arrives.
	r := relayRouter(t, f, ds, map[string]string{"REMOTE1": "passthrough:///bufconn-ds"})
	// Wire the owner side: when the fake orchestrator would push relay_open, do it ourselves
	// as soon as the holder's setup is seen.
	served := make(chan struct{})
	go func() {
		defer close(served)
		waitFor(t, "holder setup", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.setups) >= 1 })
		f.mu.Lock()
		s := f.setups[0]
		f.mu.Unlock()
		r.ServeRelay(&pb.RelayOpen{TunnelId: s.TunnelId, Serial: s.Serial, InitialCommand: s.InitialCommand, LockId: "l1"})
	}()
	t.Cleanup(func() {
		select {
		case <-served:
		case <-time.After(3 * time.Second):
			t.Error("ServeRelay goroutine did not finish")
		}
	})

	client, server := tcpPair(t)
	done := make(chan error, 1)
	go func() { done <- r.RelayToDevice("REMOTE1", "shell:", server) }()

	buf := make([]byte, 64)
	n, err := io.ReadFull(client, buf[:4])
	if err != nil || string(buf[:n]) != "OKAY" {
		t.Fatalf("expected OKAY from the device source, got %q err=%v", buf[:n], err)
	}
	if _, err := client.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	n, err = io.ReadFull(client, buf[:3])
	if err != nil || string(buf[:n]) != "ABC" {
		t.Fatalf("expected echo ABC, got %q err=%v", buf[:n], err)
	}
	// Half-close the client's write side: the device source must still be able to answer.
	client.CloseWrite()
	n, err = io.ReadFull(client, buf[:3])
	if err != nil || string(buf[:n]) != "BYE" {
		t.Fatalf("expected BYE after half-close, got %q err=%v", buf[:n], err)
	}
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RelayToDevice returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RelayToDevice did not return within 3s")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.setups) != 1 || ds.setups[0].Serial != "REMOTE1" || ds.setups[0].InitialCommand != "shell:" {
		t.Errorf("owner opened the device with %+v", ds.setups)
	}
}

func TestRelayToDeviceSurfacesOrchestratorRefusalAsError(t *testing.T) {
	f := newFakeRelayOrchestrator()
	f.refuse = status.New(codes.FailedPrecondition, "device REMOTE1 is not in lock l1")
	r := relayRouter(t, f, &fakeDeviceSource{}, nil)
	client, server := tcpPair(t)
	defer client.Close()
	err := r.RelayToDevice("REMOTE1", "", server)
	if err == nil || !strings.Contains(err.Error(), "not in lock") {
		t.Errorf("want the orchestrator's message, got %v", err)
	}
}

func TestRelayToDeviceOnOldOrchestratorIsUnknownDevice(t *testing.T) {
	f := newFakeRelayOrchestrator()
	f.refuse = status.New(codes.Unimplemented, "no Tunnel")
	r := relayRouter(t, f, &fakeDeviceSource{}, nil)
	client, server := tcpPair(t)
	defer client.Close()
	err := r.RelayToDevice("REMOTE1", "", server)
	if !errors.Is(err, errUnknownDevice) {
		t.Errorf("want errUnknownDevice, got %v", err)
	}
}

func TestServeRelayReportsUnknownSerial(t *testing.T) {
	f := newFakeRelayOrchestrator()
	r := relayRouter(t, f, &fakeDeviceSource{}, map[string]string{}) // nothing local
	r.ServeRelay(&pb.RelayOpen{TunnelId: "t9", Serial: "NOPE", LockId: "l1"})
	waitFor(t, "relay_failed on the session", func() bool {
		msgs, _ := f.snapshot()
		for _, m := range msgs {
			if rf := m.GetRelayFailed(); rf != nil && rf.TunnelId == "t9" && strings.Contains(rf.Error, "NOPE") {
				return true
			}
		}
		return false
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.setups) != 0 {
		t.Errorf("owner must not open a tunnel for an unknown serial, got %+v", f.setups)
	}
}

// TestServeRelayDoesNotDrainAfterAnError guards against a concurrency bug:
// when pumpStreams returns a real error (not the graceful nil it returns
// once both directions have already drained), the *other* copyDir goroutine
// can still be running and still calling Send/Recv on the same orchestrator
// stream. ServeRelay must not also touch that stream (CloseSend, then a
// drain loop calling Recv) in that case -- it must skip straight to
// cancelling. Here the device source errors right after the setup reply
// while the holder side is kept open and otherwise idle, so if ServeRelay
// drained anyway it would either race the still-live copyDir goroutine
// (caught by -race) or hang forever waiting on a stream nothing will ever
// close for it.
func TestServeRelayDoesNotDrainAfterAnError(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{failAfterSetup: status.New(codes.Internal, "device unplugged")}
	r := relayRouter(t, f, ds, map[string]string{"REMOTE1": "passthrough:///bufconn-ds"})

	client, server := tcpPair(t)
	go func() { r.RelayToDevice("REMOTE1", "shell:", server) }()

	waitFor(t, "holder setup", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.setups) >= 1 })
	f.mu.Lock()
	s := f.setups[0]
	f.mu.Unlock()

	served := make(chan struct{})
	go func() {
		defer close(served)
		r.ServeRelay(&pb.RelayOpen{TunnelId: s.TunnelId, Serial: s.Serial, InitialCommand: s.InitialCommand, LockId: "l1"})
	}()

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeRelay did not return within 3s after the device source errored")
	}

	// The orchestrator's own splice for this tunnel must also wind down: its
	// owner leg reliably ends the moment ocancel fires (that context is
	// cancelled directly), and its holder leg ends once the holder eventually
	// closes (which it does here) and the orchestrator's attempt to forward
	// that onward hits the already-cancelled owner leg. This is the reliable
	// half of teardown to assert on: ServeRelay's error path sends no
	// half_close of its own (by design, per the fix above), so whether the
	// holder's RelayToDevice call notices in any bounded time is best-effort
	// -- its own half_close there is only enqueued, not confirmed delivered,
	// before ocancel/cancel run -- and is not asserted here.
	client.Close()
	waitFor(t, "the orchestrator's splice for this tunnel to end", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.ended[s.TunnelId]
	})
}
