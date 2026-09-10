package proxy

import (
	"net"
	"strings"
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// featuresConn builds a connection over a pipe and returns the reply to one
// host request.
func featuresRequest(t *testing.T, tracker *DeviceListTracker, allowed map[string]struct{}, request string) string {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	c := NewAdbConnection(server, nil, tracker, allowed)
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

func TestFeaturesRespectVisibilityAndSdk(t *testing.T) {
	tracker := newTestTracker()
	tracker.UpdateDevices("src", []*pb.DeviceInfo{
		{Serial: "OLD", Sdk: 17},
		{Serial: "NEW", Sdk: 34},
		{Serial: "UNK", Sdk: 0},
	})
	onlyOld := map[string]struct{}{"OLD": {}}

	// Per-serial: visible old device gets no shell_v2.
	if r := featuresRequest(t, tracker, onlyOld, "host-serial:OLD:features"); !strings.HasPrefix(r, "OKAY") || strings.Contains(r, "shell_v2") {
		t.Errorf("OLD should be OKAY without shell_v2, got %q", r)
	}
	// Per-serial: a device this connection cannot see is refused, not described.
	if r := featuresRequest(t, tracker, onlyOld, "host-serial:NEW:features"); !strings.HasPrefix(r, "FAIL") {
		t.Errorf("NEW is not visible on this connection, want FAIL, got %q", r)
	}
	// Unknown API level is treated as modern.
	if r := featuresRequest(t, tracker, map[string]struct{}{"UNK": {}}, "host-serial:UNK:features"); !strings.Contains(r, "shell_v2") {
		t.Errorf("unknown sdk should get the modern set, got %q", r)
	}
	// host:features uses the lowest visible level.
	both := map[string]struct{}{"OLD": {}, "NEW": {}}
	if r := featuresRequest(t, tracker, both, "host:features"); strings.Contains(r, "shell_v2") {
		t.Errorf("host:features with an API 17 device visible must not advertise shell_v2, got %q", r)
	}
	if r := featuresRequest(t, tracker, map[string]struct{}{"NEW": {}}, "host:features"); !strings.Contains(r, "shell_v2") {
		t.Errorf("host:features with only API 34 visible should advertise shell_v2, got %q", r)
	}
	// Bare port (empty allowed set): nothing visible, so the generic modern set.
	if r := featuresRequest(t, tracker, map[string]struct{}{}, "host:features"); !strings.HasPrefix(r, "OKAY") {
		t.Errorf("bare port host:features should still answer, got %q", r)
	}
}
