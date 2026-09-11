package proxy

import (
	"os"
	"strings"
)

// ProxyID identifies this proxy within its tenant so several machines can
// share one API key. PROXY_ID wins; otherwise the hostname. "unknown" only if
// the OS cannot say, so the orchestrator never sees an empty id.
func ProxyID() string {
	if v := strings.TrimSpace(os.Getenv("PROXY_ID")); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
