package proxy

import (
	"net"
	"strings"
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// TestSniffConnParsesFirstRequestAcrossReads feeds the length prefix and the
// service string in awkward chunks, as a real socket may deliver them.
func TestSniffConnParsesFirstRequestAcrossReads(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	var got string
	sc := newSniffConn(server, func(s string) { got = s })

	service := "shell,v2,raw:am instrument -w -r com.example/androidx.test.runner.AndroidJUnitRunner"
	msg := padHex(len(service)) + service + "trailing bytes the tunnel forwards untouched"
	go func() {
		client.Write([]byte(msg[:2]))
		client.Write([]byte(msg[2:7]))
		client.Write([]byte(msg[7:]))
		client.Close()
	}()

	var all []byte
	buf := make([]byte, 3)
	for {
		n, err := sc.Read(buf)
		all = append(all, buf[:n]...)
		if err != nil {
			break
		}
	}
	if string(all) != msg {
		t.Errorf("sniffing altered the stream: %q", all)
	}
	if got != service {
		t.Errorf("service not captured, got %q", got)
	}
}

func TestSniffConnIgnoresNonAdbPrefixAndCountsWrites(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	called := false
	sc := newSniffConn(server, func(string) { called = true })
	go func() { client.Write([]byte("GET / HTTP/1.1\r\n")); client.Close() }()
	buf := make([]byte, 64)
	for {
		if _, err := sc.Read(buf); err != nil {
			break
		}
	}
	if called {
		t.Error("non-ADB data must not be reported as a service")
	}

	c2, s2 := net.Pipe()
	sc2 := newSniffConn(s2, nil)
	go func() {
		rb := make([]byte, 64)
		for {
			if _, err := c2.Read(rb); err != nil {
				return
			}
		}
	}()
	sc2.Write([]byte(strings.Repeat("x", 10)))
	sc2.Write([]byte(strings.Repeat("y", 5)))
	s2.Close()
	if sc2.BytesToClient() != 15 {
		t.Errorf("bytes to client = %d, want 15", sc2.BytesToClient())
	}
}

func TestLockLogCapDrainRequeue(t *testing.T) {
	l := &LockLog{}
	for i := 0; i < maxLockLogEntries+10; i++ {
		l.Record(&pb.LockLogEntry{Service: "shell:echo"})
	}
	entries := l.Drain()
	if len(entries) != maxLockLogEntries+1 {
		t.Fatalf("expected cap+marker (%d), got %d", maxLockLogEntries+1, len(entries))
	}
	if !strings.Contains(entries[len(entries)-1].Service, "truncated") {
		t.Errorf("last entry should be the truncation marker, got %q", entries[len(entries)-1].Service)
	}
	if len(l.Drain()) != 0 {
		t.Error("drain must clear pending entries")
	}

	l.Record(&pb.LockLogEntry{Service: "b"})
	l.Requeue([]*pb.LockLogEntry{{Service: "a"}})
	out := l.Drain()
	if len(out) != 2 || out[0].Service != "a" || out[1].Service != "b" {
		t.Errorf("requeued entries must come first, got %+v", out)
	}

	long := &pb.LockLogEntry{Service: strings.Repeat("s", maxLockLogServiceLen+50)}
	l.Record(long)
	if n := len(l.Drain()[0].Service); n > maxLockLogServiceLen+3 {
		t.Errorf("service not truncated: %d", n)
	}

	var nilLog *LockLog
	nilLog.Record(&pb.LockLogEntry{})
	if nilLog.Drain() != nil {
		t.Error("nil LockLog must be a no-op")
	}
}
