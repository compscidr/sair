package devicesource

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ResetBluetooth cycles the radio off and back on and waits until the
// adapter reports ON (sair#127): a stack an earlier job wedged (stuck
// advertisers, a scanner that never restarted) otherwise shows up in the
// next job as "No peers discovered". Bluetooth is turned back on even when
// it never reported off, since that is what tests expect. The outcome is
// the device's Bluetooth warning, so a radio that stays off shows on the
// dashboard, not only in a BLE test.
func (s *Server) ResetBluetooth(ctx context.Context, req *pb.ResetBluetoothRequest) (*emptypb.Empty, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	st, err := s.bluetoothState(ctx, req.Serial)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: Bluetooth: %v", req.Serial, err)
	}
	if st == "" {
		return nil, status.Errorf(codes.Unimplemented, "%s: no adapter state in dumpsys bluetooth_manager; Bluetooth is not managed on this phone", req.Serial)
	}
	slog.Info("ResetBluetooth", "serial", req.Serial, "state", st)

	offErr := s.svc(ctx, req.Serial, "bluetooth disable")
	if offErr == nil {
		offErr = s.bluetoothOff(ctx, req.Serial)
	}
	onErr := s.svc(ctx, req.Serial, "bluetooth enable")
	if onErr == nil {
		onErr = s.waitFor(ctx, s.bluetoothTimeout, req.Serial+" Bluetooth not back on", func() error {
			return s.checkBluetooth(ctx, req.Serial, "ON")
		})
	}
	err = cmp.Or(onErr, offErr)
	switch {
	case err == nil:
	case onErr == nil:
		// Back on, but the cycle was partial: the lock log should not read
		// as if the phone were left at BLE_ON (sair#156).
		off := status.Convert(offErr)
		err = status.Errorf(off.Code(), "%s: Bluetooth back on but never fully off: %s", req.Serial, off.Message())
	default:
		if _, ok := status.FromError(err); !ok {
			err = status.Errorf(codes.Internal, "%s: Bluetooth reset: %v", req.Serial, err)
		}
	}
	s.stateMu.Lock()
	if s.btWarnings == nil {
		s.btWarnings = make(map[string]string)
	}
	if err != nil {
		s.btWarnings[req.Serial] = status.Convert(err).Message()
	} else {
		delete(s.btWarnings, req.Serial)
	}
	s.stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// bluetoothOff waits, after `svc bluetooth disable`, until the adapter is
// fully OFF. Disable alone stops at BLE_ON on a phone with "Nearby device
// scanning" on or an app holding BLE through enableBle, and BLE is where a
// wedged advertiser or scanner lives. So BLE scanning is turned off, which
// drops the AOSP-tracked BLE apps (all the A16 had), and whatever the dump
// still lists as a BLE app is force-stopped (the A06's One UI beaconmanager
// and mdx.kit ignore the setting). What survives that too is cleared by a
// Bluetooth-only airplane mode toggle (the A06's system_server holder,
// sair#159). The settings are put back on return, before Bluetooth is
// enabled again. Seen on the cube's phones, Oct and Nov 2026.
func (s *Server) bluetoothOff(ctx context.Context, serial string) error {
	wait := func(timeout time.Duration, states ...string) error {
		return s.waitFor(ctx, timeout, serial+" Bluetooth not off", func() error {
			return s.checkBluetooth(ctx, serial, states...)
		})
	}
	if err := wait(s.bluetoothTimeout, "OFF", "BLE_ON"); err != nil {
		return err
	}
	if s.checkBluetooth(ctx, serial, "OFF") == nil {
		return nil
	}
	if scan, err := s.shell(ctx, serial, "settings get global ble_scan_always_enabled"); err == nil && strings.TrimSpace(scan) == "1" {
		if _, err := s.shell(ctx, serial, "settings put global ble_scan_always_enabled 0"); err == nil {
			defer s.shell(context.WithoutCancel(ctx), serial, "settings put global ble_scan_always_enabled 1")
			if wait(min(5*time.Second, s.bluetoothTimeout), "OFF") == nil {
				return nil
			}
		}
	}
	out, err := s.shell(ctx, serial, "dumpsys bluetooth_manager")
	if err != nil {
		return fmt.Errorf("dumpsys bluetooth_manager: %w", err)
	}
	apps := bleApps(out)
	slog.Info("Bluetooth held at BLE_ON; force-stopping its BLE apps", "serial", serial, "apps", apps)
	for _, pkg := range apps {
		if _, err := s.shell(ctx, serial, "am force-stop "+shellQuote(pkg)); err != nil {
			slog.Warn("force-stop failed", "serial", serial, "package", pkg, "error", err)
		}
	}
	if wait(min(5*time.Second, s.bluetoothTimeout), "OFF") == nil {
		return nil
	}
	// Still BLE_ON: a holder nothing above drops. The A06's is One UI's
	// Auto Hotspot BLE scanner inside system_server, which the dump lists
	// as "android" (sair#159). Airplane mode is the one switch that clears
	// every BLE holder; scoped to the Bluetooth radio, Wi-Fi and cell keep
	// their connections. The radio list is put back on return.
	slog.Info("Bluetooth still at BLE_ON; cycling airplane mode for Bluetooth only", "serial", serial)
	if radios, err := s.shell(ctx, serial, "settings get global airplane_mode_radios"); err == nil {
		restore := "settings put global airplane_mode_radios " + shellQuote(strings.TrimSpace(radios))
		if strings.TrimSpace(radios) == "null" {
			restore = "settings delete global airplane_mode_radios"
		}
		if _, err := s.shell(ctx, serial, "settings put global airplane_mode_radios bluetooth"); err == nil {
			defer s.shell(context.WithoutCancel(ctx), serial, restore)
		}
	}
	if _, err := s.radioShell(ctx, serial, "cmd connectivity airplane-mode enable"); err != nil {
		slog.Warn("airplane mode on failed", "serial", serial, "error", err)
	}
	err = wait(s.bluetoothTimeout, "OFF")
	if _, offErr := s.radioShell(context.WithoutCancel(ctx), serial, "cmd connectivity airplane-mode disable"); offErr != nil {
		slog.Warn("airplane mode off failed", "serial", serial, "error", offErr)
	}
	if err != nil {
		// Not %w: a wrapped status reads "rpc error: code = ... desc = ..."
		// in the lock log.
		st := status.Convert(err)
		return status.Errorf(st.Code(), "%s even through airplane mode (BLE apps: %s)", st.Message(), cmp.Or(strings.Join(apps, ", "), "none listed"))
	}
	return nil
}

// bleApps lists the packages under the dump's "BLE app(s) registered"
// headers, the AOSP one and One UI's extra Samsung one:
//
//	1 BLE app registered                    (AOSP: Moto g15, Redmi Note 14)
//	  com.google.android.gms
//
//	3 BLE apps registered                   (One UI: Galaxy A06)
//
//	2 Samsung BLE apps registered
//	  com.google.android.gms
//	  com.samsung.android.beaconmanager
func bleApps(out string) []string {
	var apps []string
	inList := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.Contains(strings.ToLower(t), "ble app"):
			inList = true
		case inList && t != "" && line != t && packageName.MatchString(t):
			apps = append(apps, t)
		case t != "":
			inList = false
		}
	}
	return apps
}

var packageName = regexp.MustCompile(`^[A-Za-z]\w*(\.\w+)+$`)

// checkBluetooth wants the adapter in one of states.
func (s *Server) checkBluetooth(ctx context.Context, serial string, states ...string) error {
	st, err := s.bluetoothState(ctx, serial)
	if err != nil {
		return err
	}
	if !slices.Contains(states, st) {
		return fmt.Errorf("adapter is %s", cmp.Or(st, "unknown"))
	}
	return nil
}

// bluetoothState is the adapter state `dumpsys bluetooth_manager` gives
// first, "" when it gives none (Android 4.x has no such service):
//
//	Bluetooth Status
//	  enabled: true
//	  state: ON                (One UI, Android 14 and 16; AOSP Android 15)
//
//	Bluetooth Status:
//	  State:         ON        (Pixel, Android 16 and 17)
//
// A phone without the service at all ("Can't find service") is "" too.
func (s *Server) bluetoothState(ctx context.Context, serial string) (string, error) {
	out, err := s.radioShell(ctx, serial, "dumpsys bluetooth_manager")
	if err != nil && strings.Contains(out, "Can't find service") {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("dumpsys bluetooth_manager: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); len(t) > 6 && strings.EqualFold(t[:6], "state:") {
			return strings.TrimPrefix(strings.TrimSpace(t[6:]), "STATE_"), nil
		}
	}
	return "", nil
}
