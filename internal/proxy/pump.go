package proxy

import (
	"io"
	"sync"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
)

// byteStream is the slice of a gRPC bidi stream the relay pump needs. Both the
// orchestrator's TunnelData stream and the device source's ForwardData stream
// carry a bytes payload, so one pump serves both hops.
type byteStream interface {
	SendBytes(p []byte) error
	// RecvBytes returns io.EOF when the peer half-closed its send side.
	RecvBytes() ([]byte, error)
	CloseSend() error
}

// pumpStreams copies a→b and b→a until both directions have ended. A
// half-close on one side becomes CloseSend on the other while the reverse
// direction keeps flowing: ADB relies on this for `shell` with piped stdin,
// `install` and `sync`. Returns the first error that was not a half-close.
//
// When one direction fails with a non-EOF error, the other direction may
// still be blocked in RecvBytes; the caller cancels both streams' contexts
// after this returns, which unblocks it.
func pumpStreams(a, b byteStream) error {
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	copyDir := func(from, to byteStream) {
		defer wg.Done()
		for {
			p, err := from.RecvBytes()
			if err != nil {
				to.CloseSend()
				if err != io.EOF {
					errs <- err
				}
				return
			}
			if len(p) == 0 {
				// A TCP adapter can surface a zero-length read; nothing to relay.
				continue
			}
			if err := to.SendBytes(p); err != nil {
				errs <- err
				return
			}
		}
	}
	wg.Add(2)
	go copyDir(a, b)
	go copyDir(b, a)
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

// tunnelStream adapts an orchestrator Tunnel stream to byteStream.
type tunnelStream struct {
	grpc.BidiStreamingClient[pb.TunnelData, pb.TunnelData]
}

func (t tunnelStream) SendBytes(p []byte) error {
	return t.Send(&pb.TunnelData{Payload: &pb.TunnelData_Data{Data: p}})
}

func (t tunnelStream) RecvBytes() ([]byte, error) {
	for {
		m, err := t.Recv()
		if err != nil {
			return nil, err
		}
		if d, ok := m.Payload.(*pb.TunnelData_Data); ok {
			return d.Data, nil
		}
		// A stray setup after the first is ignored.
	}
}

// forwardStream adapts a device-source ForwardToDevice stream to byteStream.
type forwardStream struct {
	grpc.BidiStreamingClient[dspb.ForwardData, dspb.ForwardData]
}

func (f forwardStream) SendBytes(p []byte) error {
	return f.Send(&dspb.ForwardData{Payload: &dspb.ForwardData_Data{Data: p}})
}

func (f forwardStream) RecvBytes() ([]byte, error) {
	for {
		m, err := f.Recv()
		if err != nil {
			return nil, err
		}
		if d := m.GetData(); d != nil {
			return d, nil
		}
	}
}
