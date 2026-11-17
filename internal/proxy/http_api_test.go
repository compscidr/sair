package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compscidr/sair/internal/version"
	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestHTTPApiAuthRequired(t *testing.T) {
	// Create an HTTPApi with a known API key
	api := &HTTPApi{
		apiKey: "test-key-123",
	}

	tests := []struct {
		name       string
		method     string
		path       string
		apiKey     string
		wantStatus int
	}{
		{"acquire no key", "POST", "/acquire", "", http.StatusUnauthorized},
		{"acquire wrong key", "POST", "/acquire", "wrong-key", http.StatusUnauthorized},
		{"release no key", "POST", "/release", "", http.StatusUnauthorized},
		{"status no key", "GET", "/status", "", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.apiKey != "" {
				req.Header.Set("x-api-key", tt.apiKey)
			}
			w := httptest.NewRecorder()

			switch {
			case tt.path == "/acquire":
				api.handleAcquire(w, req)
			case tt.path == "/release":
				api.handleRelease(w, req)
			case tt.path == "/status":
				api.handleStatus(w, req)
			}

			if w.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", w.Code, tt.wantStatus)
			}

			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["error"] == "" {
				t.Error("expected error in response body")
			}
		})
	}
}

func TestHTTPApiAcquireCountValidation(t *testing.T) {
	// Validation happens before the scoped port manager is touched, so a bare
	// HTTPApi is enough to exercise these paths.
	api := &HTTPApi{
		apiKey: "test-key",
	}

	tests := []struct {
		name      string
		query     string
		wantError string
	}{
		{"non-numeric count", "?count=abc", "count parameter must be a non-negative integer"},
		{"negative count", "?count=-1", "count parameter must be a non-negative integer"},
		{"count with serial", "?count=1&serial=DEVICE_A", "count and serial parameters are mutually exclusive"},
		{"non-numeric priority", "?priority=high", "priority parameter must be a 32-bit integer"},
		{"oversized priority", "?priority=2147483648", "priority parameter must be a 32-bit integer"},
		{"bad same_proxy", "?same_proxy=maybe", "same_proxy parameter must be true or false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/acquire"+tt.query, nil)
			req.Header.Set("x-api-key", "test-key")
			w := httptest.NewRecorder()

			api.handleAcquire(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}

			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response body is not JSON (%v): %s", err, w.Body.String())
			}
			if resp["error"] != tt.wantError {
				t.Errorf("got error %q, want %q", resp["error"], tt.wantError)
			}
		})
	}
}

func TestHTTPApiAcquireForwardsPriority(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int32
	}{
		{"omitted defaults to zero", "", 0},
		{"negative", "?priority=-1", -1},
		{"positive", "?priority=5", 5},
		{"same_proxy alongside", "?priority=5&same_proxy=true", 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The orchestrator refuses the lock, so no scoped port is opened; we
			// only care what the request carried.
			fake := &fakeOrchClient{err: status.Error(codes.ResourceExhausted, "busy")}
			router := &CommandRouter{orchClient: fake, apiKey: "test-key"}
			api := &HTTPApi{apiKey: "test-key", scopedPortManager: NewScopedPortManager(router, nil, 0)}

			req := httptest.NewRequest("POST", "/acquire"+tt.query, nil)
			req.Header.Set("x-api-key", "test-key")
			api.handleAcquire(httptest.NewRecorder(), req)

			if fake.lastAcquire == nil {
				t.Fatal("acquire never reached the orchestrator")
			}
			if got := fake.lastAcquire.Priority; got != tt.want {
				t.Errorf("got priority %d, want %d", got, tt.want)
			}
			if got, want := fake.lastAcquire.SameProxy, strings.Contains(tt.query, "same_proxy=true"); got != want {
				t.Errorf("got same_proxy %v, want %v", got, want)
			}
		})
	}
}

func TestHTTPApiReleaseMissingLockID(t *testing.T) {
	api := &HTTPApi{
		apiKey: "test-key",
	}

	req := httptest.NewRequest("POST", "/release", nil)
	req.Header.Set("x-api-key", "test-key")
	w := httptest.NewRecorder()

	api.handleRelease(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "lock_id parameter required" {
		t.Errorf("got error %q, want %q", resp["error"], "lock_id parameter required")
	}
}

func TestHTTPStatusForAcquireError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"quota", status.Error(codes.ResourceExhausted, "Daily job limit reached: 10 of 10 jobs used today"), http.StatusTooManyRequests, "Daily job limit reached: 10 of 10 jobs used today"},
		{"bad request", status.Error(codes.InvalidArgument, "count and serials are mutually exclusive"), http.StatusBadRequest, "count and serials are mutually exclusive"},
		{"forbidden", status.Error(codes.PermissionDenied, "nope"), http.StatusForbidden, "nope"},
		{"timeout", status.Error(codes.DeadlineExceeded, "deadline"), http.StatusGatewayTimeout, "deadline"},
		{"wifi unmanaged", status.Error(codes.FailedPrecondition, "WIFI_SSID is not set"), http.StatusNotImplemented, "WIFI_SSID is not set"},
		{"old device source", status.Error(codes.Unimplemented, "unknown method SetWifi"), http.StatusNotImplemented, "unknown method SetWifi"},
		{"relayed device", status.Error(codes.NotFound, "device R is not attached to this proxy"), http.StatusNotFound, "device R is not attached to this proxy"},
		{"other grpc", status.Error(codes.Unavailable, "lock manager not configured"), http.StatusInternalServerError, "lock manager not configured"},
		{"plain error", errors.New("boom"), http.StatusInternalServerError, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, msg := httpStatusForAcquireError(tt.err)
			if code != tt.wantCode || msg != tt.wantMsg {
				t.Errorf("got (%d, %q), want (%d, %q)", code, msg, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

func TestHTTPApiGuestKeys(t *testing.T) {
	api := &HTTPApi{apiKey: "owner-key", guestKeys: []string{"guest-a", "guest-b"}}

	tests := []struct {
		name   string
		key    string
		wantOK bool
	}{
		{"owner key", "owner-key", true},
		{"first guest key", "guest-a", true},
		{"second guest key", "guest-b", true},
		{"unknown key", "someone-else", false},
		{"empty key", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/status", nil)
			req.Header.Set("x-api-key", tt.key)
			if got := api.requireAuth(req); got != tt.wantOK {
				t.Errorf("requireAuth = %v, want %v", got, tt.wantOK)
			}
		})
	}
}

// A key that is neither the owner's nor a listed guest is checked with the
// orchestrator with the guest keys (sair#120, sair#131): a key on the owner's
// or a guest's account is let in, any other is refused, and both answers are cached so repeats cost no round trip.
// An orchestrator error (down, or one that predates CheckKey) refuses the
// request without caching, so a transient outage does not lock a key out.
func TestHTTPApiAsksOrchestratorAboutUnlistedKeys(t *testing.T) {
	calls := map[string]int{}
	answers := map[string]bool{"sibling-key": true}
	var checkErr error
	api := &HTTPApi{apiKey: "owner-key", guestKeys: []string{"guest-a"}, checkKey: func(key string, guestKeys []string) (bool, error) {
		if !slices.Equal(guestKeys, []string{"guest-a"}) {
			t.Errorf("guest keys not passed along: %v", guestKeys)
		}
		calls[key]++
		return answers[key], checkErr
	}}
	auth := func(key string) bool {
		req := httptest.NewRequest("GET", "/status", nil)
		req.Header.Set("x-api-key", key)
		return api.requireAuth(req)
	}

	if !auth("sibling-key") || !auth("sibling-key") {
		t.Error("a key on the owner's account was refused")
	}
	if auth("stranger") || auth("stranger") {
		t.Error("a key on another account was accepted")
	}
	if calls["sibling-key"] != 1 || calls["stranger"] != 1 {
		t.Errorf("answers not cached: calls = %v", calls)
	}
	if auth("owner-key") && auth("guest-a") && auth("") && calls["owner-key"]+calls["guest-a"]+calls[""] != 0 {
		t.Errorf("listed or empty keys asked the orchestrator: calls = %v", calls)
	}
	if auth("") {
		t.Error("empty key was accepted")
	}

	checkErr = status.Error(codes.Unimplemented, "unknown method CheckKey")
	if auth("another") || auth("another") {
		t.Error("a key was accepted while the orchestrator could not answer")
	}
	if calls["another"] != 2 {
		t.Errorf("an error was cached: calls = %v", calls)
	}
	checkErr = nil
	api.keyCache["sibling-key"] = keyAnswer{ok: true, until: time.Now().Add(-time.Second)}
	if !auth("sibling-key") || calls["sibling-key"] != 2 {
		t.Errorf("an expired answer was not asked again: calls = %v", calls)
	}
}

// A guest key is forwarded on the lock RPC so the orchestrator can attribute
// the lock to the guest tenant; the owner's own key is never forwarded, which
// keeps the request identical to what older proxies send.
func TestHTTPApiAcquireForwardsGuestKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"owner key sends empty tenant_api_key", "owner-key", ""},
		{"guest key is forwarded", "guest-a", "guest-a"},
		{"another key on the owner's account is forwarded", "sibling-key", "sibling-key"},
		{"another key on a guest's account is forwarded", "guest-sibling", "guest-sibling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeOrchClient{err: status.Error(codes.ResourceExhausted, "busy")}
			router := &CommandRouter{orchClient: fake, apiKey: "owner-key"}
			api := &HTTPApi{apiKey: "owner-key", guestKeys: []string{"guest-a"}, scopedPortManager: NewScopedPortManager(router, nil, 0), checkKey: router.CheckKey}

			req := httptest.NewRequest("POST", "/acquire", nil)
			req.Header.Set("x-api-key", tt.key)
			api.handleAcquire(httptest.NewRecorder(), req)

			if fake.lastAcquire == nil {
				t.Fatal("acquire never reached the orchestrator")
			}
			if got := fake.lastAcquire.TenantApiKey; got != tt.want {
				t.Errorf("got tenant_api_key %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHTTPApiStatusReportsVersion(t *testing.T) {
	api := &HTTPApi{apiKey: "test-key", scopedPortManager: NewScopedPortManager(&CommandRouter{}, nil, 0)}
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("x-api-key", "test-key")
	w := httptest.NewRecorder()
	api.handleStatus(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["version"] != version.Version {
		t.Errorf("version = %v, want %q", resp["version"], version.Version)
	}
}

func TestHTTPApiDeviceReportCarriesAccountTypes(t *testing.T) {
	tracker := newTestTracker()
	api := &HTTPApi{apiKey: "test-key", deviceListTracker: tracker}
	body := `{"source_addr":"src","devices":[{"serial":"SER","account_types":["com.google"]}]}`
	req := httptest.NewRequest("POST", "/internal/devices", strings.NewReader(body))
	req.Header.Set("x-api-key", "test-key")
	api.handleRegisterDevices(httptest.NewRecorder(), req)

	devices := tracker.GetDevices()
	if len(devices) != 1 || !slices.Equal(devices[0].AccountTypes, []string{"com.google"}) {
		t.Errorf("devices = %v", devices)
	}
}

// The device source's devicesource.WifiStats decodes straight into the
// orchestrator's copy of the message (sair#96): every field must survive.
func TestHTTPApiDeviceReportCarriesWifiStats(t *testing.T) {
	tracker := newTestTracker()
	api := &HTTPApi{apiKey: "test-key", deviceListTracker: tracker}
	sent := &dspb.WifiStats{Joins: 41, Failures: 3, FailuresByReason: map[string]int32{"not_associated": 3}, LastFailure: "why",
		LastFailureMs: 1, LastJoinMs: 2, MedianJoinMs: 3, SlowestJoinMs: 4, TimeoutMs: 5, Hint: "move it"}
	body, _ := json.Marshal(map[string]any{"source_addr": "src", "devices": []any{map[string]any{"serial": "SER", "wifi": sent}}})
	req := httptest.NewRequest("POST", "/internal/devices", strings.NewReader(string(body)))
	req.Header.Set("x-api-key", "test-key")
	api.handleRegisterDevices(httptest.NewRecorder(), req)

	devices := tracker.GetDevices()
	if len(devices) != 1 || devices[0].Wifi == nil {
		t.Fatalf("devices = %v", devices)
	}
	got, _ := json.Marshal(devices[0].Wifi)
	want, _ := json.Marshal(sent)
	if string(got) != string(want) {
		t.Errorf("wifi = %s, want %s", got, want)
	}
}

// A lock's device token works on /wifi only, only for that lock, only with a
// serial, and only while the lock lives (sair#83).
func TestHTTPApiWifiDeviceToken(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var calls []string
	m.setWifi = func(serial string, state dspb.WifiRequest_State, _ string) error {
		calls = append(calls, serial+" "+state.String())
		return nil
	}
	m.removeReverse = func(string) error { return nil }
	sp, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &HTTPApi{apiKey: "test-key", scopedPortManager: m}
	post := func(path, token string) int {
		req := httptest.NewRequest("POST", path, nil)
		if token != "" {
			req.Header.Set("x-device-token", token)
		}
		w := httptest.NewRecorder()
		api.handleWifi(w, req)
		return w.Code
	}
	wifi := "/wifi?lock_id=L1&state=on"
	if got := post(wifi+"&serial=A", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if got := post(wifi, sp.DeviceToken); got != http.StatusBadRequest {
		t.Errorf("no serial: %d", got)
	}
	if got := post(wifi+"&serial=B", sp.DeviceToken); got != http.StatusForbidden {
		t.Errorf("serial outside lock: %d", got)
	}
	if got := post(wifi+"&serial=A", sp.DeviceToken); got != http.StatusOK {
		t.Errorf("own serial: %d", got)
	}
	if got := post("/wifi?state=on&serial=A", sp.DeviceToken); got != http.StatusBadRequest {
		t.Errorf("no lock_id: %d", got)
	}
	req := httptest.NewRequest("POST", "/acquire", nil)
	req.Header.Set("x-device-token", sp.DeviceToken)
	w := httptest.NewRecorder()
	api.handleAcquire(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("token on /acquire: %d", w.Code)
	}
	m.CloseScopedPort("L1")
	if got := post(wifi+"&serial=A", sp.DeviceToken); got != http.StatusUnauthorized {
		t.Errorf("after release: %d", got)
	}
	if want := []string{"A ON", "A RESET", "A ON"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// /wifi passes band through to the device source and refuses one it would
// not take, or one with state off (sair#143).
func TestHTTPApiWifiBand(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var bands []string
	m.setWifi = func(_ string, _ dspb.WifiRequest_State, band string) error { bands = append(bands, band); return nil }
	if _, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil); err != nil {
		t.Fatal(err)
	}
	api := &HTTPApi{apiKey: "test-key", scopedPortManager: m}
	for path, want := range map[string]int{
		"/wifi?lock_id=L1&state=on&band=2.4": http.StatusOK,
		"/wifi?lock_id=L1&state=on&band=60":  http.StatusBadRequest,
		"/wifi?lock_id=L1&state=off&band=5":  http.StatusBadRequest,
	} {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("x-api-key", "test-key")
		w := httptest.NewRecorder()
		api.handleWifi(w, req)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", path, w.Code, want)
		}
	}
	if !slices.Equal(bands, []string{"2.4"}) {
		t.Errorf("bands sent = %q, want [2.4]", bands)
	}
}

func TestParseLabels(t *testing.T) {
	got, err := parseLabels([]string{"os=14", "39111JEHN16292:role=group-owner", "192.168.1.5:5555:role=a=b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []*pb.Label{{Key: "os", Value: "14"}, {Serial: "39111JEHN16292", Key: "role", Value: "group-owner"}, {Serial: "192.168.1.5:5555", Key: "role", Value: "a=b"}}
	if !slices.EqualFunc(got, want, func(a, b *pb.Label) bool { return proto.Equal(a, b) }) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range [][]string{{"novalue"}, {"=v"}, {"S:=v"}, {":k=v"}, {strings.Repeat("k", 65) + "=v"}, {"k=" + strings.Repeat("v", 257)}, slices.Repeat([]string{"k=v"}, 33)} {
		if _, err := parseLabels(bad); err == nil {
			t.Errorf("%.40q: want an error", bad)
		}
	}
}

func TestHTTPApiAcquireForwardsJob(t *testing.T) {
	fake := &fakeOrchClient{err: status.Error(codes.ResourceExhausted, "busy")}
	api := &HTTPApi{apiKey: "test-key", scopedPortManager: NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "test-key"}, nil, 0)}
	req := httptest.NewRequest("POST", "/acquire?repo=o/r&job=test&run_attempt=2&sha=abc&branch=main&workflow=CI&stage=api-34&label=os=14&label=S1:role=go", nil)
	req.Header.Set("x-api-key", "test-key")
	api.handleAcquire(httptest.NewRecorder(), req)

	want := &pb.AcquireLockRequest{Repo: "o/r", Job: "test", RunAttempt: 2, Sha: "abc", Branch: "main", Workflow: "CI", Stage: "api-34",
		Labels: []*pb.Label{{Key: "os", Value: "14"}, {Serial: "S1", Key: "role", Value: "go"}}}
	got := fake.lastAcquire
	got.ProxyId, got.Priority = "", 0
	if !proto.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	req = httptest.NewRequest("POST", "/acquire?run_attempt=x", nil)
	req.Header.Set("x-api-key", "test-key")
	w := httptest.NewRecorder()
	api.handleAcquire(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad run_attempt: %d", w.Code)
	}
}

func TestHTTPApiLabel(t *testing.T) {
	fake := &fakeOrchClient{}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "test-key"}, nil, 3600)
	m.removeReverse = func(string) error { return nil }
	if _, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil); err != nil {
		t.Fatal(err)
	}
	api := &HTTPApi{apiKey: "test-key", scopedPortManager: m}
	post := func(query string) int {
		req := httptest.NewRequest("POST", "/label"+query, nil)
		req.Header.Set("x-api-key", "test-key")
		w := httptest.NewRecorder()
		api.handleLabel(w, req)
		return w.Code
	}
	for query, want := range map[string]int{
		"?label=k=v":                  http.StatusBadRequest,
		"?lock_id=L1":                 http.StatusBadRequest,
		"?lock_id=L1&label=bad":       http.StatusBadRequest,
		"?lock_id=nope&label=k=v":     http.StatusNotFound,
		"?lock_id=L1&label=B:role=x":  http.StatusForbidden,
		"?lock_id=L1&label=A:role=go": http.StatusOK,
	} {
		if got := post(query); got != want {
			t.Errorf("%s: got %d, want %d", query, got, want)
		}
	}
	if got := fake.lastLabels; got.GetLockId() != "L1" || len(got.GetLabels()) != 1 || got.Labels[0].Serial != "A" {
		t.Errorf("orchestrator got %v", got)
	}
}

// /bluetooth takes the device token like /wifi (sair#156), restarts the
// stack on the named phone, and refuses any action but restart.
func TestHTTPApiBluetoothDeviceToken(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var calls []string
	m.resetBluetooth = func(serial string) error { calls = append(calls, serial); return nil }
	sp, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &HTTPApi{apiKey: "test-key", scopedPortManager: m}
	post := func(path, token string) int {
		req := httptest.NewRequest("POST", path, nil)
		if token != "" {
			req.Header.Set("x-device-token", token)
		} else {
			req.Header.Set("x-api-key", "test-key")
		}
		w := httptest.NewRecorder()
		api.handleBluetooth(w, req)
		return w.Code
	}
	bt := "/bluetooth?lock_id=L1&action=restart"
	if got := post(bt+"&serial=A", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if got := post(bt, sp.DeviceToken); got != http.StatusBadRequest {
		t.Errorf("token without serial: %d", got)
	}
	if got := post(bt+"&serial=B", sp.DeviceToken); got != http.StatusForbidden {
		t.Errorf("serial outside lock: %d", got)
	}
	if got := post("/bluetooth?lock_id=L1&serial=A", sp.DeviceToken); got != http.StatusBadRequest {
		t.Errorf("no action: %d", got)
	}
	if got := post(bt+"&serial=A", sp.DeviceToken); got != http.StatusOK {
		t.Errorf("own serial: %d", got)
	}
	if got := post(bt, ""); got != http.StatusOK {
		t.Errorf("api key, all devices: %d", got)
	}
	if got := post("/bluetooth?lock_id=L2&action=restart", ""); got != http.StatusNotFound {
		t.Errorf("unknown lock: %d", got)
	}
	if want := []string{"A", "A"}; !slices.Equal(calls, want) {
		t.Errorf("resets = %q, want %q", calls, want)
	}
}

// A lock this proxy no longer holds (it restarted mid-job) answers with the
// orchestrator's reason, not a bare unknown lock_id (sair#167).
func TestHTTPApiReleaseSaysWhyTheLockEnded(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"ended", status.Error(codes.NotFound, "ended: the proxy restarted"), "ended: the proxy restarted"},
		{"orchestrator unreachable", status.Error(codes.Unavailable, "down"), "unknown lock_id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			router := &CommandRouter{orchClient: &fakeOrchClient{releaseErr: tt.err}, apiKey: "test-key"}
			api := &HTTPApi{apiKey: "test-key", scopedPortManager: NewScopedPortManager(router, nil, 0)}
			req := httptest.NewRequest("POST", "/release?lock_id=gone", nil)
			req.Header.Set("x-api-key", "test-key")
			w := httptest.NewRecorder()
			api.handleRelease(w, req)

			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp)
			if w.Code != http.StatusNotFound || resp["error"] != tt.want {
				t.Errorf("got %d %q, want 404 %q", w.Code, resp["error"], tt.want)
			}
		})
	}
}
