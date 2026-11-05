package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
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
// calls that name the same tunnel id, like the real orchestrator will. Once
// both halves have joined it sends the holder an accepted message before
// splicing. Whatever error splice() returns (nil on a clean end) is also used
// to end the first-arrived leg's own RPC, so a real failure on one leg ends
// the other's promptly too -- the real orchestrator's contract -- rather than
// leaving it blocked on its own client disconnecting. It also records setups
// and lets a test push relay_open on a fakeSessionServer.
type fakeRelayOrchestrator struct {
	fakeSessionServer
	mu     sync.Mutex
	setups []*pb.TunnelSetup
	halves map[string]chan tunnelHalf
	// done delivers, per tunnel id, the error splice() returned for it (nil on
	// a clean end) -- signaling the first-arrived leg (blocked waiting for the
	// other side) to end its own RPC the same way.
	done   map[string]chan error
	refuse *status.Status  // when set, every Tunnel call ends with this status
	ended  map[string]bool // tunnel_id -> splice() has returned (both legs done relaying)
}

// tunnelHalf pairs a server-side stream with the side it was set up as, so
// the second arrival can tell which of the two is the holder.
type tunnelHalf struct {
	stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]
	side   pb.TunnelSide
}

func newFakeRelayOrchestrator() *fakeRelayOrchestrator {
	return &fakeRelayOrchestrator{fakeSessionServer: *newFakeSessionServer(), halves: map[string]chan tunnelHalf{}, done: map[string]chan error{}}
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
		ch = make(chan tunnelHalf, 1)
		f.halves[setup.TunnelId] = ch
	}
	done, ok := f.done[setup.TunnelId]
	if !ok {
		done = make(chan error, 1)
		f.done[setup.TunnelId] = done
	}
	f.mu.Unlock()

	me := tunnelHalf{stream, setup.Side}
	select {
	case peer := <-ch: // the other half arrived first: we are second, splice from here
		holder, owner := me, peer
		if holder.side != pb.TunnelSide_TUNNEL_SIDE_HOLDER {
			holder, owner = owner, holder
		}
		if err := holder.stream.Send(&pb.TunnelData{Payload: &pb.TunnelData_Accepted{Accepted: true}}); err != nil {
			done <- err
			return err
		}
		err := splice(owner.stream, holder.stream)
		f.mu.Lock()
		if f.ended == nil {
			f.ended = map[string]bool{}
		}
		f.ended[setup.TunnelId] = true
		f.mu.Unlock()
		if err != nil {
			// A leg that fails typically ends by its own client cancelling
			// its context, which a bare Recv() here would report as
			// codes.Canceled -- indistinguishable, to the *other* leg's
			// RelayToDevice, from the codes.Canceled it sees when it cancels
			// its own context after a clean pump (which relayErr treats as
			// nil, not a failure). Re-wrap as Aborted so ending the peer's
			// leg on a real failure reads as a failure there too.
			err = status.Error(codes.Aborted, "peer leg ended: "+err.Error())
		}
		done <- err
		return err
	case ch <- me: // we are first: the second caller splices
		select {
		case <-stream.Context().Done():
			return nil
		case err := <-done:
			return err
		}
	}
}

// splice pumps x<->y on server-side streams until both end, the same
// short-circuit-on-real-error/wait-on-graceful-end shape as pump.go's
// pumpStreams: a real error on either leg returns immediately (so the caller
// can end the other leg's RPC without waiting on it), while a graceful
// end (io.EOF) on one side waits for the other to also finish before
// returning nil. It forwards both data and half_close messages: a bare
// Recv()-EOF from a client's own CloseSend only tells THIS RPC "no more from
// that side" -- it can't by itself unblock the *other* leg's client, since a
// server can't half-close a client's read side without ending the whole RPC
// (which would cut off a reply still in flight). The explicit half_close
// message is what lets that signal cross to the peer leg while both stay
// open.
func splice(x, y grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	errs := make(chan error, 2)
	cp := func(from, to grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) {
		for {
			m, err := from.Recv()
			if err != nil {
				if err == io.EOF {
					errs <- nil
				} else {
					errs <- err
				}
				return
			}
			switch p := m.Payload.(type) {
			case *pb.TunnelData_Data:
				if err := to.Send(&pb.TunnelData{Payload: &pb.TunnelData_Data{Data: p.Data}}); err != nil {
					errs <- err
					return
				}
			case *pb.TunnelData_HalfClose:
				if err := to.Send(&pb.TunnelData{Payload: &pb.TunnelData_HalfClose{HalfClose: true}}); err != nil {
					errs <- err
					return
				}
			}
		}
	}
	go cp(x, y)
	go cp(y, x)
	if err := <-errs; err != nil {
		return err
	}
	if err := <-errs; err != nil {
		return err
	}
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
	// endsFirst, when set, makes ForwardToDevice return nil right after
	// sending OKAY -- the device ending its side cleanly on its own, without
	// ever waiting for the client to half-close first (e.g. `adb shell <cmd>`
	// or connectedCheck, where the command's own output is the end).
	endsFirst bool
	// installed, when non-nil, is the phone's third-party packages: ListPackages
	// lists them and ResetPackages applies since to them. Nil is an older device
	// source, Unimplemented for both.
	installed map[string]struct{}
	// listErr fails ListPackages; listGate, when set, blocks it until closed.
	listErr   error
	listGate  chan struct{}
	listCalls int
	sinces    []*dspb.PackageSnapshot // one per ResetPackages
	// baseline is what ResetPackages keeps with no since.
	baseline []string
}

func (d *fakeDeviceSource) ListPackages(ctx context.Context, req *dspb.ListPackagesRequest) (*dspb.ListPackagesResponse, error) {
	d.mu.Lock()
	d.listCalls++
	gate := d.listGate
	d.mu.Unlock()
	if gate != nil {
		<-gate
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	if d.installed == nil {
		return d.UnimplementedDeviceSourceServer.ListPackages(ctx, req)
	}
	return &dspb.ListPackagesResponse{Packages: slices.Sorted(maps.Keys(d.installed))}, nil
}

func (d *fakeDeviceSource) ResetPackages(_ context.Context, req *dspb.ResetPackagesRequest) (*dspb.ResetPackagesResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sinces = append(d.sinces, req.Since)
	keep := d.baseline
	if req.Since != nil {
		keep = req.Since.Packages
	}
	resp := &dspb.ResetPackagesResponse{}
	for _, pkg := range slices.Sorted(maps.Keys(d.installed)) {
		if !slices.Contains(keep, pkg) {
			delete(d.installed, pkg)
			resp.Removed = append(resp.Removed, pkg)
		}
	}
	return resp, nil
}

func (d *fakeDeviceSource) install(pkg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.installed[pkg] = struct{}{}
}

func (d *fakeDeviceSource) stats() (setups, lists int, sinces []*dspb.PackageSnapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.setups), d.listCalls, slices.Clone(d.sinces)
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
	if d.endsFirst {
		return nil
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
	r.StartSession("v1", 60, nil, nil, nil)
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
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
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

// TestAdbConnectionTunnelForwardsDeviceHalfCloseOnAScopedPort covers a scoped
// port (non-nil LockLog), where AdbConnection.tunnel wraps the client conn in
// a *sniffConn before handing it to RelayToDevice. If sniffConn does not
// forward CloseWrite, tcpStream.CloseSend's type assertion silently fails and
// the client never sees the device's half-close: it hangs waiting for EOF
// forever instead of finding out the device (which here ends its side right
// after OKAY, like `adb shell <cmd>` or connectedCheck) is already done.
func TestAdbConnectionTunnelForwardsDeviceHalfCloseOnAScopedPort(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{endsFirst: true}
	r := relayRouter(t, f, ds, map[string]string{"REMOTE1": "passthrough:///bufconn-ds"})

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
	client.SetReadDeadline(time.Now().Add(3 * time.Second))

	c := NewAdbConnection(server, r, nil, nil, &LockLog{}, map[string]*pb.DeviceInfo{"REMOTE1": {Serial: "REMOTE1"}})
	done := make(chan error, 1)
	go func() { done <- c.tunnel("", "REMOTE1") }()

	// Send only the service (the proxy reads it before relaying), then read
	// until EOF. A timeout here (instead of a clean EOF) means CloseWrite
	// never reached the client -- the bug this test guards against.
	fmt.Fprintf(client, "%04xshell:", len("shell:"))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read failed (want a clean EOF once the device ended first), got err=%v data=%q", err, got)
	}
	if string(got) != "OKAY" {
		t.Fatalf("expected to read OKAY before EOF, got %q", got)
	}
	// A real adb client closes the whole connection once its read side sees
	// EOF; do the same so the holder's other direction (reading the client's
	// now-absent request) also unblocks and tunnel can return.
	client.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("tunnel returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel did not return within 3s")
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
	var refused relayRefused
	if !errors.As(err, &refused) {
		t.Errorf("a refusal before accepted must be a relayRefused, got %T: %v", err, err)
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
	var refused relayRefused
	if !errors.As(err, &refused) {
		t.Errorf("an old orchestrator's refusal must be a relayRefused, got %T: %v", err, err)
	}
}

// TestRelayToDeviceRefusalAfterSetupIsStillAFail covers the case that used to
// be ambiguous: a refusal that the orchestrator only decides on after reading
// the holder's setup (e.g. because the setup names a serial not in the lock),
// rather than one it could reject outright. Before the holder waited for
// accepted, this looked identical to a mid-stream failure -- it happened
// after the setup was sent, on the same code path pumpStreams errors take --
// so it silently lost its FAIL. Now any error before accepted, for any
// reason, is a relayRefused.
func TestRelayToDeviceRefusalAfterSetupIsStillAFail(t *testing.T) {
	client := startFakeOrchestratorFrom(t, &refuseAfterSetupOrchestrator{})
	r := &CommandRouter{orchClient: client, apiKey: "key-1", proxyID: "host-a"}
	clientConn, server := tcpPair(t)
	defer clientConn.Close()
	err := r.RelayToDevice("REMOTE1", "", server)
	if err == nil || !strings.Contains(err.Error(), "not in lock") {
		t.Errorf("want the orchestrator's message, got %v", err)
	}
	var refused relayRefused
	if !errors.As(err, &refused) {
		t.Errorf("a refusal discovered after setup must still be a relayRefused, got %T: %v", err, err)
	}
}

// refuseAfterSetupOrchestrator reads the holder's setup (unlike
// fakeRelayOrchestrator.refuse, which never does) and only then refuses --
// modelling a rejection the orchestrator can only make once it has seen what
// the holder is asking for.
type refuseAfterSetupOrchestrator struct{ fakeSessionServer }

func (f *refuseAfterSetupOrchestrator) Tunnel(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return status.Error(codes.FailedPrecondition, "device REMOTE1 is not in lock l1")
}

// TestRelayToDeviceMidStreamFailureIsNotARefusal reuses Task 3's
// device-source-errors-mid-stream setup (TestServeRelayDoesNotDrainAfterAnError):
// the tunnel is accepted and the client already sees the device source's OKAY
// before the device disappears. That failure must come back as a plain
// error, not a relayRefused, since real bytes already reached the client.
func TestRelayToDeviceMidStreamFailureIsNotARefusal(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{failAfterSetup: status.New(codes.Internal, "device unplugged")}
	r := relayRouter(t, f, ds, map[string]string{"REMOTE1": "passthrough:///bufconn-ds"})

	client, server := tcpPair(t)
	done := make(chan error, 1)
	go func() { done <- r.RelayToDevice("REMOTE1", "shell:", server) }()

	waitFor(t, "holder setup", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.setups) >= 1 })
	f.mu.Lock()
	s := f.setups[0]
	f.mu.Unlock()
	go r.ServeRelay(&pb.RelayOpen{TunnelId: s.TunnelId, Serial: s.Serial, InitialCommand: s.InitialCommand, LockId: "l1"})

	// Read the OKAY the fake device source sends right after setup, so bytes
	// have definitely flowed before the mid-stream failure below.
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "OKAY" {
		t.Fatalf("expected OKAY before the mid-stream failure, got %q err=%v", buf, err)
	}
	// When a direction's RecvBytes fails (EOF or a real error), pumpStreams
	// half-closes the peer before reporting, so the peer does not hang waiting
	// for more input; a SendBytes failure reports without that courtesy, since
	// the stream that failed to send is already dead. The owner's device-source
	// error is a RecvBytes failure, so it reaches the holder as a graceful
	// tunnel end on that direction. The holder's other
	// direction -- reading the local ADB client connection -- has nothing to
	// do with the tunnel and would otherwise block forever, since nothing
	// here ever writes to or closes the client's write side. SetLinger(0)
	// makes the close an RST rather than a graceful FIN, so the holder's
	// local read fails with a real error instead of io.EOF, giving
	// pumpStreams a real error to short-circuit on and making the outcome
	// deterministic instead of a hang.
	client.SetLinger(0)
	client.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a mid-stream error")
		}
		var refused relayRefused
		if errors.As(err, &refused) {
			t.Errorf("mid-stream failure after bytes flowed must not be relayRefused, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RelayToDevice did not return within 3s")
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

// A relay_open for a phone whose release reset still holds its serial lock
// must not reach the device until the reset is done (sair#105).
func TestServeRelayWaitsForReleaseReset(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{}
	r := relayRouter(t, f, ds, map[string]string{"LOCAL1": "passthrough:///bufconn-ds"})
	m := NewScopedPortManager(r, nil, 3600)
	mu := m.wifiLock("LOCAL1")
	mu.Lock() // the reset
	go r.ServeRelay(&pb.RelayOpen{TunnelId: "t1", Serial: "LOCAL1", LockId: "l1"})
	time.Sleep(200 * time.Millisecond)
	ds.mu.Lock()
	n := len(ds.setups)
	ds.mu.Unlock()
	if n != 0 {
		t.Fatal("relay opened the device during its reset")
	}
	mu.Unlock()
	waitFor(t, "device opened after the reset", func() bool { ds.mu.Lock(); defer ds.mu.Unlock(); return len(ds.setups) == 1 })
}

// relayResetHarness is a manager on a router owning LOCAL1 and LOCAL2, with
// the reset steps a test asserts on recorded in calls. The device source
// fails each relay right after opening it, so a relay here has already ended
// when ServeRelay returns: the orchestrator fails a lock's relays before it
// pushes LockExpired.
type relayResetHarness struct {
	m      *ScopedPortManager
	f      *fakeRelayOrchestrator
	r      *CommandRouter
	ds     *fakeDeviceSource
	mu     sync.Mutex
	calls  []string
	events []*pb.BenchEvent
}

func newRelayResetHarness(t *testing.T) *relayResetHarness {
	t.Helper()
	h := &relayResetHarness{f: newFakeRelayOrchestrator(), ds: &fakeDeviceSource{failAfterSetup: status.New(codes.Unavailable, "device gone")}}
	h.r = relayRouter(t, h.f, h.ds, map[string]string{"LOCAL1": "passthrough:///bufconn-ds", "LOCAL2": "passthrough:///bufconn-ds"})
	h.m = NewScopedPortManager(h.r, nil, 3600)
	record := func(call string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls = append(h.calls, call)
	}
	h.m.removeReverse = func(serial string) error { record(serial + " unreverse"); return nil }
	h.m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
		record(serial + " " + state.String())
		return nil
	}
	h.m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) {
		return []string{"com.example.app"}, nil, nil
	}
	h.m.event = func(e *pb.BenchEvent) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, e)
	}
	return h
}

func (h *relayResetHarness) relay(serial, lockID string) {
	h.r.ServeRelay(&pb.RelayOpen{TunnelId: lockID + serial, Serial: serial, LockId: lockID})
}

func (h *relayResetHarness) sortedCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(slices.Values(h.calls))
}

// The owner's copy of a lock it only relayed: LockExpired resets the phones
// relayed under it, once, even though every relay has already ended, and
// reports the lock id on the bench events.
func TestLockExpiredResetsRelayedSerials(t *testing.T) {
	h := newRelayResetHarness(t)
	h.relay("LOCAL1", "l1")
	h.relay("LOCAL1", "l1")
	h.relay("LOCAL2", "l1")
	h.m.OnLockExpired("l1")
	want := []string{"LOCAL1 ON", "LOCAL1 RESET", "LOCAL1 unreverse", "LOCAL2 ON", "LOCAL2 RESET", "LOCAL2 unreverse"}
	if got := h.sortedCalls(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if len(h.events) == 0 {
		t.Error("no bench events")
	}
	for _, e := range h.events {
		if e.LockId != "l1" {
			t.Errorf("event %v, want lock id l1", e)
		}
	}

	h.m.OnLockExpired("l1") // the record went with the reset
	if got := h.sortedCalls(); len(got) != len(want) {
		t.Errorf("a second LockExpired reset again: calls = %q", got)
	}
}

// A phone already relayed to the next job keeps its APKs, whether that job's
// relay is open right now or between adb connections, and is reset when that
// job's own lock ends.
func TestLockExpiredSkipsSerialWithNewerRelay(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		h := newRelayResetHarness(t)
		h.ds.failAfterSetup = nil // relays stay open
		opened := func(n int) func() bool {
			return func() bool { h.ds.mu.Lock(); defer h.ds.mu.Unlock(); return len(h.ds.setups) == n }
		}
		go h.relay("LOCAL1", "l1")
		waitFor(t, "l1's relay open", opened(1))
		go h.relay("LOCAL1", "l2")
		waitFor(t, "l2's relay open", opened(2))
		h.m.OnLockExpired("l1")
		if got := h.sortedCalls(); len(got) != 0 {
			t.Errorf("reset under a live relay: calls = %q", got)
		}
	})
	t.Run("ended", func(t *testing.T) {
		h := newRelayResetHarness(t)
		h.relay("LOCAL1", "l1")
		h.relay("LOCAL1", "l2")
		h.m.OnLockExpired("l1")
		if got := h.sortedCalls(); len(got) != 0 {
			t.Errorf("reset under the next job: calls = %q", got)
		}
		h.m.OnLockExpired("l2")
		if got := h.sortedCalls(); len(got) != 3 {
			t.Errorf("l2's end did not reset: calls = %q", got)
		}
	})
}

// A reset queued on the phone's serial lock decides only once it has the
// lock: a relay under the next lock that registered meanwhile wins.
func TestLockExpiredQueuedBehindNewRelaySkipsReset(t *testing.T) {
	h := newRelayResetHarness(t)
	h.relay("LOCAL1", "l1")
	mu := h.m.wifiLock("LOCAL1")
	mu.Lock()
	expired := make(chan struct{})
	go func() { h.m.OnLockExpired("l1"); close(expired) }()
	go h.relay("LOCAL1", "l2")
	waitFor(t, "l2 registered while waiting on the serial lock", func() bool {
		rec, _ := h.r.relayLocks.Load("LOCAL1")
		return rec.(*relayRecord).lockID == "l2"
	})
	mu.Unlock()
	<-expired
	if got := h.sortedCalls(); len(got) != 0 {
		t.Errorf("calls = %q", got)
	}
}

// A local port holding the phone owns its state too.
func TestLockExpiredSkipsSerialHeldByLocalPort(t *testing.T) {
	h := newRelayResetHarness(t)
	h.relay("LOCAL1", "l1")
	if _, err := h.m.CreateScopedPort("l2", map[string]struct{}{"LOCAL1": {}}, nil); err != nil {
		t.Fatal(err)
	}
	h.m.OnLockExpired("l1")
	if got := h.sortedCalls(); len(got) != 0 {
		t.Errorf("calls = %q", got)
	}
	h.m.CloseScopedPort("l2")
}

// An owner proxy hears about every lock on its phones, most of which it has
// nothing for; a lock it did relay is unaffected by another's end.
func TestLockExpiredUnknownLockIsNoop(t *testing.T) {
	h := newRelayResetHarness(t)
	h.relay("LOCAL1", "l1")
	h.m.OnLockExpired("other")
	if got := h.sortedCalls(); len(got) != 0 {
		t.Errorf("calls = %q", got)
	}
	h.m.OnLockExpired("l1")
	if got := h.sortedCalls(); len(got) != 3 {
		t.Errorf("l1's record did not survive another lock's end: calls = %q", got)
	}
}

// The orchestrator's stop step reaches a phone local to the holder's proxy
// through the relay too, so a lock can have both a port and a relay record
// for one phone: its end resets the phone once.
func TestLockExpiredWithPortAndRelayResetsOnce(t *testing.T) {
	h := newRelayResetHarness(t)
	if _, err := h.m.CreateScopedPort("l1", map[string]struct{}{"LOCAL1": {}}, nil); err != nil {
		t.Fatal(err)
	}
	h.relay("LOCAL1", "l1")
	h.m.OnLockExpired("l1")
	want := []string{"LOCAL1 ON", "LOCAL1 RESET", "LOCAL1 unreverse"}
	if got := h.sortedCalls(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// snapshotHarness is a relayResetHarness whose resets reach the fake device
// source for real, on a phone with installed on it.
func snapshotHarness(t *testing.T, installed ...string) *relayResetHarness {
	t.Helper()
	h := newRelayResetHarness(t)
	h.ds.installed = make(map[string]struct{})
	for _, pkg := range installed {
		h.ds.installed[pkg] = struct{}{}
	}
	h.m.resetPackages = h.r.ResetPackages
	return h
}

func (h *relayResetHarness) installed() []string {
	h.ds.mu.Lock()
	defer h.ds.mu.Unlock()
	return slices.Sorted(maps.Keys(h.ds.installed))
}

// The reason for the snapshot: an app the phone's owner installed by hand
// outlives a borrower's lock, and an app that appeared during it does not.
func TestRelayedResetRemovesOnlyWhatAppearedDuringTheLock(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	h.relay("LOCAL1", "l1")
	h.ds.install("com.guest.app")
	h.relay("LOCAL1", "l1") // the snapshot stays the first relay's
	h.m.OnLockExpired("l1")
	if got := h.installed(); !slices.Equal(got, []string{"com.owner.hand"}) {
		t.Errorf("installed = %q", got)
	}
	if _, lists, sinces := h.ds.stats(); lists != 1 || len(sinces) != 1 || sinces[0] == nil {
		t.Errorf("lists = %d, sinces = %v; want one of each", lists, sinces)
	}
}

// A phone that really has no third-party apps gives an empty snapshot, which
// is not the same as none: the guest's app still goes.
func TestRelayedResetWithEmptySnapshotStillResets(t *testing.T) {
	h := snapshotHarness(t)
	h.ds.baseline = []string{"com.guest.app"} // an absent since would keep it
	h.relay("LOCAL1", "l1")
	h.ds.install("com.guest.app")
	h.m.OnLockExpired("l1")
	if got := h.installed(); len(got) != 0 {
		t.Errorf("installed = %q", got)
	}
	if _, _, sinces := h.ds.stats(); len(sinces) != 1 || sinces[0] == nil {
		t.Errorf("sinces = %v, want one, present though empty", sinces)
	}
}

// The first listing is the only one taken before the job's first command, so a
// failure isn't retried: a later listing could hold the job's apps, and the
// reset would keep them.
func TestFailedSnapshotIsNotRetried(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	h.ds.listErr = status.Error(codes.Internal, "pm failed")
	h.relay("LOCAL1", "l1")
	h.relay("LOCAL1", "l1")
	if _, lists, _ := h.ds.stats(); lists != 1 {
		t.Errorf("lists = %d, want 1", lists)
	}
}

// packageResetEvents is the harness's PACKAGE_RESET events.
func (h *relayResetHarness) packageResetEvents() []*pb.BenchEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*pb.BenchEvent
	for _, e := range h.events {
		if e.Kind == pb.BenchEventKind_BENCH_EVENT_KIND_PACKAGE_RESET {
			out = append(out, e)
		}
	}
	return out
}

// noReboot fails the test when a reset reboots the phone or asks whether a
// radio is wedged, and returns a func that says how often the failure excerpt
// was collected, the one thing a failed reset step does beyond its reporting.
func (h *relayResetHarness) noReboot(t *testing.T) (excerpts func() int) {
	t.Helper()
	h.m.rebootIfWedged = func(string, string) (bool, error) {
		t.Error("asked to reboot a wedged phone")
		return false, nil
	}
	h.m.rebootIfStale = func(string) (bool, time.Duration, error) { return false, 0, nil }
	n := 0
	h.m.recentSourceLog = func(string) ([]string, error) { n++; return nil, nil }
	return func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, e := range h.events {
			if e.Kind == pb.BenchEventKind_BENCH_EVENT_KIND_REBOOT {
				t.Errorf("reboot event %v", e)
			}
		}
		return n
	}
}

// A relayed lock's packages left on the phone are a reported failure, not a
// silent one, and nothing about them justifies rebooting the phone.
func TestRelayedResetReportsPackagesItLeft(t *testing.T) {
	t.Run("no snapshot", func(t *testing.T) {
		h := newRelayResetHarness(t) // an older device source
		excerpts := h.noReboot(t)
		h.relay("LOCAL1", "l1")
		h.m.OnLockExpired("l1")
		events := h.packageResetEvents()
		if len(events) != 1 || events[0].Outcome != "failed" || events[0].Serial != "LOCAL1" || events[0].LockId != "l1" || !strings.Contains(events[0].Detail, "no package snapshot") {
			t.Errorf("events = %v, want one failed, saying no snapshot", events)
		}
		if n := excerpts(); n != 1 {
			t.Errorf("failure excerpts = %d, want 1", n)
		}
	})
	t.Run("phone gone", func(t *testing.T) {
		h := snapshotHarness(t)
		excerpts := h.noReboot(t)
		h.m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) {
			return nil, nil, status.Error(codes.NotFound, "device LOCAL1 is not attached to this proxy")
		}
		h.relay("LOCAL1", "l1")
		h.m.OnLockExpired("l1")
		events := h.packageResetEvents()
		if len(events) != 1 || events[0].Outcome != "failed" || !strings.Contains(events[0].Detail, "not attached") {
			t.Errorf("events = %v, want one failed, saying the phone is gone", events)
		}
		if n := excerpts(); n != 1 {
			t.Errorf("failure excerpts = %d, want 1", n)
		}
	})
	t.Run("local lock's NotFound stays quiet", func(t *testing.T) {
		h := snapshotHarness(t)
		h.m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) {
			return nil, nil, status.Error(codes.NotFound, "not attached")
		}
		if _, err := h.m.CreateScopedPort("l1", map[string]struct{}{"LOCAL1": {}}, nil); err != nil {
			t.Fatal(err)
		}
		h.m.CloseScopedPort("l1")
		if events := h.packageResetEvents(); len(events) != 0 {
			t.Errorf("events = %v", events)
		}
	})
}

// A relay under a new lock before the old lock's reset means that reset never
// runs. What the old lock left can't be told from the owner's, so it stays, but
// it is said out loud.
func TestReplacedRecordWarnsWhatMayBeLeft(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	h.relay("LOCAL1", "l1")
	h.ds.install("com.guest.app")
	logs := &logBuffer{}
	defer captureSlog(logs)()
	h.relay("LOCAL1", "l2")
	for _, want := range []string{
		"serial=LOCAL1 lockId=l2 previousLockId=l1 packages=[com.guest.app]",
		"serial=LOCAL1 lockId=l2 previousLockId=l1\n",
	} {
		if !logs.contains(want) {
			t.Errorf("no %q in %q", want, logs.String())
		}
	}
	if logs.contains("com.owner.hand") {
		t.Errorf("named a package the old lock had too: %q", logs.String())
	}
	if got := h.installed(); !slices.Equal(got, []string{"com.guest.app", "com.owner.hand"}) {
		t.Errorf("installed = %q: the warning must remove nothing", got)
	}
}

// With no old snapshot to compare, only the replacement itself is warned of.
func TestReplacedRecordWithoutSnapshotWarnsOnly(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	h.ds.listErr = status.Error(codes.Internal, "pm failed")
	h.relay("LOCAL1", "l1")
	h.ds.mu.Lock()
	h.ds.listErr = nil
	h.ds.mu.Unlock()
	logs := &logBuffer{}
	defer captureSlog(logs)()
	h.relay("LOCAL1", "l2")
	if !logs.contains("lockId=l2 previousLockId=l1") {
		t.Errorf("no warning: %q", logs.String())
	}
	if logs.contains("com.owner.hand") {
		t.Errorf("with no old snapshot nothing can be blamed on it: %q", logs.String())
	}
}

// With no snapshot nothing says which apps are the owner's, so none go; the
// rest of the reset runs, a warning names the phone and lock, and the relay
// serves the job regardless.
func TestRelayedResetWithoutSnapshotLeavesPackages(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"fails", status.Error(codes.Internal, "pm failed")},
		{"older device source", nil}, // installed is nil: Unimplemented
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newRelayResetHarness(t)
			h.ds.listErr = tt.err
			logs := &logBuffer{}
			defer captureSlog(logs)()
			resets := 0
			h.m.resetPackages = func(string, *dspb.PackageSnapshot) ([]string, []string, error) {
				resets++
				return nil, nil, nil
			}
			h.m.clearUiAutomation = func(serial string) (string, error) {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.calls = append(h.calls, serial+" uiautomation")
				return "", nil
			}
			h.relay("LOCAL1", "l1")
			if setups, _, _ := h.ds.stats(); setups != 1 {
				t.Errorf("device opened %d times, want 1: the relay must survive a failed snapshot", setups)
			}
			h.m.OnLockExpired("l1")
			if resets != 0 {
				t.Errorf("resetPackages ran %d times without a snapshot", resets)
			}
			want := []string{"LOCAL1 ON", "LOCAL1 RESET", "LOCAL1 uiautomation", "LOCAL1 unreverse"}
			if got := h.sortedCalls(); !slices.Equal(got, want) {
				t.Errorf("calls = %q, want %q", got, want)
			}
			if !logs.contains(`level=WARN msg="no package snapshot for this relayed lock; leaving the phone's packages alone" serial=LOCAL1 lockId=l1`) {
				t.Errorf("no warning naming the phone and lock: %q", logs.String())
			}
		})
	}
}

// A lock can end with a relay still short of its snapshot (waiting out a
// reset, say). The reset then settles the record, so that relay can't take a
// snapshot of the phone after the reset and have it trusted.
func TestLockExpiredBeforeAnySnapshotSettlesTheRecord(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	rec := h.r.relayRecordFor("LOCAL1", "l1")
	h.m.OnLockExpired("l1")
	if _, _, sinces := h.ds.stats(); len(sinces) != 0 {
		t.Errorf("reset packages with no snapshot: %v", sinces)
	}
	if got := h.sortedCalls(); len(got) != 3 {
		t.Errorf("rest of the reset did not run: calls = %q", got)
	}
	client, err := h.r.getOrCreateDSClient("passthrough:///bufconn-ds")
	if err != nil {
		t.Fatal(err)
	}
	rec.takeSnapshot(client, "LOCAL1")
	if _, lists, _ := h.ds.stats(); lists != 0 || rec.settledSnapshot() != nil {
		t.Errorf("a settled record took a snapshot: lists = %d", lists)
	}
}

// Concurrent relays under one lock share one snapshot, and neither reaches
// the device before it is done: the first command could install something.
func TestConcurrentRelaysShareOneSnapshot(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	gate := make(chan struct{})
	h.ds.listGate = gate
	var wg sync.WaitGroup
	wg.Go(func() { h.r.ServeRelay(&pb.RelayOpen{TunnelId: "t1", Serial: "LOCAL1", LockId: "l1"}) })
	wg.Go(func() { h.r.ServeRelay(&pb.RelayOpen{TunnelId: "t2", Serial: "LOCAL1", LockId: "l1"}) })
	waitFor(t, "snapshot started", func() bool { _, lists, _ := h.ds.stats(); return lists == 1 })
	time.Sleep(100 * time.Millisecond) // the other relay arrives at the snapshot
	if setups, lists, _ := h.ds.stats(); setups != 0 || lists != 1 {
		close(gate)
		t.Fatalf("during the snapshot: device opened %d times, %d snapshots", setups, lists)
	}
	close(gate)
	wg.Wait()
	if setups, lists, _ := h.ds.stats(); setups != 2 || lists != 1 {
		t.Errorf("after: device opened %d times, %d snapshots; want 2 and 1", setups, lists)
	}
}

// A reset arriving during the snapshot waits for it and uses it.
func TestLockExpiredWaitsForSnapshotInProgress(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	gate := make(chan struct{})
	h.ds.listGate = gate
	var wg sync.WaitGroup
	wg.Go(func() { h.relay("LOCAL1", "l1") })
	waitFor(t, "snapshot started", func() bool { _, lists, _ := h.ds.stats(); return lists == 1 })
	h.ds.install("com.guest.app")
	expired := make(chan struct{})
	wg.Go(func() { h.m.OnLockExpired("l1"); close(expired) })
	select {
	case <-expired:
		t.Error("reset did not wait for the snapshot")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	wg.Wait()
	if _, _, sinces := h.ds.stats(); len(sinces) != 1 || sinces[0] == nil {
		t.Errorf("sinces = %v, want the snapshot", sinces)
	}
}

// A tunnel that died during the snapshot is not served, as after a reset.
func TestServeRelayTunnelFailedDuringSnapshotNeverOpensDevice(t *testing.T) {
	h := snapshotHarness(t)
	gate := make(chan struct{})
	h.ds.listGate = gate
	var wg sync.WaitGroup
	wg.Go(func() { h.r.ServeRelay(&pb.RelayOpen{TunnelId: "t1", Serial: "LOCAL1", LockId: "l1"}) })
	waitFor(t, "snapshot started", func() bool { _, lists, _ := h.ds.stats(); return lists == 1 })
	h.f.mu.Lock()
	done := h.f.done["t1"]
	h.f.mu.Unlock()
	done <- status.Error(codes.Aborted, "lock l1 released")
	time.Sleep(200 * time.Millisecond) // the failure reaches the proxy
	close(gate)
	wg.Wait()
	if setups, _, _ := h.ds.stats(); setups != 0 {
		t.Errorf("failed tunnel reached the device %d times", setups)
	}
}

// Each lock has its own snapshot, and the old lock's end touches nothing.
func TestRelayUnderSecondLockTakesItsOwnSnapshot(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	h.relay("LOCAL1", "l1")
	h.ds.install("com.other.owner.hand")
	h.relay("LOCAL1", "l2")
	h.ds.install("com.guest.app")
	h.m.OnLockExpired("l1")
	if _, lists, sinces := h.ds.stats(); lists != 2 || len(sinces) != 0 {
		t.Errorf("after l1's end: lists = %d, sinces = %v; want 2 and none", lists, sinces)
	}
	h.m.OnLockExpired("l2")
	if got, want := h.installed(), []string{"com.other.owner.hand", "com.owner.hand"}; !slices.Equal(got, want) {
		t.Errorf("installed = %q, want %q", got, want)
	}
}

// A lock local to this proxy can be relayed too (the stop step); its reset is
// a local one, against the baseline, whatever the relay's snapshot says.
func TestLocalPortWithRelayResetsFromTheBaseline(t *testing.T) {
	h := snapshotHarness(t, "com.owner.hand")
	if _, err := h.m.CreateScopedPort("l1", map[string]struct{}{"LOCAL1": {}}, nil); err != nil {
		t.Fatal(err)
	}
	h.relay("LOCAL1", "l1")
	h.m.OnLockExpired("l1")
	if _, lists, sinces := h.ds.stats(); lists != 1 || len(sinces) != 1 || sinces[0] != nil {
		t.Errorf("lists = %d, sinces = %v; want one reset with no since", lists, sinces)
	}
}

// The orchestrator gives an owner seconds to join a tunnel and a reset can
// take minutes, so a relay opened during a reset joins first, and only
// reaches the device once the reset is done.
func TestServeRelayJoinsBeforeReleaseResetEnds(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{}
	r := relayRouter(t, f, ds, map[string]string{"LOCAL1": "passthrough:///bufconn-ds"})
	m := NewScopedPortManager(r, nil, 3600)
	mu := m.wifiLock("LOCAL1")
	mu.Lock() // the reset
	go r.ServeRelay(&pb.RelayOpen{TunnelId: "t1", Serial: "LOCAL1", LockId: "l1"})
	waitFor(t, "owner joined during the reset", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.setups) == 1 })
	ds.mu.Lock()
	n := len(ds.setups)
	ds.mu.Unlock()
	if n != 0 {
		mu.Unlock()
		t.Fatal("relay opened the device during its reset")
	}
	mu.Unlock()
	waitFor(t, "device opened after the reset", func() bool { ds.mu.Lock(); defer ds.mu.Unlock(); return len(ds.setups) == 1 })
}

// A tunnel the orchestrator failed during the wait (its lock ended, say) is
// not served when the reset ends: its command must never reach the phone.
func TestServeRelayTunnelFailedDuringResetNeverOpensDevice(t *testing.T) {
	f := newFakeRelayOrchestrator()
	ds := &fakeDeviceSource{}
	r := relayRouter(t, f, ds, map[string]string{"LOCAL1": "passthrough:///bufconn-ds"})
	m := NewScopedPortManager(r, nil, 3600)
	mu := m.wifiLock("LOCAL1")
	mu.Lock() // the reset
	served := make(chan struct{})
	go func() {
		defer close(served)
		r.ServeRelay(&pb.RelayOpen{TunnelId: "t1", Serial: "LOCAL1", LockId: "l1"})
	}()
	waitFor(t, "owner joined during the reset", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.setups) == 1 })
	f.mu.Lock()
	done := f.done["t1"]
	f.mu.Unlock()
	done <- status.Error(codes.Aborted, "lock l1 released")
	time.Sleep(200 * time.Millisecond) // the failure reaches the proxy
	mu.Unlock()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeRelay still waiting after the reset")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.setups) != 0 {
		t.Errorf("failed tunnel reached the device: %v", ds.setups)
	}
}

// Failing to open the device happens after the owner joined, so the holder
// has already been accepted and relay_failed can no longer become a FAIL. The
// adb client gets the same FAIL through the tunnel, then the connection ends.
func TestServeRelayDeviceOpenFailureAfterJoinIsAFail(t *testing.T) {
	f := newFakeRelayOrchestrator()
	r := relayRouter(t, f, &fakeDeviceSource{}, map[string]string{"REMOTE1": "passthrough:///bufconn-ds"})
	r.dsDialer = func(context.Context, string) (net.Conn, error) { return nil, errors.New("source down") }

	client, server := tcpPair(t)
	done := make(chan error, 1)
	go func() { done <- r.RelayToDevice("REMOTE1", "shell:", server) }()
	waitFor(t, "holder setup", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.setups) >= 1 })
	f.mu.Lock()
	s := f.setups[0]
	f.mu.Unlock()
	served := make(chan struct{})
	go func() {
		defer close(served)
		r.ServeRelay(&pb.RelayOpen{TunnelId: s.TunnelId, Serial: s.Serial, InitialCommand: s.InitialCommand, LockId: "l1"})
	}()

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, got)
	}
	if !strings.HasPrefix(string(got), "FAIL") || !strings.Contains(string(got), "source down") {
		t.Errorf("adb client got %q, want a FAIL naming the cause", got)
	}
	client.Close()
	if err := <-done; err != nil {
		t.Errorf("RelayToDevice: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeRelay did not return after the holder left")
	}
}
