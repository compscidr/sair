package proxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestScopedPortWifi(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var calls []string
	m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
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
	if err := m.SetWifi("L1", nil, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.SetWifi("L1", []string{"B"}, false, ""); !errors.Is(err, errNotInLock) {
		t.Errorf("serial outside lock: err = %v", err)
	}
	if err := m.SetWifi("nope", nil, false, ""); !errors.Is(err, errUnknownLock) {
		t.Errorf("unknown lock: err = %v", err)
	}

	// L1 expired and L2 already holds A: closing L1 must not reset under L2.
	if _, err := m.CreateScopedPort("L2", set("A"), nil); err != nil {
		t.Fatal(err)
	}
	m.CloseScopedPort("L1")
	m.CloseScopedPort("L2")

	want := []string{"A ON", "A unreverse", "A RESET", "A ON"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// Two phones in one lock change state in parallel: each setWifi waits at a
// barrier for the other to arrive, which only clears when the calls overlap
// (sair#84). The same phone still serializes, so ON then OFF stays ordered.
func TestScopedPortWifiFansOutPerDevice(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var barrier sync.WaitGroup
	barrier.Add(2)
	m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
		barrier.Done()
		barrier.Wait()
		return nil
	}
	if _, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}, "B": {}}, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.SetWifi("L1", nil, true, "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetWifi serialized the two devices")
	}

	// Same serial: the second call must wait for the first.
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex
	m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
		if state == dspb.WifiRequest_ON {
			<-release
		}
		mu.Lock()
		order = append(order, state.String())
		mu.Unlock()
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- m.SetWifi("L1", []string{"A"}, true, "") }()
	time.Sleep(50 * time.Millisecond) // let the ON take A's lock before the OFF asks for it
	second := make(chan error, 1)
	go func() { second <- m.SetWifi("L1", []string{"A"}, false, "") }()
	select {
	case <-second:
		t.Fatal("OFF on A ran while ON on A was still in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-first
	<-second
	if want := []string{"ON", "OFF"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// Acquire mints a device token and reverse-forwards the HTTP API onto each
// local phone (not the relayed one), so a test on the phone can reach /wifi
// (sair#83). A phone that refuses the forward leaves the port unset.
func TestAcquireSetsUpDeviceAPI(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{
		LockId:        "L1",
		Serials:       []string{"A", "R"},
		RemoteDevices: []*pb.DeviceInfo{{Serial: "R"}},
	}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	m.DeviceAPIPort = 8550
	m.setWifi = func(string, dspb.WifiRequest_State, string) error { return nil } // acquire verifies OFF too (sair#86)
	var forwards []string
	m.addReverse = func(serial string, port int) error {
		forwards = append(forwards, fmt.Sprintf("%s:%d", serial, port))
		return nil
	}
	sp, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.DeviceToken) != 32 || sp.DeviceAPIPort != 8550 {
		t.Errorf("token %q port %d", sp.DeviceToken, sp.DeviceAPIPort)
	}
	if want := []string{"A:8550"}; !reflect.DeepEqual(forwards, want) {
		t.Errorf("forwards = %v, want %v", forwards, want)
	}
	if !m.DeviceTokenValid("L1", sp.DeviceToken) || m.DeviceTokenValid("L1", "nope") || m.DeviceTokenValid("L2", sp.DeviceToken) {
		t.Error("DeviceTokenValid")
	}
	m.CloseScopedPort("L1")
	if m.DeviceTokenValid("L1", sp.DeviceToken) {
		t.Error("token outlived the lock")
	}

	m.addReverse = func(string, int) error { return errors.New("cannot bind") }
	fake.resp.LockId = "L2"
	sp, err = m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if sp.DeviceAPIPort != 0 {
		t.Errorf("port = %d after a failed forward, want 0", sp.DeviceAPIPort)
	}
}

// An expired lock's reset that queued behind the next lock's ON on the same
// phone must notice, under the lock, that the phone has a new owner and
// leave it alone: otherwise the new job starts offline with no device API.
func TestResetSkipsPhoneRelockedWhileWaiting(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var mu sync.Mutex
	var calls []string
	release := make(chan struct{})
	m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
		mu.Lock()
		calls = append(calls, serial+" "+state.String())
		mu.Unlock()
		if state == dspb.WifiRequest_ON {
			<-release
		}
		return nil
	}
	m.removeReverse = func(serial string) error {
		mu.Lock()
		calls = append(calls, serial+" unreverse")
		mu.Unlock()
		return nil
	}
	if _, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil); err != nil {
		t.Fatal(err)
	}
	// L1 expired on the orchestrator, which granted A to L2; L2's ON is in
	// flight when L1's expiry callback runs.
	if _, err := m.CreateScopedPort("L2", map[string]struct{}{"A": {}}, nil); err != nil {
		t.Fatal(err)
	}
	on := make(chan error, 1)
	go func() { on <- m.SetWifi("L2", nil, true, "") }()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() { m.OnLockExpired("L1"); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-on
	<-closed

	if want := []string{"A ON"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	m.CloseScopedPort("L2")
}

// Closing a lock uninstalls what the job left before the Wi-Fi reset, and
// the lock log carries what went so the orchestrator shows it (sair#76). An
// older device source (Unimplemented) is simply skipped.
func TestCloseResetsPackagesBeforeWifiAndLogsIt(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var calls []string
	m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
		calls = append(calls, serial+" "+state.String())
		return nil
	}
	m.removeReverse = func(serial string) error {
		calls = append(calls, serial+" unreverse")
		return nil
	}
	m.resetPackages = func(serial string, _ *dspb.PackageSnapshot) ([]string, []string, error) {
		calls = append(calls, serial+" packages")
		if serial == "B" {
			return nil, nil, status.Error(codes.Unimplemented, "old device source")
		}
		return []string{"com.example.app", "com.example.app.test"}, []string{"bench wpa2-psk"}, nil
	}
	sp, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.CloseScopedPort("L1")
	entries := sp.Log.DrainAll()
	if len(entries) != 1 || entries[0].Serial != "A" || entries[0].Service != "sair: uninstalled com.example.app, com.example.app.test (Wi-Fi suggestions removed with them: bench wpa2-psk)" {
		t.Errorf("lock log = %v", entries)
	}

	sp, _ = m.CreateScopedPort("L2", map[string]struct{}{"B": {}}, nil)
	m.CloseScopedPort("L2")
	if entries := sp.Log.DrainAll(); len(entries) != 0 {
		t.Errorf("Unimplemented reset logged %v", entries)
	}
	want := []string{"A unreverse", "A packages", "A RESET", "A ON", "B unreverse", "B packages", "B RESET", "B ON"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// A job that never released leaves its forwards and packages; the next
// acquire cleans them before the Wi-Fi check and says so in its lock log.
func TestAcquireCleansUpWhatAnEarlierJobLeft(t *testing.T) {
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	var calls []string
	m.removeReverse = func(serial string) error { calls = append(calls, "unreverse"); return nil }
	m.resetPackages = func(serial string, _ *dspb.PackageSnapshot) ([]string, []string, error) {
		calls = append(calls, "packages")
		return []string{"com.example.app"}, nil, nil
	}
	m.setWifi = func(_ string, state dspb.WifiRequest_State, _ string) error {
		calls = append(calls, state.String())
		return nil
	}
	sp, err := m.Acquire(context.Background(), nil, 0, nil, 0, false, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"unreverse", "packages", "OFF"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	entries := sp.Log.DrainAll()
	if len(entries) != 1 || entries[0].Service != "sair: uninstalled com.example.app (left by an earlier job)" {
		t.Errorf("lock log = %v", entries)
	}
	m.CloseScopedPort("L1")
}

// A job can restart the Bluetooth stack on its phones mid-job (sair#156):
// each phone is cycled on its own, the lock log says so, a failure is
// returned but the lock is kept, and a wedged radio is never rebooted
// mid-job, that is the release reset's call.
func TestRestartBluetoothMidJob(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var mu sync.Mutex
	var calls []string
	hang := status.Error(codes.DeadlineExceeded, "B Bluetooth not back on within 30s: adapter is OFF")
	m.resetBluetooth = func(serial string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "bluetooth "+serial)
		if serial == "B" {
			return hang
		}
		return nil
	}
	m.rebootIfWedged = func(serial, _ string) (bool, error) { t.Errorf("rebooted %s mid-job", serial); return true, nil }
	sp, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}, "B": {}, "R": {}}, map[string]*pb.DeviceInfo{"R": {Serial: "R"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestartBluetooth("L1", []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartBluetooth("L1", nil); !errors.Is(err, hang) {
		t.Errorf("all: err = %v, want B's failure", err)
	}
	if err := m.RestartBluetooth("L1", []string{"R"}); err != nil {
		t.Errorf("relayed device named: err = %v", err)
	}
	if err := m.RestartBluetooth("L1", []string{"C"}); !errors.Is(err, errNotInLock) {
		t.Errorf("serial outside lock: err = %v", err)
	}
	if err := m.RestartBluetooth("nope", nil); !errors.Is(err, errUnknownLock) {
		t.Errorf("unknown lock: err = %v", err)
	}
	sort.Strings(calls)
	if want := []string{"bluetooth A", "bluetooth A", "bluetooth B", "bluetooth R"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	var lines []string
	for _, e := range sp.Log.DrainAll() {
		if strings.Contains(e.Service, "Bluetooth") {
			lines = append(lines, e.Serial+": "+e.Service)
		}
	}
	sort.Strings(lines)
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "A: sair: Bluetooth cycled off and on (") || !strings.HasSuffix(lines[0], ") (requested by the job)") ||
		lines[2] != "B: sair: Bluetooth reset failed: B Bluetooth not back on within 30s: adapter is OFF (requested by the job)" {
		t.Errorf("lock log Bluetooth lines = %q", lines)
	}
}
