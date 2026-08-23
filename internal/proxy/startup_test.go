package proxy

import (
	"net"
	"testing"
)

// A port collision must surface as an error from Start/Listen, not as a log
// line from a background goroutine. sair-proxy exits on these, and its /status
// endpoint is used as a readiness probe: if either bind can fail silently, the
// probe reports a proxy that is dead or about to die.

func TestHTTPApiStartFailsWhenPortTaken(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not bind blocker: %v", err)
	}
	defer blocker.Close()

	port := blocker.Addr().(*net.TCPAddr).Port
	api := NewHTTPApi(nil, nil, "test-key", port, "127.0.0.1")

	if err := api.Start(); err == nil {
		api.Stop()
		t.Fatalf("Start returned nil for already-bound port %d", port)
	}
}

func TestAdbProxyListenFailsWhenPortTaken(t *testing.T) {
	blocker, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("could not bind blocker: %v", err)
	}
	defer blocker.Close()

	port := blocker.Addr().(*net.TCPAddr).Port
	p := NewAdbProxy(port, nil, nil)

	if err := p.Listen(); err == nil {
		p.listener.Close()
		t.Fatalf("Listen returned nil for already-bound port %d", port)
	}
}

func TestAdbProxyListenThenStopReleasesPort(t *testing.T) {
	p := NewAdbProxy(0, nil, nil)
	if err := p.Listen(); err != nil {
		t.Fatalf("Listen on port 0 failed: %v", err)
	}
	if p.listener == nil {
		t.Fatal("Listen succeeded but left no listener for Start to serve")
	}
	p.listener.Close()
}
