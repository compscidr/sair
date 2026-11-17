package main

import (
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/compscidr/sair/internal/logging"
	"github.com/compscidr/sair/internal/proxy"
	"github.com/compscidr/sair/internal/updater"
	"github.com/compscidr/sair/internal/version"
)

const usage = `Usage: sair-proxy [--version | --help]

The SAIR ADB proxy. Takes no arguments; it is configured through env vars:

  SAIR_API_KEY                API key for the orchestrator (default: dev-key-123)
  SAIR_GUEST_API_KEYS         Comma-separated keys of accounts that may lock this proxy's devices
  ORCHESTRATOR_ADDR           Orchestrator gRPC address (default: orchestrator.sair.run:9090)
  ORCHESTRATOR_TLS            true/false (default: true for the hosted orchestrator, else false)
  PROXY_ID                    Identity reported to the orchestrator (default: the hostname)
  ADB_PROXY_PORT              Bare ADB port (default: 5037)
  PROXY_HTTP_HOST             HTTP API bind host (default: 0.0.0.0)
  PROXY_HTTP_PORT             HTTP API port (default: 8550)
  HEARTBEAT_INTERVAL_SECONDS  Lock heartbeat interval (default: 60)
  LOCK_IDLE_TIMEOUT_MINUTES   Release a lock with no adb client for this long; 0 disables (default: 20)
  SAIR_LOG_LEVEL              debug, info, warn or error (default: info)
  SAIR_AUTO_UPDATE            false disables updating to the latest release on start
  GITHUB_TOKEN                Optional token for the auto-updater's GitHub API calls
`

func main() {
	version.HandleArgs(usage)
	logging.SetLevelFromEnv()
	logging.KeepRecent()
	updater.CheckAndUpdate("sair-proxy")

	port := envInt("ADB_PROXY_PORT", 5037)
	orchestratorAddr := envStr("ORCHESTRATOR_ADDR", "orchestrator.sair.run:9090")
	apiKey := envStr("SAIR_API_KEY", "dev-key-123")
	// Other accounts' keys that may lock this proxy's devices (see README,
	// "Guest tenants"). Listing a key here is the owner's consent.
	guestKeys := envList("SAIR_GUEST_API_KEYS")
	httpAPIPort := envInt("PROXY_HTTP_PORT", 8550)
	httpAPIHost := envStr("PROXY_HTTP_HOST", "0.0.0.0")
	heartbeatInterval := envInt64("HEARTBEAT_INTERVAL_SECONDS", 60)
	// Auto-enable TLS when using the hosted orchestrator, unless explicitly set
	_, tlsExplicit := os.LookupEnv("ORCHESTRATOR_TLS")
	orchestratorTLS := envBool("ORCHESTRATOR_TLS")
	if !tlsExplicit && orchestratorAddr == "orchestrator.sair.run:9090" {
		orchestratorTLS = true
	}

	slog.Info("ADB Proxy starting...", "version", version.Version)
	slog.Info("config",
		"adb_port", port,
		"http_api", httpAPIHost+":"+strconv.Itoa(httpAPIPort),
		"orchestrator", orchestratorAddr,
		"tls", orchestratorTLS,
		"proxy_id", proxy.ProxyID(),
		"guest_keys", len(guestKeys),
	)

	commandRouter, err := proxy.NewCommandRouter(orchestratorAddr, apiKey, orchestratorTLS)
	if err != nil {
		slog.Error("failed to create command router", "error", err)
		os.Exit(1)
	}

	deviceListTracker := proxy.NewDeviceListTracker(commandRouter)
	scopedPortManager := proxy.NewScopedPortManager(commandRouter, deviceListTracker, heartbeatInterval)
	scopedPortManager.DeviceAPIPort = httpAPIPort
	scopedPortManager.IdleTimeout = time.Duration(envInt64("LOCK_IDLE_TIMEOUT_MINUTES", 20)) * time.Minute
	commandRouter.ResolveSource = deviceListTracker.GetSourceAddr
	// The release reset only checks uptime on phones that get acquired.
	go func() {
		for range time.Tick(time.Hour) {
			scopedPortManager.RebootIdle()
		}
	}()

	// Long-lived stream to the orchestrator: identity, device reports, heartbeats
	// and live logs. Falls back to the unary calls against an older orchestrator.
	commandRouter.StartSession(version.Version, heartbeatInterval, scopedPortManager.OnLockExpired, deviceListTracker.ReportNow, commandRouter.ServeRelay)

	httpAPI := proxy.NewHTTPApi(scopedPortManager, deviceListTracker, apiKey, guestKeys, httpAPIPort, httpAPIHost)

	adbProxy := proxy.NewAdbProxy(port, commandRouter, deviceListTracker)

	// Claim the ADB port before the HTTP API opens: /status is used as a
	// readiness probe, and must never answer for a proxy that is about to die
	// because something else already holds this port.
	if err := adbProxy.Listen(); err != nil {
		slog.Error("proxy failed", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down ADB Proxy...")
		httpAPI.Stop()
		scopedPortManager.ShutdownAll()
		adbProxy.Stop()
		commandRouter.Shutdown()
		slog.Info("ADB Proxy stopped")
		os.Exit(0)
	}()

	if err := httpAPI.Start(); err != nil {
		slog.Error("HTTP API failed", "error", err)
		os.Exit(1)
	}
	if err := adbProxy.Start(); err != nil {
		slog.Error("proxy failed", "error", err)
		os.Exit(1)
	}
}

func envStr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envList reads a comma-separated list, trimming spaces and dropping empties.
func envList(key string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}

func envInt64(key string, defaultVal int64) int64 {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}

func envBool(key string) bool {
	v := strings.ToLower(os.Getenv(key))
	return v == "true" || v == "1" || v == "yes"
}
