package proxy

import (
	"strings"
	"testing"
)

func has(list, f string) bool {
	for _, x := range strings.Split(list, ",") {
		if x == f {
			return true
		}
	}
	return false
}

func TestFeaturesForSdk(t *testing.T) {
	old := FeaturesForSdk(17)
	for _, f := range []string{"shell_v2", "cmd", "stat_v2", "sendrecv_v2"} {
		if has(old, f) {
			t.Errorf("API 17 must not advertise %s (got %q)", f, old)
		}
	}
	n := FeaturesForSdk(24)
	if !has(n, "shell_v2") || !has(n, "cmd") || has(n, "stat_v2") || has(n, "sendrecv_v2") {
		t.Errorf("API 24 feature set wrong: %q", n)
	}
	full := FeaturesForSdk(34)
	for _, f := range []string{"shell_v2", "cmd", "stat_v2", "ls_v2", "fixed_push_mkdir", "apex", "sendrecv_v2", "track_app"} {
		if !has(full, f) {
			t.Errorf("API 34 must advertise %s (got %q)", f, full)
		}
	}
	for _, f := range []string{"abb", "abb_exec"} {
		if has(full, f) {
			t.Errorf("must never advertise %s", f)
		}
	}
	if FeaturesForSdk(0) != FeaturesForSdk(modernSdk) {
		t.Error("unknown API level should get the modern set")
	}
}
