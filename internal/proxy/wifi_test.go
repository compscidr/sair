package proxy

import (
	"errors"
	"reflect"
	"testing"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
)

func TestScopedPortWifi(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var calls []string
	m.setWifi = func(serial string, state dspb.WifiRequest_State) error {
		calls = append(calls, serial+" "+state.String())
		return nil
	}
	m.removeReverse = func(serial string) error {
		calls = append(calls, serial+" unreverse")
		return nil
	}
	set := func(s ...string) map[string]struct{} {
		out := map[string]struct{}{}
		for _, v := range s {
			out[v] = struct{}{}
		}
		return out
	}
	if _, err := m.CreateScopedPort("L1", set("A", "R"), map[string]*pb.DeviceInfo{"R": {Serial: "R"}}); err != nil {
		t.Fatal(err)
	}

	// "All" skips the relayed device; naming a serial outside the lock is refused.
	if err := m.SetWifi("L1", nil, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetWifi("L1", []string{"B"}, false); !errors.Is(err, errNotInLock) {
		t.Errorf("serial outside lock: err = %v", err)
	}
	if err := m.SetWifi("nope", nil, false); !errors.Is(err, errUnknownLock) {
		t.Errorf("unknown lock: err = %v", err)
	}

	// L1 expired and L2 already holds A: closing L1 must not reset under L2.
	if _, err := m.CreateScopedPort("L2", set("A"), nil); err != nil {
		t.Fatal(err)
	}
	m.CloseScopedPort("L1")
	m.CloseScopedPort("L2")

	want := []string{"A ON", "A unreverse", "A RESET"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}
