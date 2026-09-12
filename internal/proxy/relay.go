package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// errUnknownDevice is what a remote serial degrades to when the orchestrator
// predates the relay: the ADB client sees the same FAIL it would for a serial
// nobody has.
var errUnknownDevice = errors.New("unknown device")

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

// RelayToDevice serves one ADB connection to a device that lives on another
// proxy: it opens the holder half of a tunnel through the orchestrator, which
// asks the owner to open the other half, and pumps until the connection ends.
// The returned error's text is meant for the ADB client (written as FAIL by
// the caller); an old orchestrator yields errUnknownDevice.
func (r *CommandRouter) RelayToDevice(serial, initialCommand string, conn net.Conn) error {
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", r.apiKey)))
	defer cancel()
	stream, err := r.orchClient.Tunnel(ctx)
	if err != nil {
		return relayErr(err)
	}
	id := uuid.NewString()
	if err := stream.Send(&pb.TunnelData{Payload: &pb.TunnelData_Setup{Setup: &pb.TunnelSetup{
		TunnelId: id, Side: pb.TunnelSide_TUNNEL_SIDE_HOLDER, Serial: serial, InitialCommand: initialCommand,
	}}}); err != nil {
		// Send on a refused stream returns io.EOF; the status is on Recv.
		if _, rerr := stream.Recv(); rerr != nil {
			return relayErr(rerr)
		}
		return relayErr(err)
	}
	slog.Debug("relay: holder tunnel open", "tunnelId", id, "serial", serial)
	err = pumpStreams(tcpStream{conn}, closeSignalStream{tunnelStream{stream}})
	// A graceful CloseSend (as opposed to the cancel below, which aborts the
	// RPC outright) lets the orchestrator finish delivering anything already
	// queued -- including our own half_close message above -- before the
	// stream ends. Cancel then reclaims the call; by this point pumpStreams
	// has already drained both directions, so there is nothing left to lose.
	stream.CloseSend()
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

// ServeRelay answers the orchestrator's relay_open for one of this proxy's
// devices: opens the device through the local device source and the owner
// half of the tunnel, and pumps until either side ends. Failures before the
// tunnel is up are reported as relay_failed so the holder's client gets a FAIL.
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
	client, err := r.getOrCreateDSClient(sourceAddr)
	if err != nil {
		fail(err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fwd, err := client.ForwardToDevice(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	if err := fwd.Send(&dspb.ForwardData{Payload: &dspb.ForwardData_Setup{Setup: &dspb.ForwardSetup{Serial: open.Serial, InitialCommand: open.InitialCommand}}}); err != nil {
		fail(err.Error())
		return
	}
	octx, ocancel := context.WithCancel(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", r.apiKey)))
	defer ocancel()
	tun, err := r.orchClient.Tunnel(octx)
	if err != nil {
		fail(err.Error())
		return
	}
	if err := tun.Send(&pb.TunnelData{Payload: &pb.TunnelData_Setup{Setup: &pb.TunnelSetup{TunnelId: open.TunnelId, Side: pb.TunnelSide_TUNNEL_SIDE_OWNER}}}); err != nil {
		fail(err.Error())
		return
	}
	slog.Info("relay: serving", "tunnelId", open.TunnelId, "serial", open.Serial, "lockId", open.LockId)
	if err := pumpStreams(closeSignalStream{tunnelStream{tun}}, forwardStream{fwd}); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("relay: ended with error", "tunnelId", open.TunnelId, "error", err)
	}
	// SendMsg only guarantees the message is handed to the transport, not that
	// it reached the orchestrator (grpc-go: "an untimely stream closure may
	// result in lost messages") -- an immediate cancel could drop our own
	// half_close before the holder ever sees it. CloseSend, then draining
	// Recv to the orchestrator's own EOF, forces a round trip: the
	// orchestrator only closes its side of Tunnel once it has spliced
	// everything we sent through to the holder AND the holder has gracefully
	// closed its own call (the holder does that once it sees this owner
	// leg's half_close), so by the time Recv returns here it is safe to
	// cancel -- there is nothing left in flight to lose.
	tun.CloseSend()
	for {
		if _, err := tun.Recv(); err != nil {
			break
		}
	}
	ocancel()
	cancel()
}
