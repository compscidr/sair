package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/compscidr/sair/internal/devicesource"
	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	port := 8080
	if v := os.Getenv("DEVICE_SOURCE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	server := grpc.NewServer()
	pb.RegisterDeviceSourceServer(server, devicesource.NewServer())
	reflection.Register(server)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down gRPC server")
		server.GracefulStop()
	}()

	slog.Info("DeviceSource server listening", "port", port)
	if err := server.Serve(lis); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
