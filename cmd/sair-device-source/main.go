package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/compscidr/sair/internal/devicesource"
	"github.com/compscidr/sair/internal/logging"
	"github.com/compscidr/sair/internal/updater"
	"github.com/compscidr/sair/internal/version"
	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"
)

const usage = `Usage: sair-device-source [--version | --help]

The SAIR device source. Takes no arguments; it is configured through env vars:

  SAIR_API_KEY                  API key sent with device reports (default: dev-key-123)
  PROXY_ADDR                    Proxy HTTP API to report devices to (default: http://localhost:8550)
  DEVICE_SOURCE_PORT            gRPC port (default: 8552)
  DEVICE_SOURCE_ADDR            Address the proxy dials back (default: localhost:<DEVICE_SOURCE_PORT>)
  ADB_PORT                      Real ADB server port (default: 5038)
  DEVICE_SERIALS                Only manage these serials, comma-separated (default: every attached phone)
  EXCLUDE_SERIALS               Manage every attached phone except these; mutually exclusive with DEVICE_SERIALS
  WIFI_SSID                     Bench Wi-Fi network; empty means Wi-Fi is not managed
  WIFI_PASSPHRASE               Bench Wi-Fi passphrase; empty for an open network
  WIFI_PING_HOST                Host a phone must reach when online, and must not when offline (default: dns.google)
  WIFI_CONNECT_TIMEOUT_SECONDS  How long to wait for a phone to be online on the bench Wi-Fi (default: 60)
  WIFI_DISCONNECT_TIMEOUT_SECONDS  How long to wait for a phone to be offline after Wi-Fi off / reset (default: 20)
  BASELINE_PACKAGES             Third-party packages to keep on release, comma-separated; unset records
                                each phone's packages when first seen (set but empty keeps none)
  RESET_FORCE_STOP              Apps force-stopped on release (default: com.android.chrome,com.google.android.gm)
  RESET_CLEAR_PACKAGES          Apps whose data is cleared on release (default: none; clearing Chrome signs it out)
  REBOOT_UPTIME_HOURS           Reboot a phone on release once it has been up this long (default: 24; 0 = never)
  SAIR_LOG_LEVEL                debug, info, warn or error (default: info)
  SAIR_AUTO_UPDATE              false disables updating to the latest release on start
  GITHUB_TOKEN                  Optional token for the auto-updater's GitHub API calls
`

func main() {
	version.HandleArgs(usage)
	logging.SetLevelFromEnv()
	logging.KeepRecent()
	updater.CheckAndUpdate("sair-device-source")

	slog.Info("DeviceSource starting...", "version", version.Version)

	port := 8552
	if v := os.Getenv("DEVICE_SOURCE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	proxyAddr := os.Getenv("PROXY_ADDR")
	if proxyAddr == "" {
		proxyAddr = "http://localhost:8550"
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	// LookupEnv: a set-but-empty DEVICE_SERIALS is a real allowlist (manage nothing).
	if _, allow := os.LookupEnv("DEVICE_SERIALS"); allow && os.Getenv("EXCLUDE_SERIALS") != "" {
		slog.Error("DEVICE_SERIALS and EXCLUDE_SERIALS are mutually exclusive; set one")
		os.Exit(1)
	}
	dsServer := devicesource.NewServer()
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(64*1024*1024),
		grpc.MaxSendMsgSize(64*1024*1024),
		grpc.InitialWindowSize(16*1024*1024),
		grpc.InitialConnWindowSize(16*1024*1024),
	)
	pb.RegisterDeviceSourceServer(server, dsServer)
	reflection.Register(server)

	// Determine our address for the proxy to connect back for commands
	sourceAddr := os.Getenv("DEVICE_SOURCE_ADDR")
	if sourceAddr == "" {
		sourceAddr = fmt.Sprintf("localhost:%d", port)
	}

	// Start device reporter — pushes device list to proxy
	apiKey := os.Getenv("SAIR_API_KEY")
	if apiKey == "" {
		apiKey = "dev-key-123"
	}
	stopReport := make(chan struct{})
	go reportDevices(dsServer, proxyAddr, sourceAddr, apiKey, stopReport)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down gRPC server")
		close(stopReport)
		server.GracefulStop()
	}()

	slog.Info("DeviceSource server listening", "port", port, "proxy", proxyAddr)
	if err := server.Serve(lis); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}

type deviceReport struct {
	SourceAddr string       `json:"source_addr"`
	Version    string       `json:"version,omitempty"`
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
	// Same JSON as the proxy's orchestrator WifiStats: one message, two packages.
	Wifi *pb.WifiStats `json:"wifi,omitempty"`
}

func reportDevices(dsServer *devicesource.Server, proxyAddr, sourceAddr, apiKey string, stop chan struct{}) {
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	report := func() {
		devices, err := dsServer.GetDevices(nil, &emptypb.Empty{})
		if err != nil {
			slog.Warn("failed to get local devices", "error", err)
			return
		}

		var infos []deviceInfo
		for _, d := range devices.Devices {
			infos = append(infos, deviceInfo{
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

		body, err := json.Marshal(deviceReport{
			SourceAddr: sourceAddr,
			Version:    version.Version,
			Devices:    infos,
		})
		if err != nil {
			slog.Warn("failed to marshal device report", "error", err)
			return
		}

		req, err := http.NewRequest("POST", proxyAddr+"/internal/devices", bytes.NewReader(body))
		if err != nil {
			slog.Warn("failed to create request", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("failed to report devices to proxy", "error", err)
			return
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			slog.Warn("proxy rejected device report", "status", resp.StatusCode)
			return
		}

		slog.Info("reported devices to proxy", "count", len(infos), "proxy", proxyAddr)
	}

	// Report immediately on startup
	report()

	for {
		select {
		case <-ticker.C:
			report()
		case <-stop:
			return
		}
	}
}
