package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/compscidr/sair/internal/devicesource"
	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeAdbServer answers host:transport and the reverse: services the way the
// real adb server does, keeping the forwards in a set.
type fakeAdbServer struct {
	mu       sync.Mutex
	forwards map[string]bool
}

func (f *fakeAdbServer) serve(c net.Conn) {
	defer c.Close()
	if req, err := ReadRequest(c); err != nil || !strings.HasPrefix(req, "host:transport:") {
		WriteFail(c, "expected transport")
		return
	}
	WriteOkay(c)
	req, err := ReadRequest(c)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(req, "reverse:forward:"):
		f.forwards[strings.TrimPrefix(req, "reverse:forward:")] = true
		io.WriteString(c, "OKAYOKAY")
	case req == "reverse:list-forward":
		var sb strings.Builder
		for fw := range f.forwards {
			sb.WriteString("host " + strings.ReplaceAll(fw, ";", " ") + "\n")
		}
		WriteOkayWithPayload(c, sb.String())
	case strings.HasPrefix(req, "reverse:killforward:"):
		delete(f.forwards, strings.TrimPrefix(req, "reverse:killforward:"))
		io.WriteString(c, "OKAYOKAY")
	case req == "reverse:killforward-all":
		clear(f.forwards)
		WriteOkay(c)
	default:
		WriteFail(c, "unknown service "+req)
	}
}

func (f *fakeAdbServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forwards)
}

// adbService runs one device service on serial through the ADB port at addr
// and returns everything the server replied after the transport's OKAY.
func adbService(t *testing.T, addr, serial, service string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%04x%s", len("host:transport:"+serial), "host:transport:"+serial)
	okay := make([]byte, 4)
	if _, err := io.ReadFull(c, okay); err != nil || string(okay) != "OKAY" {
		t.Fatalf("transport: %q %v", okay, err)
	}
	fmt.Fprintf(c, "%04x%s", len(service), service)
	reply, _ := io.ReadAll(c)
	return string(reply)
}

// adb reverse add/list/remove reaches the real adb server unchanged through a
// scoped port and the device source, and closing the port removes leftovers.
func TestAdbReverseThroughScopedPort(t *testing.T) {
	adb := &fakeAdbServer{forwards: map[string]bool{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go adb.serve(c)
		}
	}()

	t.Setenv("ADB_PORT", fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	dspb.RegisterDeviceSourceServer(srv, devicesource.NewServer())
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	tracker := newTestTracker()
	tracker.UpdateDevices("passthrough:///bufconn-ds", []*pb.DeviceInfo{{Serial: "SER"}})
	r := &CommandRouter{
		dsDialer:      func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) },
		ResolveSource: tracker.GetSourceAddr,
	}
	m := NewScopedPortManager(r, tracker, 3600)
	m.setWifi = func(string, dspb.WifiRequest_State) error { return status.Error(codes.FailedPrecondition, "unmanaged") }
	sp, err := m.CreateScopedPort("L1", map[string]struct{}{"SER": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", sp.Port)

	for _, port := range []string{"9000", "9001"} {
		if got := adbService(t, addr, "SER", "reverse:forward:tcp:"+port+";tcp:"+port); got != "OKAYOKAY" {
			t.Fatalf("forward %s: %q", port, got)
		}
	}
	if got := adbService(t, addr, "SER", "reverse:list-forward"); !strings.Contains(got, "tcp:9000 tcp:9000") {
		t.Errorf("list-forward = %q", got)
	}
	if got := adbService(t, addr, "SER", "reverse:killforward:tcp:9000;tcp:9000"); got != "OKAYOKAY" {
		t.Errorf("killforward = %q", got)
	}
	if adb.count() != 1 {
		t.Fatalf("forwards before release = %d, want 1", adb.count())
	}

	m.CloseScopedPort("L1")
	if adb.count() != 0 {
		t.Errorf("forwards after release = %d, want 0", adb.count())
	}
}

// A relayed device's reverse would tunnel to the owning proxy's host, so it is
// refused before anything is relayed.
func TestAdbReverseRefusedOnRelayedDevice(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	c := NewAdbConnection(server, &CommandRouter{}, newTestTracker(), nil, nil, map[string]*pb.DeviceInfo{"R": {Serial: "R"}})
	go func() { c.handleHostCommand("host:transport:R"); server.Close() }()

	okay := make([]byte, 4)
	if _, err := io.ReadFull(client, okay); err != nil || string(okay) != "OKAY" {
		t.Fatalf("transport: %q %v", okay, err)
	}
	fmt.Fprintf(client, "%04x%s", len("reverse:list-forward"), "reverse:list-forward")
	reply, _ := io.ReadAll(client)
	if !strings.HasPrefix(string(reply), "FAIL") || !strings.Contains(string(reply), "relayed from another proxy") {
		t.Errorf("reply = %q", reply)
	}
}
