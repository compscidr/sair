package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
)

// AdbProxy is the main ADB Proxy server.
//
// Listens on a TCP port (default 5037, matching ADB server) and translates
// ADB smart socket protocol requests into orchestrator gRPC calls.
//
// The bare port (5037) shows NO devices — runners must acquire a scoped port
// via the proxy HTTP API to access devices.
type AdbProxy struct {
	port              int
	commandRouter     *CommandRouter
	sessionManager    *SessionManager
	deviceListTracker *DeviceListTracker
	listener          net.Listener
	running           atomic.Bool
}

func NewAdbProxy(
	port int,
	commandRouter *CommandRouter,
	sessionManager *SessionManager,
	deviceListTracker *DeviceListTracker,
) *AdbProxy {
	return &AdbProxy{
		port:              port,
		commandRouter:     commandRouter,
		sessionManager:    sessionManager,
		deviceListTracker: deviceListTracker,
	}
}

func (p *AdbProxy) Start() error {
	p.deviceListTracker.Start()
	p.running.Store(true)

	var err error
	p.listener, err = net.Listen("tcp", fmt.Sprintf(":%d", p.port))
	if err != nil {
		return err
	}

	slog.Info("ADB proxy listening (bare — no devices visible)", "port", p.port)

	for p.running.Load() {
		conn, err := p.listener.Accept()
		if err != nil {
			if p.running.Load() {
				slog.Error("error accepting connection", "error", err)
			}
			continue
		}
		// Disable Nagle's algorithm so ADB protocol messages are sent immediately
		if tc, ok := conn.(*net.TCPConn); ok {
			if err := tc.SetNoDelay(true); err != nil {
				slog.Warn("failed to set TCP_NODELAY", "remote", conn.RemoteAddr(), "error", err)
			}
		}
		// Bare port: allowedSerials = empty map → no devices visible
		adbConn := NewAdbConnection(conn, p.commandRouter, p.sessionManager, p.deviceListTracker, map[string]struct{}{})
		go adbConn.Handle()
	}
	return nil
}

func (p *AdbProxy) Stop() {
	p.running.Store(false)
	p.sessionManager.ReleaseAll()
	p.deviceListTracker.Stop()
	if p.listener != nil {
		p.listener.Close()
	}
}
