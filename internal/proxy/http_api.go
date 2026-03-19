package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// HTTPApi provides the HTTP API for the ADB proxy.
//
// Runners call this API (instead of the orchestrator) to acquire/release
// device locks. The proxy handles all orchestrator communication and
// opens scoped ADB ports for per-runner isolation.
type HTTPApi struct {
	scopedPortManager  *ScopedPortManager
	deviceListTracker  *DeviceListTracker
	apiKey             string
	server             *http.Server
}

func NewHTTPApi(scopedPortManager *ScopedPortManager, deviceListTracker *DeviceListTracker, apiKey string, port int, host string) *HTTPApi {
	api := &HTTPApi{
		scopedPortManager:  scopedPortManager,
		deviceListTracker:  deviceListTracker,
		apiKey:             apiKey,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /acquire", api.handleAcquire)
	mux.HandleFunc("POST /release", api.handleRelease)
	mux.HandleFunc("GET /status", api.handleStatus)
	mux.HandleFunc("POST /internal/devices", api.handleRegisterDevices)

	api.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", host, port),
		Handler: mux,
	}

	return api
}

func (a *HTTPApi) Start() {
	go func() {
		slog.Info("proxy HTTP API started", "addr", a.server.Addr)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP API failed", "error", err)
		}
	}()
}

func (a *HTTPApi) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.server.Shutdown(ctx)
	slog.Info("proxy HTTP API stopped")
}

func (a *HTTPApi) requireAuth(r *http.Request) bool {
	return r.Header.Get("x-api-key") == a.apiKey
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

	repo := r.URL.Query().Get("repo")
	sp, err := a.scopedPortManager.Acquire(requestedSerials, repo)
	if err != nil {
		slog.Error("failed to acquire", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	serials := make([]string, 0, len(sp.Serials))
	for s := range sp.Serials {
		serials = append(serials, s)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"lock_id": sp.LockID,
		"serials": serials,
		"port":    sp.Port,
	})
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

	released := a.scopedPortManager.Release(lockID)
	if released {
		writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
	} else {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown lock_id"})
	}
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

	writeJSON(w, http.StatusOK, map[string]any{"scoped_ports": portInfos})
}

type deviceReport struct {
	SourceAddr string       `json:"source_addr"`
	Devices    []deviceInfo `json:"devices"`
}

type deviceInfo struct {
	Serial       string `json:"serial"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Sdk          int32  `json:"sdk"`
	Release      int32  `json:"release"`
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
		})
	}

	a.deviceListTracker.UpdateDevices(report.SourceAddr, pbDevices)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
