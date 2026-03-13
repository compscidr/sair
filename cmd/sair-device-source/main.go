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
	"github.com/compscidr/sair/internal/updater"
	"github.com/compscidr/sair/internal/version"
	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"
)

func main() {
	updater.CheckAndUpdate("sair-device-source")

	slog.Info("DeviceSource starting...", "version", version.Version)

	port := 8080
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
	stopReport := make(chan struct{})
	go reportDevices(dsServer, proxyAddr, sourceAddr, stopReport)

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
	Devices    []deviceInfo `json:"devices"`
}

type deviceInfo struct {
	Serial       string `json:"serial"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Sdk          int32  `json:"sdk"`
	Release      int32  `json:"release"`
}

func reportDevices(dsServer *devicesource.Server, proxyAddr, sourceAddr string, stop chan struct{}) {
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
			})
		}

		body, _ := json.Marshal(deviceReport{
			SourceAddr: sourceAddr,
			Devices:    infos,
		})

		resp, err := http.Post(proxyAddr+"/internal/devices", "application/json", bytes.NewReader(body))
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
