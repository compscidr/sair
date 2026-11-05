package devicesource

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Baseline package set (sair#76): the third-party packages a phone had when
// this device source first saw it. Everything a job installs on top is
// uninstalled on release, so the next job (possibly another tenant's) never
// gets the last one's APKs, their signed-in accounts, or a suggestion their
// uid registered for the bench Wi-Fi.
//
// ponytail: the baseline lives in memory, so a device source restart while a
// job is running bakes that job's packages into the baseline until the next
// restart on a clean phone. Persist it under a state dir if that bites.

// recordBaseline takes the baseline the first time a serial is seen; a no-op
// afterwards and when BASELINE_PACKAGES overrides it. On a bench already
// running jobs, "first seen" is "whatever the last job left", so anything
// `pm list instrumentation` names (a test package and its target) is never
// baselined: nothing a bench owner installed by hand shows up there, and a
// CI job's leftovers always do.
func (s *Server) recordBaseline(serial string) {
	if s.baselineOverride != nil {
		return
	}
	s.baselineMu.Lock()
	_, ok := s.baseline[serial]
	s.baselineMu.Unlock()
	if ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := s.shell(ctx, serial, "pm list packages -3")
	if err != nil {
		s.baselineMu.Lock()
		warned := s.baselineWarned[serial]
		if s.baselineWarned == nil {
			s.baselineWarned = make(map[string]bool)
		}
		s.baselineWarned[serial] = true
		s.baselineMu.Unlock()
		if !warned {
			slog.Warn("reading baseline packages failed; releases cannot reset packages until it succeeds", "serial", serial, "error", err)
		}
		return
	}
	pkgs := parsePackages(out)
	var excluded []string
	if out, err := s.shell(ctx, serial, "pm list instrumentation"); err == nil {
		for _, pkg := range instrumentationPackages(out, pkgs) {
			delete(pkgs, pkg)
			excluded = append(excluded, pkg)
		}
	}
	s.baselineMu.Lock()
	if s.baseline == nil {
		s.baseline = make(map[string]map[string]struct{})
	}
	s.baseline[serial] = pkgs
	s.baselineMu.Unlock()
	slog.Info("recorded baseline packages; set BASELINE_PACKAGES if a previous job's are among them",
		"serial", serial, "packages", slices.Sorted(maps.Keys(pkgs)), "instrumentationExcluded", excluded)
}

// instrumentationPackages reads `pm list instrumentation` and returns, sorted,
// every test package whose target is a different package that is also
// installed, plus that target: the shape of a CI job's APK pair.
//
//	instrumentation:com.example.app.test/androidx.test.runner.AndroidJUnitRunner (target=com.example.app)
//
// A production app that ships an instrumentation of its own (Microsoft 365
// targets an uninstalled test package; some Samsung apps target themselves)
// is not that shape and stays in the baseline.
func instrumentationPackages(out string, installed map[string]struct{}) []string {
	set := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "instrumentation:")
		if !ok {
			continue
		}
		pkg, _, _ := strings.Cut(rest, "/")
		_, target, _ := strings.Cut(rest, "(target=")
		target = strings.TrimSuffix(strings.TrimSpace(target), ")")
		_, pkgInstalled := installed[pkg]
		_, targetInstalled := installed[target]
		if pkg == "" || target == "" || pkg == target || !pkgInstalled || !targetInstalled {
			continue
		}
		set[pkg] = struct{}{}
		set[target] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

func (s *Server) baselineFor(serial string) map[string]struct{} {
	if s.baselineOverride != nil {
		return s.baselineOverride
	}
	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()
	return s.baseline[serial]
}

// ListPackages is the phone's third-party packages, for a later ResetPackages
// to keep. Anything but a clean listing is an error: taken for an empty one, it
// would have that reset uninstall every app the phone's owner has.
func (s *Server) ListPackages(ctx context.Context, req *pb.ListPackagesRequest) (*pb.ListPackagesResponse, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	out, err := s.shell(ctx, req.Serial, "pm list packages -3")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: list packages: %v", req.Serial, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "package:") {
			return nil, status.Errorf(codes.Internal, "%s: list packages: %s", req.Serial, line)
		}
	}
	return &pb.ListPackagesResponse{Packages: slices.Sorted(maps.Keys(parsePackages(out)))}, nil
}

// ResetPackages uninstalls every third-party package not in the baseline, or
// not in req.Since when that is present (what a relayed phone had before its
// lock; then no baseline is needed), then force-stops and clears the configured
// apps. Reports what went so the lock log shows what a job left behind. A package that refuses to go is
// logged and skipped: one stubborn APK must not keep the rest on the phone.
func (s *Server) ResetPackages(ctx context.Context, req *pb.ResetPackagesRequest) (*pb.ResetPackagesResponse, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	var keep map[string]struct{}
	if req.Since != nil {
		keep = packageSet(req.Since.Packages)
	} else if keep = s.baselineFor(req.Serial); keep == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "no baseline package set recorded for %s yet", req.Serial)
	}
	// Suggestions are read before and after so the ones a removed package
	// owned can be named: Android drops a package's suggestions with it.
	before, _ := s.shell(ctx, req.Serial, "cmd wifi list-all-suggestions")

	out, err := s.shell(ctx, req.Serial, "pm list packages -3")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: list packages: %v", req.Serial, err)
	}
	resp := &pb.ResetPackagesResponse{}
	for _, pkg := range slices.Sorted(maps.Keys(parsePackages(out))) {
		if _, ok := keep[pkg]; ok {
			continue
		}
		out, err := s.shell(ctx, req.Serial, "pm uninstall "+shellQuote(pkg))
		if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "Success") {
			slog.Warn("uninstall failed; package stays on the phone", "serial", req.Serial, "package", pkg, "output", strings.TrimSpace(out), "error", err)
			continue
		}
		resp.Removed = append(resp.Removed, pkg)
	}
	for _, pkg := range s.stopPackages {
		if _, err := s.shell(ctx, req.Serial, "am force-stop "+shellQuote(pkg)); err != nil {
			slog.Debug("force-stop failed", "serial", req.Serial, "package", pkg, "error", err)
		}
	}
	for _, pkg := range s.clearPackages {
		if _, err := s.shell(ctx, req.Serial, "pm clear "+shellQuote(pkg)); err != nil {
			slog.Warn("clearing app data failed", "serial", req.Serial, "package", pkg, "error", err)
		}
	}
	if len(resp.Removed) > 0 && before != "" {
		after, _ := s.shell(ctx, req.Serial, "cmd wifi list-all-suggestions")
		resp.SuggestionsRemoved = linesGone(before, after)
	}
	if len(resp.Removed) > 0 {
		slog.Info("uninstalled packages a job left behind", "serial", req.Serial, "packages", resp.Removed, "suggestionsRemoved", resp.SuggestionsRemoved)
	}
	return resp, nil
}

// parsePackages reads `pm list packages`: one "package:<name>" per line.
func parsePackages(out string) map[string]struct{} {
	pkgs := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "package:"); ok && name != "" {
			pkgs[name] = struct{}{}
		}
	}
	return pkgs
}

// linesGone returns the non-empty, non-header lines of before that are
// missing from after, trimmed, in order.
func linesGone(before, after string) []string {
	have := make(map[string]struct{})
	for _, line := range strings.Split(after, "\n") {
		have[strings.TrimSpace(line)] = struct{}{}
	}
	var gone []string
	for _, line := range strings.Split(before, "\n") {
		line = strings.TrimSpace(line)
		if _, ok := have[line]; ok || line == "" {
			continue
		}
		gone = append(gone, line)
	}
	return gone
}

// packageList parses a comma-separated env value into package names.
func packageList(v string) []string {
	var pkgs []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}

func packageSet(pkgs []string) map[string]struct{} {
	if pkgs == nil {
		return nil
	}
	set := make(map[string]struct{}, len(pkgs))
	for _, p := range pkgs {
		set[p] = struct{}{}
	}
	return set
}
