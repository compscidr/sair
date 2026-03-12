package proxy

import (
	"context"
	"io"
	"log/slog"
	"time"

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

// CommandRouter is a gRPC client that routes ADB commands to the orchestrator.
type CommandRouter struct {
	conn   *grpc.ClientConn
	client pb.OrchestratorClient
	apiKey string
}

func NewCommandRouter(addr, apiKey string, useTLS bool) (*CommandRouter, error) {
	var opts []grpc.DialOption
	if useTLS {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}

	return &CommandRouter{
		conn:   conn,
		client: pb.NewOrchestratorClient(conn),
		apiKey: apiKey,
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

func (r *CommandRouter) AcquireDevice(serial string, timeoutSeconds, idleTimeoutSeconds int32) (*pb.AcquireDeviceResponse, error) {
	req := &pb.AcquireDeviceRequest{
		TimeoutSeconds:     timeoutSeconds,
		IdleTimeoutSeconds: idleTimeoutSeconds,
	}
	if serial != "" {
		req.Requirements = &pb.DeviceRequirements{Serial: serial}
	}
	return r.client.AcquireDevice(r.ctx(), req)
}

func (r *CommandRouter) ExecuteOnSession(sessionID, command string, onOutput func([]byte)) error {
	req := &pb.SessionCommand{
		SessionId: sessionID,
		Command:   command,
	}
	stream, err := r.client.ExecuteOnSession(r.ctx(), req)
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
			onOutput([]byte(result.Stdout))
		}
		if result.Stderr != "" {
			onOutput([]byte(result.Stderr))
		}
	}
}

func (r *CommandRouter) ReleaseDevice(sessionID string) (bool, error) {
	resp, err := r.client.ReleaseDevice(r.ctx(), &pb.ReleaseDeviceRequest{SessionId: sessionID})
	if err != nil {
		return false, err
	}
	return resp.Released, nil
}

func (r *CommandRouter) ListDevices() (*pb.DeviceList, error) {
	return r.client.ListDevices(r.ctx(), &pb.ListDevicesRequest{})
}

// ForwardToSession relays raw bytes between TCP streams and gRPC bidirectional stream.
func (r *CommandRouter) ForwardToSession(sessionID, command string, tcpIn io.Reader, tcpOut io.Writer) error {
	stream, err := r.client.ForwardToSession(r.ctx())
	if err != nil {
		return err
	}

	// Send setup message
	err = stream.Send(&pb.SessionForwardData{
		Payload: &pb.SessionForwardData_Setup{
			Setup: &pb.SessionForwardSetup{
				SessionId:      sessionID,
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
			n, err := tcpIn.Read(buf)
			if err != nil {
				stream.CloseSend()
				done <- err
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&pb.SessionForwardData{
				Payload: &pb.SessionForwardData_Data{Data: data},
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
				if _, err := tcpOut.Write(data); err != nil {
					done <- err
					return
				}
			}
		}
	}()

	// Wait for either direction
	<-done
	return nil
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
	resp, err := r.client.AcquireLock(ctx, req)
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
	resp, err := r.client.ReleaseLock(ctx, &pb.ReleaseLockRequest{LockId: lockID})
	if err != nil {
		return false, err
	}
	return resp.Released, nil
}

func (r *CommandRouter) LockHeartbeat(lockID string) (bool, error) {
	ctx, cancel := r.ctxWithTimeout(30 * time.Second)
	defer cancel()
	resp, err := r.client.LockHeartbeat(ctx, &pb.LockHeartbeatRequest{LockId: lockID})
	if err != nil {
		return false, err
	}
	return resp.Alive, nil
}

func (r *CommandRouter) Shutdown() {
	if err := r.conn.Close(); err != nil {
		slog.Error("failed to close gRPC connection", "error", err)
	}
}
