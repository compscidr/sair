package proxy

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// v2 frames one shell-v2 packet: id, little-endian length, payload.
func v2(id byte, payload string) []byte {
	b := []byte{id, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[1:], uint32(len(payload)))
	return append(b, payload...)
}

// request opens an observed tunnel for service and returns the device-side
// writer: what it writes is what the device sent back to the client.
func request(t *testing.T, log *LockLog, service string) (device net.Conn, obs *tunnelObserver) {
	t.Helper()
	client, server := net.Pipe()
	conn, obs := newTunnelObserver(log, "SERIAL", server)
	go func() { // the client's side of the pipe: send the request, swallow the reply
		client.Write([]byte(padHex(len(service)) + service))
		buf := make([]byte, 1024)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil { // sniff the request
		t.Fatal(err)
	}
	return conn, obs
}

func outputs(entries []*pb.LockLogEntry) (out string) {
	for _, e := range entries {
		out += e.Output
	}
	return out
}

func TestShellV2OutputStreamsInPiecesUnderOneRequestID(t *testing.T) {
	log := &LockLog{}
	device, obs := request(t, log, "shell,v2,raw:logcat")

	entries := log.Drain()
	if len(entries) != 1 || !entries[0].Open || entries[0].Service != "shell,v2,raw:logcat" || entries[0].RequestId == 0 {
		t.Fatalf("want one open entry with a request id, got %+v", entries)
	}
	id := entries[0].RequestId

	// OKAY, then stdout/stderr frames split at awkward points across writes.
	frames := append(v2(1, "line one\n"), v2(2, "warn\n")...)
	frames = append(frames, v2(3, "\x00")...) // exit status is not output
	device.Write([]byte("OK"))
	device.Write(append([]byte("AY"), frames[:7]...))
	device.Write(frames[7:12])

	entries = log.Drain()
	if got := outputs(entries); got != "line on" || entries[0].RequestId != id || entries[0].Open || entries[0].Service != "" {
		t.Fatalf("want the output seen so far as one piece for request %d, got %q in %+v", id, got, entries)
	}
	device.Write(frames[12:])
	obs.finish()
	entries = log.Drain()
	if got := outputs(entries); got != "e\nwarn\n" {
		t.Errorf("want the rest of the output (exit frame excluded), got %q", got)
	}
	last := entries[len(entries)-1]
	if last.RequestId != id || last.Open || last.Output != "" || last.Service != "shell,v2,raw:logcat" || last.BytesToClient != int64(4+len(frames)) {
		t.Errorf("want a close entry for request %d with bytes and service, got %+v", id, last)
	}
}

func TestLegacyShellOutputIsRawAndFailIsKept(t *testing.T) {
	log := &LockLog{}
	device, obs := request(t, log, "shell:ls")
	device.Write([]byte("OKAYa\r\nb\r\n"))
	obs.finish()
	if got := outputs(log.Drain()); got != "a\r\nb\r\n" {
		t.Errorf("legacy shell output is passed through raw, got %q", got)
	}

	device, obs = request(t, log, "shell:x")
	device.Write([]byte("FAIL0006closed"))
	obs.finish()
	if got := outputs(log.Drain()); got != "closed" {
		t.Errorf("a FAIL reply's message is the output, got %q", got)
	}
}

func TestBinaryServicesAndBinaryOutputAreNotCaptured(t *testing.T) {
	log := &LockLog{}
	device, obs := request(t, log, "sync:")
	device.Write([]byte("OKAY\x00\x01\x02binary"))
	obs.finish()
	entries := log.Drain()
	if got := outputs(entries); got != "" {
		t.Errorf("sync output must not be captured, got %q", got)
	}
	if entries[len(entries)-1].BytesToClient != 13 {
		t.Errorf("bytes are still counted, got %+v", entries[len(entries)-1])
	}

	device, obs = request(t, log, "shell,v2,raw:cat blob")
	device.Write(append([]byte("OKAY"), v2(1, "\xff\xfe\x00junk")...))
	device.Write(v2(1, "\xff more"))
	all := outputs(log.Drain())
	device.Write(v2(1, "\xff\xff"))
	obs.finish()
	all += outputs(log.Drain())
	if n := strings.Count(all, "(binary output omitted)"); n != 1 || len(all) != len("(binary output omitted)") {
		t.Errorf("non-UTF-8 output is one marker per request and nothing else, got %q", all)
	}
}

func TestOutputHoldsBackSplitRunes(t *testing.T) {
	log := &LockLog{}
	device, obs := request(t, log, "shell,v2,raw:echo")
	s := "héllo"
	device.Write(append([]byte("OKAY"), v2(1, s[:2])...)) // "h" + first byte of é
	first := outputs(log.Drain())
	device.Write(v2(1, s[2:]))
	obs.finish()
	if got := first + outputs(log.Drain()); got != s || first != "h" {
		t.Errorf("a rune split across pieces stays whole: first %q, all %q", first, got)
	}
}

func TestDrainBudgetLeavesTheRestForNextTime(t *testing.T) {
	log := &LockLog{}
	device, obs := request(t, log, "shell:big")
	device.Write([]byte("OKAY" + strings.Repeat("x", maxLockLogDrainBytes+100)))
	obs.finish()
	first := log.Drain()
	if size := len(outputs(first)); size > maxLockLogDrainBytes {
		t.Errorf("one drain must stay within the budget, got %d bytes", size)
	}
	rest := log.Drain()
	if got := len(outputs(first)) + len(outputs(rest)); got != maxLockLogDrainBytes+100 {
		t.Errorf("nothing is lost across drains, got %d bytes", got)
	}
	if rest[len(rest)-1].Service != "shell:big" {
		t.Errorf("the close entry comes last, got %+v", rest[len(rest)-1])
	}
	if len(log.Drain()) != 0 {
		t.Error("nothing left")
	}
}

func TestUnflushedOutputIsCappedWithAMarker(t *testing.T) {
	log := &LockLog{}
	device, obs := request(t, log, "shell:runaway")
	device.Write([]byte("OKAY"))
	chunk := []byte(strings.Repeat("y", 1<<20))
	for i := 0; i < maxUnflushedOutput/len(chunk)+2; i++ {
		device.Write(chunk)
	}
	obs.finish()
	var all string
	for {
		e := log.Drain()
		if len(e) == 0 {
			break
		}
		all += outputs(e)
	}
	if len(all) > maxUnflushedOutput+100 || !strings.Contains(all, "dropped") {
		t.Errorf("output past the cap is dropped with a marker, got %d bytes, marker %v", len(all), strings.Contains(all, "dropped"))
	}
}

func TestPendingOutputIsCappedByBytes(t *testing.T) {
	log := &LockLog{}
	piece := strings.Repeat("z", maxOutputPiece)
	for i := 0; i < maxLockLogPendingBytes/maxOutputPiece+2; i++ {
		log.Record(&pb.LockLogEntry{RequestId: 1, Output: piece})
	}
	all := log.DrainAll()
	if size := len(outputs(all)); size > maxLockLogPendingBytes {
		t.Errorf("pending output must stay within the byte cap, got %d", size)
	}
	if last := all[len(all)-1]; !strings.Contains(last.Service, "truncated") {
		t.Errorf("the cap leaves one marker, got %+v", last)
	}
	if log.Drain() != nil {
		t.Error("nothing left after DrainAll")
	}
}
