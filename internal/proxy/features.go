package proxy

import "strings"

// modernSdk is assumed when a device's API level is unknown (0).
const modernSdk = 36

// FeaturesForSdk builds the host:features reply for a device at the given API
// level. Real adb intersects the host's features with the device's; the proxy
// never sees adbd's list, so it derives it from the API level at which adbd
// gained each feature. Advertising a feature the device lacks makes adb use a
// protocol the device closes on: shell_v2 on an API 17 phone loops forever in
// "- waiting for device -". abb and abb_exec are deliberately left out (the
// proxy cannot serve them), which makes clients fall back to shell:pm.
func FeaturesForSdk(sdk int32) string {
	if sdk <= 0 {
		sdk = modernSdk
	}
	features := []string{"openscreen_mdns"} // host-side, device-independent
	if sdk >= 24 {
		features = append(features, "shell_v2", "cmd")
	}
	if sdk >= 26 {
		features = append(features, "stat_v2", "ls_v2")
	}
	if sdk >= 28 {
		features = append(features, "fixed_push_mkdir")
	}
	if sdk >= 29 {
		features = append(features, "apex", "fixed_push_symlink_timestamp")
	}
	if sdk >= 30 {
		features = append(features, "remount_shell", "track_app",
			"sendrecv_v2", "sendrecv_v2_brotli", "sendrecv_v2_lz4", "sendrecv_v2_zstd", "sendrecv_v2_dry_run_send")
	}
	return strings.Join(features, ",")
}
