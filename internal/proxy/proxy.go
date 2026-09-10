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
// ADB smart socket protocol requests into device-source gRPC calls.
//
// The bare port (5037) shows NO devices — runners must acquire a scoped port
// via the proxy HTTP API to access devices.
type AdbProxy struct {
	port              int
	commandRouter     *CommandRouter
	deviceListTracker *DeviceListTracker
	listener          net.Listener
	running           atomic.Bool
}

func NewAdbProxy(
	port int,
	commandRouter *CommandRouter,
	deviceListTracker *DeviceListTracker,
) *AdbProxy {
	return &AdbProxy{
		port:              port,
		commandRouter:     commandRouter,
		deviceListTracker: deviceListTracker,
	}
}

// Listen binds the ADB port without serving. Callers that advertise readiness
// elsewhere should Listen first, so a port collision — a stale adb server on
// 5037, say — kills the process before anything reports the proxy as up.
func (p *AdbProxy) Listen() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.port))
	if err != nil {
		return err
	}
	p.listener = ln
	return nil
}

// Start serves the accept loop, binding first if Listen has not already run.
func (p *AdbProxy) Start() error {
	if p.listener == nil {
		if err := p.Listen(); err != nil {
			return err
		}
	}

	p.deviceListTracker.Start()
	p.running.Store(true)

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
		adbConn := NewAdbConnection(conn, p.commandRouter, p.deviceListTracker, map[string]struct{}{}, nil)
		go adbConn.Handle()
	}
	return nil
}

func (p *AdbProxy) Stop() {
	p.running.Store(false)
	p.deviceListTracker.Stop()
	if p.listener != nil {
		p.listener.Close()
	}
}
