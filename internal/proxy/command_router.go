package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// LockResult holds the result of a lock acquisition.
type LockResult struct {
	LockID  string
	Serials map[string]struct{}
}

// CommandRouter is a gRPC client that routes:
//   - ADB commands directly to the local device-source
//   - Lock management to the remote orchestrator
type CommandRouter struct {
	orchConn   *grpc.ClientConn
	orchClient pb.OrchestratorClient
	dsConn     *grpc.ClientConn
	dsClient   dspb.DeviceSourceClient
	apiKey     string
}

func NewCommandRouter(orchestratorAddr, deviceSourceAddr, apiKey string, orchestratorTLS bool) (*CommandRouter, error) {
	// Orchestrator connection (remote, may use TLS)
	var orchOpts []grpc.DialOption
	if orchestratorTLS {
		orchOpts = append(orchOpts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	} else {
		orchOpts = append(orchOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	orchConn, err := grpc.NewClient(orchestratorAddr, orchOpts...)
	if err != nil {
		return nil, err
	}

	// Device-source connection (local, plaintext)
	dsConn, err := grpc.NewClient(deviceSourceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		orchConn.Close()
		return nil, err
	}

	return &CommandRouter{
		orchConn:   orchConn,
		orchClient: pb.NewOrchestratorClient(orchConn),
		dsConn:     dsConn,
		dsClient:   dspb.NewDeviceSourceClient(dsConn),
		apiKey:     apiKey,
	}, nil
}

func (r *CommandRouter) ctx() context.Context {
	md := metadata.Pairs("x-api-key", r.apiKey)
	return metadata.NewOutgoingContext(context.Background(), md)
}

func (r *CommandRouter) ctxWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	md := metadata.Pairs("x-api-key", r.apiKey)
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	return context.WithTimeout(ctx, d)
}

// ADB operations — go directly to the device-source

func (r *CommandRouter) ExecuteOnDevice(ctx context.Context, serial, command string, onOutput func([]byte) error) error {
	req := &dspb.DeviceCommand{
		Serial:  serial,
		Command: command,
	}
	stream, err := r.dsClient.ExecOnDevice(ctx, req)
	if err != nil {
		return err
	}
	for {
		result, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if result.Stdout != "" {
			if err := onOutput([]byte(result.Stdout)); err != nil {
				return err
			}
		}
		if result.Stderr != "" {
			if err := onOutput([]byte(result.Stderr)); err != nil {
				return err
			}
		}
	}
}

// ExecuteOnDeviceShellV2 runs a command via ExecOnDevice and writes the output
// using shell v2 binary framing (stdout/stderr/exit packets). This lets ddmlib
// clients that send shell,v2 requests get properly framed responses without
// going through the fragile ForwardToDevice bidirectional tunnel.
func (r *CommandRouter) ExecuteOnDeviceShellV2(ctx context.Context, serial, command string, conn net.Conn) error {
	req := &dspb.DeviceCommand{
		Serial:  serial,
		Command: command,
	}
	stream, err := r.dsClient.ExecOnDevice(ctx, req)
	if err != nil {
		return err
	}
	var exitCode int32
	for {
		result, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if result.Stdout != "" {
			if err := WriteShellV2Packet(conn, shellV2Stdout, []byte(result.Stdout)); err != nil {
				return err
			}
		}
		if result.Stderr != "" {
			if err := WriteShellV2Packet(conn, shellV2Stderr, []byte(result.Stderr)); err != nil {
				return err
			}
		}
		exitCode = result.ExitCode
	}
	return WriteShellV2Packet(conn, shellV2Exit, []byte{byte(exitCode)})
}

// ForwardToDevice relays raw bytes between a TCP connection and the device-source's ForwardToDevice gRPC stream.
func (r *CommandRouter) ForwardToDevice(serial, command string, conn net.Conn) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := r.dsClient.ForwardToDevice(ctx)
	if err != nil {
		return err
	}

	// Send setup message
	err = stream.Send(&dspb.ForwardData{
		Payload: &dspb.ForwardData_Setup{
			Setup: &dspb.ForwardSetup{
				Serial:         serial,
				InitialCommand: command,
			},
		},
	})
	if err != nil {
		return err
	}

	done := make(chan error, 2)

	// TCP → gRPC
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				stream.CloseSend()
				done <- err
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&dspb.ForwardData{
				Payload: &dspb.ForwardData_Data{Data: data},
			}); err != nil {
				done <- err
				return
			}
		}
	}()

	// gRPC → TCP
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if data := resp.GetData(); data != nil {
				if _, err := conn.Write(data); err != nil {
					done <- err
					return
				}
			}
		}
	}()

	// Wait for first direction to finish, then tear down the other.
	// cancel() unblocks gRPC Recv/Send; SetReadDeadline unblocks TCP Read.
	err = <-done
	cancel()
	conn.SetReadDeadline(time.Now())
	<-done

	if err == io.EOF {
		return nil
	}
	return err
}

// Orchestrator operations — lock management only

func (r *CommandRouter) ListDevices() (*pb.DeviceList, error) {
	return r.orchClient.ListDevices(r.ctx(), &pb.ListDevicesRequest{})
}

func (r *CommandRouter) AcquireLock(serials map[string]struct{}, deadlineMinutes int64) (*LockResult, error) {
	if deadlineMinutes <= 0 {
		deadlineMinutes = 30
	}
	ctx, cancel := r.ctxWithTimeout(time.Duration(deadlineMinutes) * time.Minute)
	defer cancel()

	req := &pb.AcquireLockRequest{}
	for s := range serials {
		req.Serials = append(req.Serials, s)
	}
	resp, err := r.orchClient.AcquireLock(ctx, req)
	if err != nil {
		return nil, err
	}

	resultSerials := make(map[string]struct{})
	for _, s := range resp.Serials {
		resultSerials[s] = struct{}{}
	}
	return &LockResult{LockID: resp.LockId, Serials: resultSerials}, nil
}

func (r *CommandRouter) ReleaseLock(lockID string) (bool, error) {
	ctx, cancel := r.ctxWithTimeout(30 * time.Second)
	defer cancel()
	resp, err := r.orchClient.ReleaseLock(ctx, &pb.ReleaseLockRequest{LockId: lockID})
	if err != nil {
		return false, err
	}
	return resp.Released, nil
}

func (r *CommandRouter) LockHeartbeat(lockID string) (bool, error) {
	ctx, cancel := r.ctxWithTimeout(30 * time.Second)
	defer cancel()
	resp, err := r.orchClient.LockHeartbeat(ctx, &pb.LockHeartbeatRequest{LockId: lockID})
	if err != nil {
		return false, err
	}
	return resp.Alive, nil
}

func (r *CommandRouter) Shutdown() {
	if err := r.orchConn.Close(); err != nil {
		slog.Error("failed to close orchestrator connection", "error", err)
	}
	if err := r.dsConn.Close(); err != nil {
		slog.Error("failed to close device-source connection", "error", err)
	}
}
