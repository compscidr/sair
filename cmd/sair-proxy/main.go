package main

import (
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/compscidr/sair/internal/proxy"
)

func main() {
	port := envInt("ADB_PROXY_PORT", 5037)
	orchestratorAddr := envStr("ORCHESTRATOR_ADDR", "localhost:9090")
	apiKey := envStr("SAIR_API_KEY", "dev-key-123")
	gracePeriodMs := envInt64("SESSION_GRACE_PERIOD_MS", 30000)
	httpAPIPort := envInt("PROXY_HTTP_PORT", 8550)
	httpAPIHost := envStr("PROXY_HTTP_HOST", "0.0.0.0")
	heartbeatInterval := envInt64("HEARTBEAT_INTERVAL_SECONDS", 60)
	orchestratorTLS := envBool("ORCHESTRATOR_TLS")

	slog.Info("ADB Proxy starting...")
	slog.Info("config",
		"adb_port", port,
		"http_api", httpAPIHost+":"+strconv.Itoa(httpAPIPort),
		"orchestrator", orchestratorAddr,
		"tls", orchestratorTLS,
	)

	commandRouter, err := proxy.NewCommandRouter(orchestratorAddr, apiKey, orchestratorTLS)
	if err != nil {
		slog.Error("failed to create command router", "error", err)
		os.Exit(1)
	}

	sessionManager := proxy.NewSessionManager(commandRouter, gracePeriodMs)
	deviceListTracker := proxy.NewDeviceListTracker(commandRouter, 5000)
	scopedPortManager := proxy.NewScopedPortManager(commandRouter, sessionManager, deviceListTracker, heartbeatInterval)
	httpAPI := proxy.NewHTTPApi(scopedPortManager, apiKey, httpAPIPort, httpAPIHost)

	adbProxy := proxy.NewAdbProxy(port, commandRouter, sessionManager, deviceListTracker)

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

	httpAPI.Start()
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
