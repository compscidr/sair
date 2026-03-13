package updater

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/compscidr/sair/internal/version"
)

const repo = "compscidr/sair"

type releaseInfo struct {
	TagName string `json:"tag_name"`
}

// CheckAndUpdate checks GitHub for a newer release and self-updates if found.
// It is a no-op for dev builds or when SAIR_AUTO_UPDATE=false.
// On success it re-execs the new binary (never returns).
func CheckAndUpdate(binaryName string) {
	if version.Version == "dev" {
		return
	}
	if strings.EqualFold(os.Getenv("SAIR_AUTO_UPDATE"), "false") {
		return
	}

	latest, err := fetchLatestVersion()
	if err != nil {
		slog.Debug("auto-update: check failed", "error", err)
		return
	}

	current := strings.TrimPrefix(version.Version, "v")
	target := strings.TrimPrefix(latest, "v")
	if current == target {
		slog.Debug("auto-update: up to date", "version", version.Version)
		return
	}

	slog.Info("auto-update: newer version available", "current", version.Version, "latest", latest)

	execPath, err := os.Executable()
	if err != nil {
		slog.Warn("auto-update: cannot determine executable path", "error", err)
		return
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		slog.Warn("auto-update: cannot resolve symlinks", "error", err)
		return
	}

	if err := downloadAndReplace(latest, binaryName, execPath); err != nil {
		slog.Warn("auto-update: failed", "error", err)
		return
	}

	slog.Info("auto-update: updated successfully, restarting", "version", latest)
	if err := syscall.Exec(execPath, os.Args, os.Environ()); err != nil {
		slog.Error("auto-update: restart failed", "error", err)
	}
}

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Strip auth header on cross-domain redirects (GitHub -> S3)
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

func addAuth(req *http.Request) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func fetchLatestVersion() (string, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	addAuth(req)

	resp, err := newClient(10 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	return release.TagName, nil
}

func downloadAndReplace(tag, binaryName, execPath string) error {
	archiveURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/sair-%s-%s.tar.gz",
		repo, tag, runtime.GOOS, runtime.GOARCH)
	slog.Info("auto-update: downloading", "url", archiveURL)

	req, err := http.NewRequest("GET", archiveURL, nil)
	if err != nil {
		return err
	}
	addAuth(req)

	resp, err := newClient(2 * time.Minute).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("binary %s not found in archive", binaryName)
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if filepath.Base(hdr.Name) == binaryName && hdr.Typeflag == tar.TypeReg {
			return atomicReplace(execPath, tr, hdr.FileInfo().Mode())
		}
	}
}

func atomicReplace(targetPath string, src io.Reader, mode os.FileMode) error {
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".sair-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	ok = true
	return nil
}
