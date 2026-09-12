package proxy

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

const (
	// maxLockLogEntries and maxLockLogPendingBytes cap what the proxy keeps
	// per lock between drains, so a runaway client cannot grow memory without
	// bound. The byte cap is what the release call may carry in one message;
	// the orchestrator's receive limit is sized for it plus what open tunnels
	// hold (maxUnflushedOutput each).
	maxLockLogEntries      = 2000
	maxLockLogPendingBytes = 32 << 20
	// maxLockLogServiceLen caps a single service string (an `am instrument`
	// line with many -e args is a few hundred bytes; 1000 keeps it readable).
	maxLockLogServiceLen = 1000
	// maxLockLogDrainBytes bounds the output one Drain hands back, so the
	// message carrying it (a LockLog on the stream, or a unary heartbeat)
	// stays well inside gRPC's 4 MB default receive limit.
	maxLockLogDrainBytes = 2 << 20
	// maxOutputPiece is the largest single output entry: pieces this size
	// always fit a drain, so one request's output never wedges the log.
	maxOutputPiece = maxLockLogDrainBytes / 2
	// maxUnflushedOutput caps what one open tunnel holds between drains (the
	// session down for minutes under a logcat); past it output is dropped and
	// the request ends with one marker saying how much.
	maxUnflushedOutput = 8 << 20
	binaryOutputMarker = "(binary output omitted)"
)

// LockLog collects the ADB requests made on one scoped port until they are
// drained into a heartbeat or the release call. Requests still running are
// kept in open so their output can be drained while they run.
type LockLog struct {
	mu           sync.Mutex
	pending      []*pb.LockLogEntry
	pendingBytes int
	truncated    bool
	nextID       uint64
	open         map[*tunnelObserver]struct{}
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
	if len(l.pending) >= maxLockLogEntries || l.pendingBytes+len(e.Output) > maxLockLogPendingBytes {
		if !l.truncated {
			l.truncated = true
			l.pending = append(l.pending, &pb.LockLogEntry{
				TimestampMs: e.TimestampMs,
				Service:     "(log truncated: too much logged between heartbeats)",
			})
		}
		return
	}
	l.pending = append(l.pending, e)
	l.pendingBytes += len(e.Output)
}

// Drain returns what was recorded since the last drain, followed by the
// output that still-running requests produced meanwhile, up to
// maxLockLogDrainBytes of output; anything past the budget waits for the
// next drain. Always returns at least one pending entry when there is one.
func (l *LockLog) Drain() []*pb.LockLogEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	budget := maxLockLogDrainBytes
	var out []*pb.LockLogEntry
	n := 0
	for ; n < len(l.pending); n++ {
		e := l.pending[n]
		if len(e.Output) > budget && len(out) > 0 {
			break
		}
		out = append(out, e)
		budget -= len(e.Output)
		l.pendingBytes -= len(e.Output)
	}
	l.pending = append([]*pb.LockLogEntry(nil), l.pending[n:]...)
	if len(l.pending) == 0 {
		l.pending = nil
		l.truncated = false
	}
	for o := range l.open {
		if budget <= 0 {
			break
		}
		if piece := o.take(budget, false); piece != "" {
			out = append(out, &pb.LockLogEntry{RequestId: o.entry.RequestId, Output: piece})
			budget -= len(piece)
		}
	}
	return out
}

// DrainAll returns everything, for the release call, which is one unary
// message: at most maxLockLogPendingBytes plus what open tunnels hold.
func (l *LockLog) DrainAll() []*pb.LockLogEntry {
	var all []*pb.LockLogEntry
	for {
		batch := l.Drain()
		if len(batch) == 0 {
			return all
		}
		all = append(all, batch...)
	}
}

// Requeue puts drained entries back at the front after a failed send.
func (l *LockLog) Requeue(entries []*pb.LockLogEntry) {
	if l == nil || len(entries) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(entries, l.pending...)
	for _, e := range entries {
		l.pendingBytes += len(e.Output)
	}
}

func (l *LockLog) register(o *tunnelObserver) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open == nil {
		l.open = map[*tunnelObserver]struct{}{}
	}
	l.open[o] = struct{}{}
	l.nextID++
	return l.nextID
}

func (l *LockLog) unregister(o *tunnelObserver) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.open, o)
}

// sniffConn wraps a client connection that is about to be tunneled to a
// device. It reads the first ADB request (a 4-hex-digit length followed by the
// service string) off the client side without altering the stream, counts the
// bytes written back to the client and hands them to onReply.
type sniffConn struct {
	net.Conn
	header        []byte
	want          int
	service       []byte
	done          bool
	onService     func(service string)
	onReply       func(p []byte)
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
	if n > 0 && s.onReply != nil {
		s.onReply(p[:n])
	}
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

// CloseWrite half-closes the wrapped socket so the relay can forward the
// device's end to the client (sniffConn embeds the net.Conn interface, which
// hides the *net.TCPConn's CloseWrite).
func (s *sniffConn) CloseWrite() error {
	if cw, ok := s.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// tunnelObserver records one device session into a LockLog: an open entry
// when the request is seen, output pieces while it runs (shell services
// only), and a close entry when the tunnel ends.
type tunnelObserver struct {
	log     *LockLog
	serial  string
	entry   *pb.LockLogEntry
	started time.Time
	conn    *sniffConn

	mu      sync.Mutex
	capture bool   // a shell service: keep what comes back
	v2      bool   // shell v2: output arrives in stdout/stderr packets
	status  []byte // the reply's first four bytes, OKAY or FAIL
	skip    int    // bytes still to skip after FAIL (its hex length)
	hdr     []byte // v2 packet header in progress
	left    int    // payload bytes left in the current v2 packet
	keep    bool   // the current v2 packet is stdout or stderr
	buf     []byte // decoded output not yet drained
	dropped int    // bytes dropped past maxUnflushedOutput
	binary  bool   // output was not UTF-8; nothing more is kept
}

func newTunnelObserver(log *LockLog, serial string, conn net.Conn) (net.Conn, *tunnelObserver) {
	if log == nil {
		return conn, nil
	}
	o := &tunnelObserver{log: log, serial: serial}
	o.conn = newSniffConn(conn, func(service string) {
		o.started = time.Now()
		o.entry = &pb.LockLogEntry{TimestampMs: o.started.UnixMilli(), Serial: serial, Service: service}
		o.mu.Lock()
		o.capture = strings.HasPrefix(service, "shell")
		o.v2 = strings.HasPrefix(service, "shell,v2")
		o.mu.Unlock()
		o.entry.RequestId = log.register(o)
		log.Record(&pb.LockLogEntry{TimestampMs: o.entry.TimestampMs, Serial: serial, Service: service, RequestId: o.entry.RequestId, Open: true})
	})
	o.conn.onReply = o.observe
	return o.conn, o
}

// observe decodes what the device sent back into buf: past the OKAY (or the
// FAIL and its length), raw for a legacy shell, stdout/stderr payloads for
// shell v2. A reply that is neither OKAY nor FAIL is not ADB; stop looking.
func (o *tunnelObserver) observe(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(p) > 0 && o.capture {
		switch {
		case len(o.status) < 4:
			need := min(4-len(o.status), len(p))
			o.status = append(o.status, p[:need]...)
			p = p[need:]
			if len(o.status) < 4 {
				return
			}
			switch string(o.status) {
			case "OKAY":
			case "FAIL":
				o.v2, o.skip = false, 4
			default:
				o.capture = false
			}
		case o.skip > 0:
			n := min(o.skip, len(p))
			o.skip -= n
			p = p[n:]
		case !o.v2:
			o.append(p)
			return
		case o.left == 0:
			need := min(5-len(o.hdr), len(p))
			o.hdr = append(o.hdr, p[:need]...)
			p = p[need:]
			if len(o.hdr) < 5 {
				return
			}
			o.left = int(binary.LittleEndian.Uint32(o.hdr[1:]))
			o.keep = o.hdr[0] == 1 || o.hdr[0] == 2
			o.hdr = o.hdr[:0]
		default:
			n := min(o.left, len(p))
			if o.keep {
				o.append(p[:n])
			}
			o.left -= n
			p = p[n:]
		}
	}
}

func (o *tunnelObserver) append(p []byte) {
	if o.binary {
		return
	}
	if room := maxUnflushedOutput - len(o.buf); len(p) > room {
		o.dropped += len(p) - max(room, 0)
		p = p[:max(room, 0)]
	}
	o.buf = append(o.buf, p...)
}

// take returns up to max bytes of buffered output as text, cut on a rune
// boundary so a character split across pieces stays whole (final takes the
// tail as is). Non-UTF-8 output makes the request binary: this piece is the
// marker, and nothing further is kept.
func (o *tunnelObserver) take(max int, final bool) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.buf) == 0 {
		if final && o.dropped > 0 {
			n := o.dropped
			o.dropped = 0
			return fmt.Sprintf("\n(%d bytes of output dropped)\n", n)
		}
		return ""
	}
	cut := min(max, len(o.buf))
	if !final || cut < len(o.buf) {
		for i := cut - 1; i >= 0 && i >= cut-utf8.UTFMax; i-- {
			if utf8.RuneStart(o.buf[i]) {
				if !utf8.FullRune(o.buf[i:cut]) {
					cut = i
				}
				break
			}
		}
	}
	piece := o.buf[:cut]
	if !utf8.Valid(piece) {
		o.binary, o.buf, o.dropped = true, nil, 0
		return binaryOutputMarker
	}
	o.buf = append([]byte(nil), o.buf[cut:]...)
	if len(o.buf) == 0 {
		o.buf = nil
	}
	return string(piece)
}

// finish records the rest of the output and the session itself, if a request
// was seen.
func (o *tunnelObserver) finish() {
	if o == nil || o.entry == nil {
		return
	}
	o.log.unregister(o)
	for {
		piece := o.take(maxOutputPiece, true)
		if piece == "" {
			break
		}
		o.log.Record(&pb.LockLogEntry{RequestId: o.entry.RequestId, Output: piece})
	}
	o.entry.DurationMs = time.Since(o.started).Milliseconds()
	o.entry.BytesToClient = o.conn.BytesToClient()
	o.log.Record(o.entry)
}
