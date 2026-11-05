package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// errUnknownDevice is what a remote serial degrades to when the orchestrator
// predates the relay: the ADB client sees the same FAIL it would for a serial
// nobody has.
var errUnknownDevice = errors.New("unknown device")

// relayRefused marks a RelayToDevice error that happened before any bytes
// were relayed: the orchestrator (or dial) refused outright. Callers may
// safely write it back to the ADB client as FAIL. An error that isn't a
// relayRefused may have happened after bytes already flowed, so writing FAIL
// for it would corrupt the stream.
type relayRefused struct{ err error }

func (e relayRefused) Error() string { return e.err.Error() }

// Unwrap lets errors.Is(err, errUnknownDevice) see through the wrapper.
func (e relayRefused) Unwrap() error { return e.err }

// refused categorizes a pre-pump error (via relayErr) and wraps it as a
// relayRefused, if any.
func refused(err error) error {
	e := relayErr(err)
	if e == nil {
		return nil
	}
	return relayRefused{err: e}
}

// tcpStream adapts the ADB client's TCP connection to byteStream so the same
// pump serves the holder side. CloseSend half-closes the socket's write side.
type tcpStream struct{ conn net.Conn }

func (t tcpStream) SendBytes(p []byte) error { _, err := t.conn.Write(p); return err }
func (t tcpStream) RecvBytes() ([]byte, error) {
	buf := make([]byte, 32768)
	n, err := t.conn.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	if err == nil {
		return nil, nil
	}
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	return nil, err
}
func (t tcpStream) CloseSend() error {
	if cw, ok := t.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// closeSignalStream wraps the orchestrator Tunnel stream (both holder and
// owner use it) so a TCP-style half-close survives the orchestrator hop.
// gRPC's own CloseSend only tells the orchestrator "no more from me on this
// call"; the orchestrator, splicing two independent Tunnel calls, has no
// transport-level way to make the peer's own call observe that without
// ending it outright, which would also cut off a reply still in flight (the
// device finishing its output after stdin closes). So this CloseSend sends
// an explicit half_close message instead, which the orchestrator forwards,
// and RecvBytes turns a received one into io.EOF for pumpStreams. It is
// separate from the real gRPC CloseSend that RelayToDevice/ServeRelay make
// on the underlying stream once pumpStreams returns and both directions have
// already drained.
type closeSignalStream struct {
	tunnelStream
}

func (c closeSignalStream) RecvBytes() ([]byte, error) {
	for {
		m, err := c.Recv()
		if err != nil {
			return nil, err
		}
		if _, ok := m.Payload.(*pb.TunnelData_HalfClose); ok {
			return nil, io.EOF
		}
		if d, ok := m.Payload.(*pb.TunnelData_Data); ok {
			return d.Data, nil
		}
	}
}

func (c closeSignalStream) CloseSend() error {
	return c.Send(&pb.TunnelData{Payload: &pb.TunnelData_HalfClose{HalfClose: true}})
}

// recvAheadStream runs a tunnel's first Recv from the start, so failed can tell
// whether the tunnel has ended without taking bytes from the pump: Recv hands
// the result over first. Like any stream, one Recv at a time.
type recvAheadStream struct {
	grpc.BidiStreamingClient[pb.TunnelData, pb.TunnelData]
	done  chan struct{} // closed once the first Recv returned, with first and err set
	first *pb.TunnelData
	err   error
	taken bool
}

func newRecvAheadStream(s grpc.BidiStreamingClient[pb.TunnelData, pb.TunnelData]) *recvAheadStream {
	t := &recvAheadStream{BidiStreamingClient: s, done: make(chan struct{})}
	go func() {
		t.first, t.err = s.Recv()
		close(t.done)
	}()
	return t
}

// failed is the error that ended the tunnel if its first Recv already saw one.
func (t *recvAheadStream) failed() error {
	select {
	case <-t.done:
		return t.err
	default:
		return nil
	}
}

func (t *recvAheadStream) Recv() (*pb.TunnelData, error) {
	if t.taken {
		return t.BidiStreamingClient.Recv()
	}
	t.taken = true
	<-t.done
	return t.first, t.err
}

// RelayToDevice serves one ADB connection to a device that lives on another
// proxy: it opens the holder half of a tunnel through the orchestrator, which
// asks the owner to open the other half, and pumps until the connection ends.
// The returned error's text is meant for the ADB client (written as FAIL by
// the caller); an old orchestrator yields errUnknownDevice.
//
// The holder waits for the orchestrator's accepted message (sent once the
// owner has joined) before relaying any bytes. Every error up to and
// including that wait -- "not in lock", the owner never joining, an old
// orchestrator, or anything else -- is therefore a refusal (relayRefused):
// nothing has been relayed yet, so it is always safe to report as FAIL. Once
// accepted has been seen, pumpStreams may have relayed real bytes before
// failing, so errors from there on are plain.
func (r *CommandRouter) RelayToDevice(serial, initialCommand string, conn net.Conn) error {
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", r.apiKey)))
	defer cancel()
	stream, err := r.orchClient.Tunnel(ctx)
	if err != nil {
		return refused(err)
	}
	id := uuid.NewString()
	if err := stream.Send(&pb.TunnelData{Payload: &pb.TunnelData_Setup{Setup: &pb.TunnelSetup{
		TunnelId: id, Side: pb.TunnelSide_TUNNEL_SIDE_HOLDER, Serial: serial, InitialCommand: initialCommand,
	}}}); err != nil {
		// Send on a refused stream returns io.EOF; the status is on Recv.
		if _, rerr := stream.Recv(); rerr != nil {
			return refused(rerr)
		}
		return refused(err)
	}
	first, err := stream.Recv()
	if err != nil {
		return refused(err)
	}
	if _, ok := first.Payload.(*pb.TunnelData_Accepted); !ok {
		return refused(fmt.Errorf("orchestrator did not accept the tunnel"))
	}
	slog.Debug("relay: holder tunnel open", "tunnelId", id, "serial", serial)
	err = pumpStreams(tcpStream{conn}, closeSignalStream{tunnelStream{stream}})
	// No graceful CloseSend before cancel here, unlike ServeRelay: the
	// holder's own half_close can be dropped by this cancel when the device
	// ended first (pumpStreams returned because the owner leg reached EOF, not
	// because the holder's CloseSend was ever sent/delivered). That is safe
	// only because the orchestrator ends the peer leg (wrapped as Aborted) the
	// moment either leg's splice ends, and the owner tolerates seeing its own
	// leg end that way instead of via a half_close it was waiting on. A
	// CloseSend this close to cancel wouldn't reliably add anything anyway
	// (CloseSend only enqueues, cancel can drop the queued frame moments
	// later) and, on an error return, would risk a Send racing the
	// still-running copyDir goroutine (see the drain comment below).
	cancel()
	conn.SetReadDeadline(time.Now())
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return relayErr(err)
}

// relayErr turns a gRPC status into what the ADB client should see.
func relayErr(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.Unimplemented:
		return errUnknownDevice
	case codes.Canceled:
		return nil
	}
	return fmt.Errorf("%s", st.Message())
}

// relayRecord is one lock's relay of one phone. Its snapshot is the phone's
// third-party packages before that lock's first command, which is all the
// lock's reset may keep: an app its owner installed before then is in it.
type relayRecord struct {
	lockID   string
	once     sync.Once
	snapshot *dspb.PackageSnapshot // written inside once; nil means there is none
	prev     *relayRecord          // the record this one replaced; used inside once
}

// takeSnapshot lists the phone's packages unless this record has already had
// its one chance, and returns once the snapshot is settled either way, so a
// concurrent relay of the record waits for it. A failure leaves none: the
// reset then skips the packages, and the job proceeds. A package missing from
// the record this one replaced is named as possibly left by that lock; it is in
// this snapshot, so it stays.
func (rec *relayRecord) takeSnapshot(client dspb.DeviceSourceClient, serial string) {
	rec.once.Do(func() {
		prev := rec.prev
		rec.prev = nil
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := client.ListPackages(ctx, &dspb.ListPackagesRequest{Serial: serial})
		if err != nil {
			slog.Warn("relay: reading the phone's packages failed; this lock's reset will leave its packages alone", "serial", serial, "lockId", rec.lockID, "error", err)
			return
		}
		rec.snapshot = &dspb.PackageSnapshot{Packages: resp.Packages}
		if prev == nil {
			return
		}
		if old := prev.settledSnapshot(); old != nil {
			left := slices.DeleteFunc(slices.Clone(resp.Packages), func(pkg string) bool { return slices.Contains(old.Packages, pkg) })
			if len(left) > 0 {
				slog.Warn("relay: packages possibly left by the previous lock stay on the phone", "serial", serial, "lockId", rec.lockID, "previousLockId", prev.lockID, "packages", left)
			}
		}
	})
}

// settledSnapshot waits out a snapshot in progress. If none has started it
// settles the record as having none, so a relay still holding it can't take
// one after the reset and have it read as the phone's state before the lock.
func (rec *relayRecord) settledSnapshot() *dspb.PackageSnapshot {
	rec.once.Do(func() {})
	return rec.snapshot
}

// relayRecordFor returns serial's record for lockID, replacing one for any
// other lock. That lock's reset then won't run, so what it left stays: warned
// of here, and by takeSnapshot with names.
func (r *CommandRouter) relayRecordFor(serial, lockID string) *relayRecord {
	for {
		fresh := &relayRecord{lockID: lockID}
		v, loaded := r.relayLocks.LoadOrStore(serial, fresh)
		if !loaded {
			return fresh
		}
		rec := v.(*relayRecord)
		if rec.lockID == lockID {
			return rec
		}
		fresh.prev = rec
		if r.relayLocks.CompareAndSwap(serial, rec, fresh) {
			slog.Warn("relay: phone relayed under a new lock before the previous lock's reset, which now won't run", "serial", serial, "lockId", lockID, "previousLockId", rec.lockID)
			return fresh
		}
	}
}

// takeRelayRecord removes and returns serial's record if it names lockID.
func (r *CommandRouter) takeRelayRecord(serial, lockID string) *relayRecord {
	v, ok := r.relayLocks.Load(serial)
	if !ok {
		return nil
	}
	rec := v.(*relayRecord)
	if rec.lockID != lockID || !r.relayLocks.CompareAndDelete(serial, rec) {
		return nil
	}
	return rec
}

// ServeRelay answers the orchestrator's relay_open for one of this proxy's
// devices: joins the owner half of the tunnel, waits out any reset of the
// phone, snapshots its packages if this lock hasn't yet, opens the device
// through the local device source, and pumps until either side ends. Failures
// before joining are reported as relay_failed and a failure to open the device
// as a FAIL in the tunnel, so either way the holder's client gets a FAIL.
func (r *CommandRouter) ServeRelay(open *pb.RelayOpen) {
	fail := func(msg string) {
		slog.Warn("relay: cannot serve", "tunnelId", open.TunnelId, "serial", open.Serial, "error", msg)
		if r.sess != nil {
			r.sess.sendRelayFailed(open.TunnelId, msg)
		}
	}
	sourceAddr := ""
	if r.ResolveSource != nil {
		sourceAddr = r.ResolveSource(open.Serial)
	}
	if sourceAddr == "" {
		fail("no device " + open.Serial + " on this proxy")
		return
	}
	// Before the wait below: a reset either sees this and leaves the phone
	// alone, or already holds the serial's lock and this relay waits for it.
	rec := r.relayRecordFor(open.Serial, open.LockId)
	octx, ocancel := context.WithCancel(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", r.apiKey)))
	defer ocancel()
	stream, err := r.orchClient.Tunnel(octx)
	if err != nil {
		fail(err.Error())
		return
	}
	if err := stream.Send(&pb.TunnelData{Payload: &pb.TunnelData_Setup{Setup: &pb.TunnelSetup{TunnelId: open.TunnelId, Side: pb.TunnelSide_TUNNEL_SIDE_OWNER}}}); err != nil {
		// Send on a refused stream returns io.EOF; the status is on Recv.
		if _, rerr := stream.Recv(); rerr != nil {
			if st, ok := status.FromError(rerr); ok {
				fail(st.Message())
			} else {
				fail(rerr.Error())
			}
			return
		}
		fail(err.Error())
		return
	}
	// Joined, so the orchestrator's short join timeout no longer applies: to
	// the adb client, a reset below is just a slow service open.
	tun := newRecvAheadStream(stream)
	// A remote holder can be granted a phone whose release reset is still
	// running here (reboot, Wi-Fi reset, wake); a local acquire waits for it
	// on the same lock, so the relay does too (sair#105).
	if r.waitReset != nil {
		r.waitReset(open.Serial)
	}
	// The tunnel may have died during that wait (its lock ended, say); its
	// command must not reach the phone.
	tunnelDead := func() bool {
		err := tun.failed()
		if err != nil {
			fail(errText(err))
		}
		return err != nil
	}
	if tunnelDead() {
		return
	}
	// The holder was accepted when this side joined, so relay_failed would only
	// end its connection. It gets the FAIL through the tunnel instead.
	failInBand := func(msg string) {
		slog.Warn("relay: cannot open the device", "tunnelId", open.TunnelId, "serial", open.Serial, "error", msg)
		var reply bytes.Buffer
		WriteFail(&reply, msg)
		s := closeSignalStream{tunnelStream{tun}}
		if err := s.SendBytes(reply.Bytes()); err != nil {
			return
		}
		s.CloseSend()
		tun.CloseSend()
		// As after a clean pump below: wait for the holder, or the FAIL may be lost.
		for {
			if _, err := tun.Recv(); err != nil {
				return
			}
		}
	}
	client, err := r.getOrCreateDSClient(sourceAddr)
	if err != nil {
		failInBand(err.Error())
		return
	}
	// After the wait, so what a reset just removed isn't in it; before the device
	// opens, so the first command can't install anything first.
	rec.takeSnapshot(client, open.Serial)
	if tunnelDead() { // a reset may be waiting on the snapshot: same reasoning
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fwd, err := client.ForwardToDevice(ctx)
	if err != nil {
		failInBand(errText(err))
		return
	}
	if err := fwd.Send(&dspb.ForwardData{Payload: &dspb.ForwardData_Setup{Setup: &dspb.ForwardSetup{Serial: open.Serial, InitialCommand: open.InitialCommand}}}); err != nil {
		failInBand(err.Error())
		return
	}
	slog.Info("relay: serving", "tunnelId", open.TunnelId, "serial", open.Serial, "lockId", open.LockId)
	pumpErr := pumpStreams(closeSignalStream{tunnelStream{tun}}, forwardStream{fwd})
	if pumpErr != nil && !errors.Is(pumpErr, io.EOF) {
		slog.Debug("relay: ended with error", "tunnelId", open.TunnelId, "error", pumpErr)
	}
	// Only drain when pumpStreams returned nil: that happens exclusively once
	// BOTH copyDir goroutines have already finished on their own (pumpStreams
	// returns the first REAL error immediately, without waiting for the
	// other direction -- see its comment). On a real-error return the other
	// goroutine can still be live, e.g. blocked in tun.Recv() -- calling
	// Recv again here would race it (grpc-go: "it is not safe to call
	// RecvMsg on the same stream in different goroutines"), and CloseSend
	// below is a Send that could just as easily race the other goroutine's
	// own SendBytes on tun. So on error we skip straight to cancelling.
	//
	// When it is nil, SendMsg only guarantees our own half_close (sent from
	// within pumpStreams, above) was handed to the transport, not that it
	// reached the orchestrator (grpc-go: "an untimely stream closure may
	// result in lost messages") -- an immediate cancel could still drop it
	// before the holder ever sees it. CloseSend, then draining Recv to the
	// orchestrator's own EOF, forces a round trip: the orchestrator only
	// closes its side of Tunnel once it has spliced everything we sent
	// through to the holder AND the holder has gracefully closed its own
	// call (which the holder does once it sees this leg's half_close), so by
	// the time Recv returns here it is safe to cancel -- there is nothing
	// left in flight to lose, and no other goroutine left to race.
	if pumpErr == nil {
		tun.CloseSend()
		for {
			if _, err := tun.Recv(); err != nil {
				break
			}
		}
	}
	ocancel()
	cancel()
}
