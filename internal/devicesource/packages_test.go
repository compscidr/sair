package devicesource

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// The last job's app and test package are still on the phone when the
	// device source first sees it; pm list instrumentation names both.
	basePackages    = "package:com.bench.keep\npackage:com.example.app.test\npackage:com.vendor.tool\npackage:com.example.app\n"
	instrumentation = "instrumentation:com.example.app.test/androidx.test.runner.AndroidJUnitRunner (target=com.example.app)\n"
	jobPackages     = "package:com.bench.keep\npackage:com.example.app.test\npackage:com.vendor.tool\npackage:com.example.app\npackage:com.stubborn\n"
	suggestions     = "SSID                             Security type(s)\nbench                            wpa2-psk\nDIRECT-TEST-ab                   wpa2-psk\n"
)

// The baseline is read once per phone and reused, minus the instrumentation
// packages a previous job left; a release then removes exactly what is not
// baseline, in order, reports it with the Wi-Fi suggestions that went, and
// keeps going past a package that will not go.
func TestResetPackagesRemovesWhatTheJobAdded(t *testing.T) {
	s := &Server{stopPackages: []string{"com.android.chrome"}, clearPackages: []string{"com.android.chrome"}}
	outputs := map[string]string{
		"pm list packages -3":                 basePackages,
		"pm list instrumentation":             instrumentation,
		"pm uninstall 'com.example.app'":      "Success\n",
		"pm uninstall 'com.example.app.test'": "Success\n",
		"pm uninstall 'com.stubborn'":         "Failure [DELETE_FAILED_DEVICE_POLICY_MANAGER]\n",
		"cmd wifi list-all-suggestions":       suggestions,
	}
	cmds := fakeShell(s, outputs)
	s.recordBaseline("ABC")
	s.recordBaseline("ABC")
	if got := len(*cmds); got != 2 {
		t.Fatalf("baseline read %d commands, want 2 (packages, instrumentation): %q", got, *cmds)
	}

	// The job installed its app and test package; one more refuses to go.
	outputs["pm list packages -3"] = jobPackages
	after := strings.Replace(suggestions, "bench                            wpa2-psk\n", "", 1)
	calls := 0
	inner := s.shell
	s.shell = func(ctx context.Context, serial, command string) (string, error) {
		out, err := inner(ctx, serial, command)
		if command == "cmd wifi list-all-suggestions" {
			if calls++; calls == 2 {
				return after, nil
			}
		}
		return out, err
	}
	resp, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"com.example.app", "com.example.app.test"}; !reflect.DeepEqual(resp.Removed, want) {
		t.Errorf("removed = %v, want %v", resp.Removed, want)
	}
	if want := []string{"bench                            wpa2-psk"}; !reflect.DeepEqual(resp.SuggestionsRemoved, want) {
		t.Errorf("suggestions removed = %q, want %q", resp.SuggestionsRemoved, want)
	}
	want := []string{
		"pm list packages -3", "pm list instrumentation", // the baseline
		"cmd wifi list-all-suggestions", "pm list packages -3",
		"pm uninstall 'com.example.app'", "pm uninstall 'com.example.app.test'", "pm uninstall 'com.stubborn'",
		"am force-stop 'com.android.chrome'", "pm clear 'com.android.chrome'",
		"cmd wifi list-all-suggestions",
	}
	if !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands:\n got %q\nwant %q", *cmds, want)
	}
}

func TestResetPackagesNeedsABaseline(t *testing.T) {
	s := &Server{}
	cmds := fakeShell(s, nil)
	_, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC"})
	if status.Code(err) != codes.FailedPrecondition || len(*cmds) != 0 {
		t.Fatalf("err = %v, cmds = %q; want FailedPrecondition and no commands", err, *cmds)
	}
}

// A relayed lock's snapshot replaces the baseline: what the phone had before
// the lock stays and anything else goes, even a package in the baseline (a
// reinstall during the lock). It needs no baseline, and the rest of the reset
// still runs.
func TestResetPackagesSince(t *testing.T) {
	const listed = "package:com.bench.keep\npackage:com.owner.hand\npackage:com.job.app\n"
	uninstalls := map[string]string{
		"pm list packages -3":           listed,
		"pm uninstall 'com.bench.keep'": "Success",
		"pm uninstall 'com.owner.hand'": "Success",
		"pm uninstall 'com.job.app'":    "Success",
	}
	base := map[string]map[string]struct{}{"ABC": packageSet([]string{"com.bench.keep"})}
	for _, tt := range []struct {
		name     string
		baseline map[string]map[string]struct{}
		since    *pb.PackageSnapshot
		want     []string
	}{
		{"keeps the snapshot, not the baseline", base, &pb.PackageSnapshot{Packages: []string{"com.owner.hand"}}, []string{"com.bench.keep", "com.job.app"}},
		{"empty keeps nothing", base, &pb.PackageSnapshot{}, []string{"com.bench.keep", "com.job.app", "com.owner.hand"}},
		{"absent uses the baseline", base, nil, []string{"com.job.app", "com.owner.hand"}},
		{"no baseline needed", nil, &pb.PackageSnapshot{Packages: []string{"com.bench.keep", "com.owner.hand"}}, []string{"com.job.app"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{baseline: tt.baseline, stopPackages: []string{"com.android.chrome"}}
			cmds := fakeShell(s, uninstalls)
			resp, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC", Since: tt.since})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resp.Removed, tt.want) {
				t.Errorf("removed = %v, want %v", resp.Removed, tt.want)
			}
			if !slices.Contains(*cmds, "am force-stop 'com.android.chrome'") {
				t.Errorf("rest of the reset did not run: %q", *cmds)
			}
		})
	}
}

func TestListPackages(t *testing.T) {
	s := &Server{}
	fakeShell(s, map[string]string{"pm list packages -3": "package:com.z\npackage:com.a\npackage:com.m\n"})
	resp, err := s.ListPackages(context.Background(), &pb.ListPackagesRequest{Serial: "ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"com.a", "com.m", "com.z"}; !reflect.DeepEqual(resp.Packages, want) {
		t.Errorf("packages = %v, want %v", resp.Packages, want)
	}
}

// A listing that failed without saying so must not read as "no packages": a
// reset would keep nothing and take the owner's apps with it.
func TestListPackagesRefusesAnythingButAListing(t *testing.T) {
	s := &Server{}
	fakeShell(s, map[string]string{"pm list packages -3": "Error: Could not access the Package Manager.  Is the system running?\n"})
	if _, err := s.ListPackages(context.Background(), &pb.ListPackagesRequest{Serial: "ABC"}); status.Code(err) != codes.Internal {
		t.Errorf("err = %v, want Internal", err)
	}
	fakeShell(s, nil) // no third-party packages at all is a fine answer
	resp, err := s.ListPackages(context.Background(), &pb.ListPackagesRequest{Serial: "ABC"})
	if err != nil || len(resp.Packages) != 0 {
		t.Errorf("empty listing: packages = %v, err = %v", resp.GetPackages(), err)
	}
	fakeShell(s, nil, "pm list packages")
	if _, err := s.ListPackages(context.Background(), &pb.ListPackagesRequest{Serial: "ABC"}); status.Code(err) != codes.Internal {
		t.Errorf("shell failure: err = %v, want Internal", err)
	}
}

// BASELINE_PACKAGES replaces the recorded set for every phone; nothing is
// read on first sight, and only the listed packages survive a release.
func TestResetPackagesHonoursTheOverride(t *testing.T) {
	s := &Server{baselineOverride: packageSet(packageList(" com.bench.keep ,com.vendor.tool"))}
	cmds := fakeShell(s, map[string]string{
		"pm list packages -3":                 jobPackages,
		"pm uninstall 'com.example.app'":      "Success",
		"pm uninstall 'com.example.app.test'": "Success",
		"pm uninstall 'com.stubborn'":         "Success",
	})
	s.recordBaseline("ABC")
	if len(*cmds) != 0 {
		t.Fatalf("baseline read despite the override: %q", *cmds)
	}
	resp, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"com.example.app", "com.example.app.test", "com.stubborn"}; !reflect.DeepEqual(resp.Removed, want) {
		t.Errorf("removed = %v, want %v", resp.Removed, want)
	}
	if resp.SuggestionsRemoved != nil {
		t.Errorf("no suggestion listing, yet reported %q", resp.SuggestionsRemoved)
	}
}

// Only a test package with a different, installed target is a CI leftover.
// Seen on the cube (sair#89 rollout): Microsoft 365 declares an instrumentation
// targeting an uninstalled test package, and a Samsung app targets itself;
// neither may be uninstalled on release.
func TestInstrumentationPackages(t *testing.T) {
	out := "instrumentation:com.example.app.test/androidx.test.runner.AndroidJUnitRunner (target=com.example.app)\n" +
		"instrumentation:com.microsoft.office.officehubrow/android.test.InstrumentationTestRunner (target=com.microsoft.office.msohttp.tests)\n" +
		"instrumentation:com.samsung.android.app.dressroom/androidx.test.runner.AndroidJUnitRunner (target=com.samsung.android.app.dressroom)\n" +
		"instrumentation:com.other.tests/.Runner (target=com.other)\n"
	installed := parsePackages("package:com.example.app\npackage:com.example.app.test\npackage:com.microsoft.office.officehubrow\npackage:com.samsung.android.app.dressroom\npackage:com.other.tests\n")
	want := []string{"com.example.app", "com.example.app.test"}
	if got := instrumentationPackages(out, installed); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
