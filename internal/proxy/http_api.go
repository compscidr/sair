package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compscidr/sair/internal/version"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HTTPApi provides the HTTP API for the ADB proxy.
//
// Runners call this API (instead of the orchestrator) to acquire/release
// device locks. The proxy handles all orchestrator communication and
// opens scoped ADB ports for per-runner isolation.
type HTTPApi struct {
	scopedPortManager *ScopedPortManager
	deviceListTracker *DeviceListTracker
	apiKey            string
	// guestKeys are other tenants' API keys the owner lets lock this proxy's
	// devices. The owner's proxy vouches for them: putting a key here is the
	// owner's consent, no portal-side sharing is involved. See README.
	guestKeys []string
	server    *http.Server

	// checkKey asks the orchestrator whether a key is on the owner's account
	// (sair#120) or a guest key's account (sair#131); commandRouter.CheckKey,
	// tests swap it. Answers are cached
	// for keyCacheTTL, misses included, so a bad key is not a round trip per
	// request. An error (orchestrator down, or one that predates the RPC) is
	// a refusal and is not cached.
	checkKey   func(key string, guestKeys []string) (bool, error)
	keyCacheMu sync.Mutex
	keyCache   map[string]keyAnswer
}

type keyAnswer struct {
	ok    bool
	until time.Time
}

// keyCacheTTL matches the orchestrator's own key refresh, so a key revoked
// in the portal stops working here within about a minute.
const keyCacheTTL = 30 * time.Second

func NewHTTPApi(scopedPortManager *ScopedPortManager, deviceListTracker *DeviceListTracker, apiKey string, guestKeys []string, port int, host string) *HTTPApi {
	api := &HTTPApi{
		scopedPortManager: scopedPortManager,
		deviceListTracker: deviceListTracker,
		apiKey:            apiKey,
		guestKeys:         guestKeys,
	}
	if scopedPortManager != nil && scopedPortManager.commandRouter != nil {
		api.checkKey = scopedPortManager.commandRouter.CheckKey
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /acquire", api.handleAcquire)
	mux.HandleFunc("POST /release", api.handleRelease)
	mux.HandleFunc("POST /wifi", api.handleWifi)
	mux.HandleFunc("POST /bluetooth", api.handleBluetooth)
	mux.HandleFunc("POST /label", api.handleLabel)
	mux.HandleFunc("GET /status", api.handleStatus)
	mux.HandleFunc("POST /internal/devices", api.handleRegisterDevices)

	api.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", host, port),
		Handler: mux,
	}

	return api
}

// Start binds the HTTP API port and serves in the background. The bind happens
// synchronously so a port collision is returned to the caller instead of being
// logged from a goroutine after "started" has already been announced.
func (a *HTTPApi) Start() error {
	ln, err := net.Listen("tcp", a.server.Addr)
	if err != nil {
		return err
	}
	go func() {
		slog.Info("proxy HTTP API started", "addr", a.server.Addr)
		if err := a.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP API failed", "error", err)
		}
	}()
	return nil
}

func (a *HTTPApi) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.server.Shutdown(ctx)
	slog.Info("proxy HTTP API stopped")
}

// requireAuth accepts the owner key, any listed guest key, and any other key
// the orchestrator says is on the owner's account or a listed guest's
// account; anything else is refused.
func (a *HTTPApi) requireAuth(r *http.Request) bool {
	key := r.Header.Get("x-api-key")
	if key == a.apiKey || slices.Contains(a.guestKeys, key) {
		return true
	}
	if key == "" || a.checkKey == nil {
		return false
	}

	a.keyCacheMu.Lock()
	defer a.keyCacheMu.Unlock()
	now := time.Now()
	if c, hit := a.keyCache[key]; hit && now.Before(c.until) {
		return c.ok
	}
	// ponytail: the lock is held across the round trip, so concurrent
	// requests with unlisted keys serialize behind it; fine at CI rates.
	ok, err := a.checkKey(key, a.guestKeys)
	if err != nil {
		slog.Warn("could not check api key with the orchestrator", "error", err)
		return false
	}
	if a.keyCache == nil {
		a.keyCache = make(map[string]keyAnswer)
	}
	// ponytail: entries are only evicted on lookup; a flood of distinct bad
	// keys grows the map until the next hit on each. Sweep if that matters.
	a.keyCache[key] = keyAnswer{ok: ok, until: now.Add(keyCacheTTL)}
	return ok
}

// tenantKey is the key to attribute a lock to: empty for the owner (the
// orchestrator session already is the owner), the caller's key otherwise. A
// guest's key (listed, or another on a guest's account) puts the lock under
// the guest's account; another key on the owner's account resolves to the
// owner, so it is attributed the same way.
func (a *HTTPApi) tenantKey(r *http.Request) string {
	if key := r.Header.Get("x-api-key"); key != a.apiKey {
		return key
	}
	return ""
}

// clientIP is the runner's address as this proxy sees it, without the port.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *HTTPApi) handleAcquire(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing x-api-key header"})
		return
	}

	serialParam := r.URL.Query().Get("serial")
	var requestedSerials map[string]struct{}
	if serialParam != "" {
		requestedSerials = make(map[string]struct{})
		for _, s := range strings.Split(serialParam, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				requestedSerials[s] = struct{}{}
			}
		}
		if len(requestedSerials) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial parameter must contain at least one non-empty serial"})
			return
		}
	}

	// count requests that many arbitrary free devices instead of named ones,
	// so it is mutually exclusive with serial. Omitted (or 0) means "all".
	var count int64
	if countParam := r.URL.Query().Get("count"); countParam != "" {
		parsed, err := strconv.ParseInt(countParam, 10, 32)
		if err != nil || parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "count parameter must be a non-negative integer"})
			return
		}
		if parsed > 0 && requestedSerials != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "count and serial parameters are mutually exclusive"})
			return
		}
		count = parsed
	}

	// priority orders waiters when devices are busy: higher first, FIFO within a value.
	var priority int64
	if p := r.URL.Query().Get("priority"); p != "" {
		parsed, err := strconv.ParseInt(p, 10, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "priority parameter must be a 32-bit integer"})
			return
		}
		priority = parsed
	}

	// wifi=on (the default) hands over devices online on the bench Wi-Fi;
	// wifi=off hands them over verified offline.
	wifi, ok := parseWifiState(r.URL.Query().Get("wifi"), true)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wifi parameter must be on or off"})
		return
	}

	// wifi_band=2.4|5|6 joins each phone on that GHz band of the bench Wi-Fi.
	wifiBand := r.URL.Query().Get("wifi_band")
	if !validWifiBand(wifiBand) || (wifiBand != "" && !wifi) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wifi_band parameter must be 2.4, 5 or 6, with wifi on"})
		return
	}

	// same_proxy=true takes every device from one proxy, for phones that must
	// sit together; the orchestrator does the picking.
	sameProxy := false
	if v := r.URL.Query().Get("same_proxy"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "same_proxy parameter must be true or false"})
			return
		}
		sameProxy = parsed
	}

	q := r.URL.Query()
	var runAttempt int64
	if v := q.Get("run_attempt"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_attempt parameter must be a 32-bit integer"})
			return
		}
		runAttempt = parsed
	}
	labels, err := parseLabels(q["label"])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	job := &pb.AcquireLockRequest{
		Repo: q.Get("repo"), RunUrl: q.Get("run_url"), Job: q.Get("job"), RunAttempt: int32(runAttempt),
		Sha: q.Get("sha"), Branch: q.Get("branch"), Workflow: q.Get("workflow"), Stage: q.Get("stage"), Labels: labels,
	}
	sp, err := a.scopedPortManager.Acquire(r.Context(), requestedSerials, int32(count), job, int32(priority), wifi, wifiBand, sameProxy, a.tenantKey(r), clientIP(r))
	if err != nil {
		code, msg := httpStatusForAcquireError(err)
		slog.Error("failed to acquire", "error", err, "status", code, "message", msg)
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	// The runner hung up after the grant (or during the device setup after
	// it): nobody will use this port, so free the devices now rather than at
	// the idle timeout (sair#133).
	if r.Context().Err() != nil {
		slog.Warn("client left before the lock was handed over; releasing", "lockId", sp.LockID)
		a.scopedPortManager.Release(sp.LockID, "cancelled")
		return
	}

	serials := make([]string, 0, len(sp.Serials))
	for s := range sp.Serials {
		serials = append(serials, s)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"lock_id":         sp.LockID,
		"serials":         serials,
		"port":            sp.Port,
		"device_token":    sp.DeviceToken,
		"device_api_port": sp.DeviceAPIPort,
	})
}

// httpStatusForAcquireError maps an orchestrator error to an HTTP status and
// a message without the "rpc error: code = ... desc = ..." wrapping, so
// sair-acquire can show the reason (e.g. a plan limit) to the user verbatim.
func httpStatusForAcquireError(err error) (int, string) {
	st, ok := status.FromError(err)
	if !ok {
		return http.StatusInternalServerError, err.Error()
	}
	switch st.Code() {
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, st.Message()
	case codes.InvalidArgument:
		return http.StatusBadRequest, st.Message()
	case codes.PermissionDenied, codes.Unauthenticated:
		return http.StatusForbidden, st.Message()
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, st.Message()
	case codes.FailedPrecondition, codes.Unimplemented:
		return http.StatusNotImplemented, st.Message()
	case codes.NotFound:
		return http.StatusNotFound, st.Message()
	default:
		return http.StatusInternalServerError, st.Message()
	}
}

func (a *HTTPApi) handleRelease(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing x-api-key header"})
		return
	}

	lockID := r.URL.Query().Get("lock_id")
	if lockID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lock_id parameter required"})
		return
	}

	released, err := a.scopedPortManager.Release(lockID, r.URL.Query().Get("status"))
	if released {
		writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
		return
	}
	// The orchestrator's NotFound says why a lock ended (e.g. a proxy
	// restart), which this proxy may not remember.
	msg := "unknown lock_id"
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		msg = st.Message()
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": msg})
}

// Label caps, per request. The orchestrator stores what it is sent, so the
// proxy keeps a runaway client from filling a job record.
const (
	maxLabels        = 32
	maxLabelKeyLen   = 64
	maxLabelValueLen = 256
)

// parseLabels reads label parameters: "k=v" for the lock, or "SERIAL:k=v"
// for one of its devices. The serial is split off at the last ':' before the
// '=', since TCP serials (host:port) have one; keys cannot.
func parseLabels(params []string) ([]*pb.Label, error) {
	if len(params) > maxLabels {
		return nil, fmt.Errorf("at most %d labels per request", maxLabels)
	}
	labels := make([]*pb.Label, 0, len(params))
	for _, p := range params {
		left, value, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("label %q must be k=v or SERIAL:k=v", p)
		}
		var serial string
		key := left
		if i := strings.LastIndex(left, ":"); i >= 0 {
			serial, key = left[:i], left[i+1:]
		}
		if key == "" || (serial == "" && strings.Contains(left, ":")) {
			return nil, fmt.Errorf("label %q must be k=v or SERIAL:k=v", p)
		}
		if len(key) > maxLabelKeyLen || len(value) > maxLabelValueLen {
			return nil, fmt.Errorf("label %q: key is at most %d bytes, value at most %d", p, maxLabelKeyLen, maxLabelValueLen)
		}
		labels = append(labels, &pb.Label{Serial: serial, Key: key, Value: value})
	}
	return labels, nil
}

// handleLabel adds labels to a held lock, for roles a client only knows once
// it has the serials (sair#141).
func (a *HTTPApi) handleLabel(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing x-api-key header"})
		return
	}
	lockID := r.URL.Query().Get("lock_id")
	if lockID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lock_id parameter required"})
		return
	}
	labels, err := parseLabels(r.URL.Query()["label"])
	if err == nil && len(labels) == 0 {
		err = errors.New("label parameter required")
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	err = a.scopedPortManager.AddLabels(lockID, labels)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case errors.Is(err, errUnknownLock):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, errNotInLock):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	default:
		code, msg := httpStatusForAcquireError(err)
		slog.Error("adding labels failed", "lockId", lockID, "error", err)
		writeJSON(w, code, map[string]string{"error": msg})
	}
}

// parseWifiState reads an on/off parameter; empty means def.
func parseWifiState(v string, def bool) (on, ok bool) {
	switch v {
	case "":
		return def, true
	case "on":
		return true, true
	case "off":
		return false, true
	}
	return false, false
}

// validWifiBand reports whether v is a WifiRequest.band the device source
// takes; "" is any band.
func validWifiBand(v string) bool {
	return v == "" || v == "2.4" || v == "5" || v == "6"
}

// lockRequest authorizes a mid-job request on a lock's devices (/wifi,
// /bluetooth) and reads which devices: lock_id, and serial (comma-separated;
// empty is every local device of the lock). ok is false when the response
// was already written.
//
// Besides the tenant key, a lock's device token (x-device-token) is accepted
// here and nowhere else: it is what a test on a locked phone holds (sair#83).
// It must name the serial, since the proxy cannot tell which phone a
// reverse-forwarded connection came from.
func (a *HTTPApi) lockRequest(w http.ResponseWriter, r *http.Request) (lockID string, serials []string, ok bool) {
	q := r.URL.Query()
	lockID = q.Get("lock_id")
	if lockID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lock_id parameter required"})
		return "", nil, false
	}
	byToken := !a.requireAuth(r)
	if byToken && !a.scopedPortManager.DeviceTokenValid(lockID, r.Header.Get("x-device-token")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing x-api-key or x-device-token header"})
		return "", nil, false
	}
	if byToken && strings.TrimSpace(q.Get("serial")) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial parameter required with x-device-token"})
		return "", nil, false
	}
	for _, s := range strings.Split(q.Get("serial"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			serials = append(serials, s)
		}
	}
	return lockID, serials, true
}

// writeLockResult answers a mid-job device request from its error.
func writeLockResult(w http.ResponseWriter, lockID, what string, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case errors.Is(err, errUnknownLock):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, errNotInLock):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	default:
		code, msg := httpStatusForAcquireError(err)
		slog.Error(what+" failed", "lockId", lockID, "error", err)
		writeJSON(w, code, map[string]string{"error": msg})
	}
}

// handleWifi turns Wi-Fi on or off on a lock's devices mid-job, using the
// bench network so the job never needs the credentials.
func (a *HTTPApi) handleWifi(w http.ResponseWriter, r *http.Request) {
	lockID, serials, ok := a.lockRequest(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	on, ok := parseWifiState(q.Get("state"), false)
	if !ok || q.Get("state") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state parameter must be on or off"})
		return
	}
	band := q.Get("band")
	if !validWifiBand(band) || (band != "" && !on) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "band parameter must be 2.4, 5 or 6, with state on"})
		return
	}
	writeLockResult(w, lockID, "wifi change", a.scopedPortManager.SetWifi(lockID, serials, on, band))
}

// handleBluetooth restarts the Bluetooth stack on a lock's devices mid-job
// (action=restart), the bench's own off-then-on cycle, for a test that
// exercises recovery from it (sair#156). Returns once the adapter is back
// ON; the job keeps the lock whatever happens.
func (a *HTTPApi) handleBluetooth(w http.ResponseWriter, r *http.Request) {
	lockID, serials, ok := a.lockRequest(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("action") != "restart" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action parameter must be restart"})
		return
	}
	writeLockResult(w, lockID, "bluetooth restart", a.scopedPortManager.RestartBluetooth(lockID, serials))
}

func (a *HTTPApi) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing x-api-key header"})
		return
	}

	ports := a.scopedPortManager.GetAllScopedPorts()
	now := time.Now()

	type portInfo struct {
		LockID  string   `json:"lock_id"`
		Serials []string `json:"serials"`
		Port    int      `json:"port"`
		AgeMs   int64    `json:"age_ms"`
	}

	portInfos := make([]portInfo, 0, len(ports))
	for _, sp := range ports {
		serials := make([]string, 0, len(sp.Serials))
		for s := range sp.Serials {
			serials = append(serials, s)
		}
		portInfos = append(portInfos, portInfo{
			LockID:  sp.LockID,
			Serials: serials,
			Port:    sp.Port,
			AgeMs:   now.Sub(sp.CreatedAt).Milliseconds(),
		})
	}

	// Per-phone state a bench owner acts on: when sair last rebooted it
	// (sair#93) and why it is not ready, e.g. a lock screen sair cannot
	// dismiss (sair#92).
	type devInfo struct {
		Serial     string `json:"serial"`
		LastReboot string `json:"last_reboot,omitempty"`
		Warning    string `json:"warning,omitempty"`
		// How often it failed to get online on the bench Wi-Fi (sair#96).
		Wifi *pb.WifiStats `json:"wifi,omitempty"`
	}
	devices := []devInfo{}
	if a.deviceListTracker != nil {
		for _, d := range a.deviceListTracker.GetDevices() {
			di := devInfo{Serial: d.Serial, Warning: d.Warning, Wifi: d.Wifi}
			if d.LastRebootMs > 0 {
				di.LastReboot = time.UnixMilli(d.LastRebootMs).UTC().Format(time.RFC3339)
			}
			devices = append(devices, di)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"scoped_ports": portInfos, "devices": devices, "version": version.Version})
}

type deviceReport struct {
	SourceAddr string       `json:"source_addr"`
	Version    string       `json:"version,omitempty"` // empty from device sources that predate it
	Devices    []deviceInfo `json:"devices"`
}

type deviceInfo struct {
	Serial       string   `json:"serial"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	Sdk          int32    `json:"sdk"`
	Release      int32    `json:"release"`
	AccountTypes []string `json:"account_types,omitempty"`
	LastRebootMs int64    `json:"last_reboot_ms,omitempty"`
	Warning      string   `json:"warning,omitempty"`
	// The device source sends its devicesource.WifiStats; the orchestrator's
	// copy of the message has the same fields, so it decodes straight into it.
	Wifi *pb.WifiStats `json:"wifi,omitempty"`
}

func (a *HTTPApi) handleRegisterDevices(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing x-api-key header"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
	var report deviceReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if report.SourceAddr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_addr required"})
		return
	}

	var pbDevices []*pb.DeviceInfo
	for _, d := range report.Devices {
		pbDevices = append(pbDevices, &pb.DeviceInfo{
			Serial:       d.Serial,
			Manufacturer: d.Manufacturer,
			Model:        d.Model,
			Sdk:          d.Sdk,
			Release:      d.Release,
			AccountTypes: d.AccountTypes,
			LastRebootMs: d.LastRebootMs,
			Warning:      d.Warning,
			Wifi:         d.Wifi,
		})
	}

	a.deviceListTracker.UpdateDevices(report.SourceAddr, report.Version, pbDevices)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
