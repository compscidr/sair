package proxy

import (
	"net"
	"strings"
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// hostRequest drives one host command against a connection that can see the
// given local (tracker) and remote (lock-granted) devices, returning the reply.
func hostRequest(t *testing.T, tracker *DeviceListTracker, allowed map[string]struct{}, remote map[string]*pb.DeviceInfo, request string) string {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	c := NewAdbConnection(server, nil, tracker, allowed, nil, remote)
	go func() { c.handleHostCommand(request); server.Close() }()
	buf := make([]byte, 4096)
	var out []byte
	for {
		n, err := client.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(out)
}

func TestRemoteDevicesAreListedWithOwnerInfoAndFeatures(t *testing.T) {
	tracker := newTestTracker()
	tracker.UpdateDevices("src", []*pb.DeviceInfo{{Serial: "LOCAL1", Model: "Pixel 8", Sdk: 34}})
	allowed := map[string]struct{}{"LOCAL1": {}, "REMOTE1": {}}
	remote := map[string]*pb.DeviceInfo{"REMOTE1": {Serial: "REMOTE1", Model: "Galaxy S24", Manufacturer: "Samsung", Sdk: 17}}

	short := hostRequest(t, tracker, allowed, remote, "host:devices")
	if !strings.Contains(short, "LOCAL1\tdevice") || !strings.Contains(short, "REMOTE1\tdevice") {
		t.Errorf("both devices should be listed, got %q", short)
	}
	long := hostRequest(t, tracker, allowed, remote, "host:devices-l")
	if !strings.Contains(long, "REMOTE1") || !strings.Contains(long, "Galaxy_S24") {
		t.Errorf("remote device should carry the owner's model, got %q", long)
	}
	// Remote API 17 device pulls host:features down to the legacy set.
	if f := hostRequest(t, tracker, allowed, remote, "host:features"); strings.Contains(f, "shell_v2") {
		t.Errorf("an API 17 remote device is visible, so shell_v2 must not be advertised, got %q", f)
	}
	if f := hostRequest(t, tracker, allowed, remote, "host-serial:REMOTE1:features"); !strings.HasPrefix(f, "OKAY") {
		t.Errorf("per-serial features for a remote device should be OKAY, got %q", f)
	}
	// A remote serial not in the lock is not visible even if in the remote map.
	if f := hostRequest(t, tracker, map[string]struct{}{"LOCAL1": {}}, remote, "host-serial:REMOTE1:features"); !strings.HasPrefix(f, "FAIL") {
		t.Errorf("remote device outside allowed set must be refused, got %q", f)
	}
}

func TestScopedPortAssignsStableTransportIdsToRemoteDevices(t *testing.T) {
	sp := &ScopedPort{RemoteDevices: map[string]*pb.DeviceInfo{"R1": {Serial: "R1"}, "R2": {Serial: "R2"}}}
	a, b := sp.remoteTransportID("R1"), sp.remoteTransportID("R2")
	if a == b || a < 1_000_000 || b < 1_000_000 {
		t.Errorf("remote transport ids must be distinct and in the high range, got %d %d", a, b)
	}
	if sp.remoteTransportID("R1") != a {
		t.Errorf("transport id must be stable for the port's lifetime")
	}
	if sp.remoteTransportID("NOPE") != 0 {
		t.Errorf("unknown serial has no transport id")
	}
}

func TestAcquireLockCopiesRemoteDevices(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{
		LockId: "lock-1", Serials: []string{"LOCAL1", "REMOTE1"},
		RemoteDevices: []*pb.DeviceInfo{{Serial: "REMOTE1", Model: "Galaxy"}},
	}}
	router := &CommandRouter{orchClient: fake, apiKey: "k", proxyID: "host-a"}
	res, err := router.AcquireLock(nil, 2, 30, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RemoteDevices) != 1 || res.RemoteDevices["REMOTE1"].Model != "Galaxy" {
		t.Errorf("remote devices not carried: %+v", res.RemoteDevices)
	}
	if _, ok := res.Serials["REMOTE1"]; !ok {
		t.Errorf("remote serial must also be in Serials (it is granted)")
	}
}
