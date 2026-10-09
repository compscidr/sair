package devicesource

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeShell records commands and fails any whose prefix is in fail.
func fakeShell(s *Server, outputs map[string]string, fail ...string) *[]string {
	var cmds []string
	s.shell = func(_ context.Context, _, command string) (string, error) {
		cmds = append(cmds, command)
		for _, f := range fail {
			if strings.HasPrefix(command, f) {
				return "", errors.New("exit status 1")
			}
		}
		return outputs[command], nil
	}
	return &cmds
}

func TestSetWifi(t *testing.T) {
	listing := "Network Id      SSID                         Security type\n0            bench                          wpa2-psk\n3            other                          open\n"
	tests := []struct {
		name  string
		state pb.WifiRequest_State
		fail  []string
		want  []string
	}{
		{"reset", pb.WifiRequest_RESET, nil, []string{
			"cmd wifi remove-all-suggestions", "cmd wifi list-networks",
			"cmd wifi forget-network 0", "cmd wifi forget-network 3",
			"svc wifi disable", "svc wifi enable",
		}},
		{"off falls back to the bench SSID", pb.WifiRequest_OFF, []string{"cmd wifi remove-all"}, []string{
			"cmd wifi remove-all-suggestions", `cmd wifi remove-suggestion 'it'\''s here'`, "cmd wifi list-networks",
			"cmd wifi forget-network 0", "cmd wifi forget-network 3",
		}},
		{"on", pb.WifiRequest_ON, nil, []string{
			`cmd wifi add-suggestion 'it'\''s here' wpa2 'pass word'`, "cmd wifi start-scan", "ping -c1 -W2 '8.8.8.8'",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{wifiSSID: "it's here", wifiPassphrase: "pass word", wifiPingHost: "8.8.8.8", wifiTimeout: time.Second}
			cmds := fakeShell(s, map[string]string{"cmd wifi list-networks": listing}, tt.fail...)
			if _, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: tt.state}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*cmds, tt.want) {
				t.Errorf("commands:\n got %q\nwant %q", *cmds, tt.want)
			}
		})
	}
}

func TestSetWifiOnTimesOutWithoutInternet(t *testing.T) {
	s := &Server{wifiSSID: "bench", wifiPingHost: "8.8.8.8", wifiTimeout: time.Millisecond}
	cmds := fakeShell(s, nil, "ping")
	_, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: pb.WifiRequest_ON})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if (*cmds)[0] != "cmd wifi add-suggestion 'bench' open" {
		t.Errorf("no passphrase should suggest an open network, got %q", (*cmds)[0])
	}
}

func TestSetWifiUnmanagedWithoutSSID(t *testing.T) {
	s := &Server{}
	cmds := fakeShell(s, nil)
	_, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: pb.WifiRequest_RESET})
	if status.Code(err) != codes.FailedPrecondition || len(*cmds) != 0 {
		t.Fatalf("err = %v, cmds = %q; want FailedPrecondition and no commands", err, *cmds)
	}
}
