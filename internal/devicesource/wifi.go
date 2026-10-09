package devicesource

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// SetWifi puts a device's Wi-Fi in a known state using the bench network from
// WIFI_SSID / WIFI_PASSPHRASE. Only suggestions added by the shell uid are
// touched: a test app's own suggestions are its own to clean up.
func (s *Server) SetWifi(ctx context.Context, req *pb.WifiRequest) (*emptypb.Empty, error) {
	if err := validateSerial(req.Serial); err != nil {
		return nil, err
	}
	if s.wifiSSID == "" {
		return nil, status.Error(codes.FailedPrecondition, "WIFI_SSID is not set on the device source; Wi-Fi is not managed on this bench")
	}
	slog.Info("SetWifi", "serial", req.Serial, "state", req.State)

	var err error
	switch req.State {
	case pb.WifiRequest_RESET:
		if err = s.wifiForget(ctx, req.Serial); err == nil {
			if _, err = s.shell(ctx, req.Serial, "svc wifi disable"); err == nil {
				_, err = s.shell(ctx, req.Serial, "svc wifi enable")
			}
		}
	case pb.WifiRequest_OFF:
		err = s.wifiForget(ctx, req.Serial)
	case pb.WifiRequest_ON:
		err = s.wifiConnect(ctx, req.Serial)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown Wi-Fi state %d", req.State)
	}
	if err != nil {
		if _, ok := status.FromError(err); !ok {
			err = status.Errorf(codes.Internal, "%s: Wi-Fi %s: %v", req.Serial, req.State, err)
		}
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// wifiForget drops every shell-uid suggestion and saved network. If the shell
// may not remove all suggestions it falls back to the bench SSID alone.
func (s *Server) wifiForget(ctx context.Context, serial string) error {
	if _, err := s.shell(ctx, serial, "cmd wifi remove-all-suggestions"); err != nil {
		slog.Warn("remove-all-suggestions failed; removing the bench SSID only", "serial", serial, "error", err)
		if _, err := s.shell(ctx, serial, "cmd wifi remove-suggestion "+shellQuote(s.wifiSSID)); err != nil {
			return fmt.Errorf("remove suggestion: %w", err)
		}
	}
	out, err := s.shell(ctx, serial, "cmd wifi list-networks")
	if err != nil {
		slog.Warn("list-networks failed; saved networks not forgotten", "serial", serial, "error", err)
		return nil
	}
	for _, id := range savedNetworkIDs(out) {
		if _, err := s.shell(ctx, serial, "cmd wifi forget-network "+id); err != nil {
			return fmt.Errorf("forget network %s: %w", id, err)
		}
	}
	return nil
}

// wifiConnect suggests the bench network and waits until the device can ping
// wifiPingHost, so a job never gets a phone that is still associating.
func (s *Server) wifiConnect(ctx context.Context, serial string) error {
	add := "cmd wifi add-suggestion " + shellQuote(s.wifiSSID) + " open"
	if s.wifiPassphrase != "" {
		add = "cmd wifi add-suggestion " + shellQuote(s.wifiSSID) + " wpa2 " + shellQuote(s.wifiPassphrase)
	}
	if _, err := s.shell(ctx, serial, add); err != nil {
		return fmt.Errorf("add suggestion: %w", err)
	}
	if _, err := s.shell(ctx, serial, "cmd wifi start-scan"); err != nil {
		return fmt.Errorf("start scan: %w", err)
	}
	deadline := time.Now().Add(s.wifiTimeout)
	for {
		if _, err := s.shell(ctx, serial, "ping -c1 -W2 "+shellQuote(s.wifiPingHost)); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return status.Errorf(codes.DeadlineExceeded, "%s did not reach %s within %s after joining Wi-Fi %q",
				serial, s.wifiPingHost, s.wifiTimeout, s.wifiSSID)
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// adbShell runs one command line on the device. The command is passed as a
// single argument so the device shell parses it, quoting included. Errors
// carry the output but never the command, which may hold the passphrase.
func (s *Server) adbShell(ctx context.Context, serial, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "adb", "-P", strconv.Itoa(s.adbPort), "-s", serial, "shell", command).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// savedNetworkIDs parses `cmd wifi list-networks`:
//
//	Network Id      SSID                         Security type
//	0            bench                          wpa2-psk
func savedNetworkIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err == nil {
			ids = append(ids, f[0])
		}
	}
	return ids
}

// shellQuote quotes s for the device's sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
