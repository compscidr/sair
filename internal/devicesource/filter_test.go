package devicesource

import (
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/compscidr/sair/internal/logging"
	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const adbDevices = "List of devices attached\nCI1\tdevice\nDEV1\tdevice\nCI2\tdevice\nUNAUTH\tunauthorized\n\n"

// DEVICE_SERIALS keeps only the listed phones, EXCLUDE_SERIALS drops the
// listed ones, neither reports everything in state "device" (sair#90).
func TestSerialFilter(t *testing.T) {
	tests := []struct {
		name        string
		allow, deny []string
		want        []string
	}{
		{"unfiltered", nil, nil, []string{"CI1", "DEV1", "CI2"}},
		{"allowlist", []string{"CI1", "CI2", "TYPO"}, nil, []string{"CI1", "CI2"}},
		{"allowlist set but empty", []string{}, nil, nil},
		{"denylist", nil, []string{"DEV1"}, []string{"CI1", "CI2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{allow: packageSet(tt.allow), deny: packageSet(tt.deny)}
			if tt.allow != nil && s.allow == nil {
				s.allow = map[string]struct{}{}
			}
			got := s.filterSerials(parseAdbDevices(adbDevices))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A filtered phone is as good as unplugged: every per-device RPC refuses it
// before touching the phone, so no lock, reset or forward can reach it.
func TestFilteredSerialIsRefusedByRPCs(t *testing.T) {
	s := &Server{wifiSSID: "bench", deny: packageSet([]string{"DEV1"}), baselineOverride: map[string]struct{}{}}
	cmds := fakeShell(s, nil)
	if _, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "DEV1", State: pb.WifiRequest_RESET}); status.Code(err) != codes.NotFound {
		t.Errorf("SetWifi: err = %v, want NotFound", err)
	}
	if _, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "DEV1"}); status.Code(err) != codes.NotFound {
		t.Errorf("ResetPackages: err = %v, want NotFound", err)
	}
	if _, err := s.ListPackages(context.Background(), &pb.ListPackagesRequest{Serial: "DEV1"}); status.Code(err) != codes.NotFound {
		t.Errorf("ListPackages: err = %v, want NotFound", err)
	}
	if len(*cmds) != 0 {
		t.Errorf("a filtered phone was touched: %q", *cmds)
	}
}

// Build props are read in one shell and only once per attachment: a device
// report that runs adb shells every 5 s is late on a busy host, and a late
// report gets the source dropped (sair#104). A failed read is retried, and a
// phone that leaves is read again when it comes back.
func TestDeviceInfoReadsBuildPropsOncePerAttachment(t *testing.T) {
	const props = "getprop ro.product.model; getprop ro.product.manufacturer; getprop ro.build.version.release; getprop ro.build.version.sdk"
	s := &Server{baselineOverride: map[string]struct{}{}}
	cmds := fakeShell(s, map[string]string{props: "SM-A065M\r\nsamsung\r\n14\r\n34\r\n"}, "dumpsys")
	countProps := func() int {
		n := 0
		for _, c := range *cmds {
			if c == props {
				n++
			}
		}
		return n
	}

	for range 2 {
		d, err := s.getDeviceInfo("ABC")
		if err != nil {
			t.Fatal(err)
		}
		if d.Model != "SM-A065M" || d.Manufacturer != "samsung" || d.Release != 14 || d.Sdk != 34 {
			t.Fatalf("device = %+v", d)
		}
	}
	if n := countProps(); n != 1 {
		t.Fatalf("props read %d times, want 1: %q", n, *cmds)
	}

	s.forgetProps([]string{"OTHER"})
	s.getDeviceInfo("ABC")
	if n := countProps(); n != 2 {
		t.Errorf("props not re-read after the phone left: %d reads", n)
	}

	fakeShell(s, map[string]string{}, "getprop")
	d, _ := s.getDeviceInfo("NEW")
	if d.Model != "" {
		t.Errorf("model from a failed read: %q", d.Model)
	}
	cmds = fakeShell(s, map[string]string{props: "Pixel\ngoogle\n15\n35\n"}, "dumpsys")
	if d, _ := s.getDeviceInfo("NEW"); d.Model != "Pixel" {
		t.Errorf("failed read was cached: %+v", d)
	}
}

// RecentLog hands out a phone's own lines only, and nothing for a phone the
// filter excludes: the proxy must not be able to pull another phone's log.
func TestRecentLogFiltersBySerial(t *testing.T) {
	logging.KeepRecent()
	slog.Info("SetWifi", "serial", "DEV2", "state", "ON")
	slog.Info("SetWifi", "serial", "DEV3", "state", "ON")
	s := &Server{deny: packageSet([]string{"DEV1"})}
	resp, err := s.RecentLog(context.Background(), &pb.RecentLogRequest{Serial: "DEV2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Lines) == 0 || slices.ContainsFunc(resp.Lines, func(l string) bool { return !strings.Contains(l, "DEV2") }) {
		t.Errorf("lines for DEV2 = %q", resp.Lines)
	}
	if _, err := s.RecentLog(context.Background(), &pb.RecentLogRequest{Serial: "DEV1"}); status.Code(err) != codes.NotFound {
		t.Errorf("excluded serial: err = %v, want NotFound", err)
	}
}
