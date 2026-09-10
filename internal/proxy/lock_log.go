package proxy

import (
	"encoding/hex"
	"net"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

const (
	// maxLockLogEntries caps what the proxy keeps per lock between drains, so a
	// runaway client cannot grow memory without bound.
	maxLockLogEntries = 2000
	// maxLockLogServiceLen caps a single service string (an `am instrument`
	// line with many -e args is a few hundred bytes; 1000 keeps it readable).
	maxLockLogServiceLen = 1000
)

// LockLog collects the ADB requests made on one scoped port until they are
// drained into a heartbeat or the release call.
type LockLog struct {
	mu        sync.Mutex
	pending   []*pb.LockLogEntry
	truncated bool
}

// Record appends an entry, truncating the service string and dropping the
// entry (with a single marker) once the cap is reached.
func (l *LockLog) Record(e *pb.LockLogEntry) {
	if l == nil || e == nil {
		return
	}
	const ellipsis = "…"
	if len(e.Service) > maxLockLogServiceLen {
		// Stay within the byte cap including the marker.
		e.Service = e.Service[:maxLockLogServiceLen-len(ellipsis)] + ellipsis
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) >= maxLockLogEntries {
		if !l.truncated {
			l.truncated = true
			l.pending = append(l.pending, &pb.LockLogEntry{
				TimestampMs: e.TimestampMs,
				Service:     "(log truncated: too many requests between heartbeats)",
			})
		}
		return
	}
	l.pending = append(l.pending, e)
}

// Drain returns everything recorded since the last drain and clears it.
func (l *LockLog) Drain() []*pb.LockLogEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.pending
	l.pending = nil
	l.truncated = false
	return out
}

// Requeue puts drained entries back at the front after a failed send.
func (l *LockLog) Requeue(entries []*pb.LockLogEntry) {
	if l == nil || len(entries) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(entries, l.pending...)
}

// sniffConn wraps a client connection that is about to be tunneled to a
// device. It reads the first ADB request (a 4-hex-digit length followed by the
// service string) off the client side without altering the stream, and counts
// the bytes written back to the client.
type sniffConn struct {
	net.Conn
	header        []byte
	want          int
	service       []byte
	done          bool
	onService     func(service string)
	bytesToClient atomic.Int64
}

func newSniffConn(conn net.Conn, onService func(string)) *sniffConn {
	return &sniffConn{Conn: conn, onService: onService}
}

func (s *sniffConn) Read(p []byte) (int, error) {
	n, err := s.Conn.Read(p)
	if n > 0 && !s.done {
		s.feed(p[:n])
	}
	return n, err
}

func (s *sniffConn) Write(p []byte) (int, error) {
	n, err := s.Conn.Write(p)
	s.bytesToClient.Add(int64(n))
	return n, err
}

func (s *sniffConn) feed(data []byte) {
	for len(data) > 0 && !s.done {
		if s.want == 0 {
			need := 4 - len(s.header)
			if need > len(data) {
				need = len(data)
			}
			s.header = append(s.header, data[:need]...)
			data = data[need:]
			if len(s.header) < 4 {
				return
			}
			raw, err := hex.DecodeString(string(s.header))
			if err != nil || len(raw) != 2 {
				s.done = true // not an ADB request; leave the stream alone
				return
			}
			s.want = int(raw[0])<<8 | int(raw[1])
			if s.want == 0 {
				s.done = true
				return
			}
			continue
		}
		need := s.want - len(s.service)
		if need > len(data) {
			need = len(data)
		}
		s.service = append(s.service, data[:need]...)
		data = data[need:]
		if len(s.service) == s.want {
			s.done = true
			if s.onService != nil {
				s.onService(string(s.service))
			}
		}
	}
}

// BytesToClient is how many bytes the device side has sent back so far.
func (s *sniffConn) BytesToClient() int64 { return s.bytesToClient.Load() }

// tunnelObserver records one device session into a LockLog: created before
// the tunnel starts, finished when it ends.
type tunnelObserver struct {
	log     *LockLog
	serial  string
	entry   *pb.LockLogEntry
	started time.Time
	conn    *sniffConn
}

func newTunnelObserver(log *LockLog, serial string, conn net.Conn) (net.Conn, *tunnelObserver) {
	if log == nil {
		return conn, nil
	}
	o := &tunnelObserver{log: log, serial: serial}
	o.conn = newSniffConn(conn, func(service string) {
		o.started = time.Now()
		o.entry = &pb.LockLogEntry{TimestampMs: o.started.UnixMilli(), Serial: serial, Service: service}
	})
	return o.conn, o
}

// finish records the session if a request was seen.
func (o *tunnelObserver) finish() {
	if o == nil || o.entry == nil {
		return
	}
	o.entry.DurationMs = time.Since(o.started).Milliseconds()
	o.entry.BytesToClient = o.conn.BytesToClient()
	o.log.Record(o.entry)
}
