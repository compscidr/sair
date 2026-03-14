package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
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
//   - ADB commands to device-sources (connections created lazily from reported source addresses)
//   - Lock management to the remote orchestrator
type CommandRouter struct {
	orchConn   *grpc.ClientConn
	orchClient pb.OrchestratorClient
	apiKey     string

	// Device-source connections, created lazily when sources register
	dsMu    sync.Mutex
	dsConns map[string]*grpc.ClientConn          // sourceAddr -> conn
	dsClients map[string]dspb.DeviceSourceClient  // sourceAddr -> client
}

func NewCommandRouter(orchestratorAddr, apiKey string, orchestratorTLS bool) (*CommandRouter, error) {
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

	return &CommandRouter{
		orchConn:   orchConn,
		orchClient: pb.NewOrchestratorClient(orchConn),
		apiKey:     apiKey,
		dsConns:    make(map[string]*grpc.ClientConn),
		dsClients:  make(map[string]dspb.DeviceSourceClient),
	}, nil
}

// getOrCreateDSClient returns a device-source gRPC client for the given address,
// creating a connection lazily if needed.
func (r *CommandRouter) getOrCreateDSClient(sourceAddr string) (dspb.DeviceSourceClient, error) {
	r.dsMu.Lock()
	defer r.dsMu.Unlock()

	if client, ok := r.dsClients[sourceAddr]; ok {
		return client, nil
	}

	slog.Info("creating gRPC connection to device-source", "addr", sourceAddr)
	conn, err := grpc.NewClient(sourceAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(64*1024*1024),
			grpc.MaxCallSendMsgSize(64*1024*1024),
		),
		grpc.WithInitialWindowSize(16*1024*1024),
		grpc.WithInitialConnWindowSize(16*1024*1024),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to device-source %s: %w", sourceAddr, err)
	}

	client := dspb.NewDeviceSourceClient(conn)
	r.dsConns[sourceAddr] = conn
	r.dsClients[sourceAddr] = client
	return client, nil
}

// RemoveDSClient closes and removes a device-source connection (called when a source goes stale).
func (r *CommandRouter) RemoveDSClient(sourceAddr string) {
	r.dsMu.Lock()
	defer r.dsMu.Unlock()

	if conn, ok := r.dsConns[sourceAddr]; ok {
		conn.Close()
		delete(r.dsConns, sourceAddr)
		delete(r.dsClients, sourceAddr)
		slog.Info("removed gRPC connection to device-source", "addr", sourceAddr)
	}
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

// ForwardToDevice relays raw bytes between a TCP connection and a device-source's ForwardToDevice gRPC stream.
// The sourceAddr is the gRPC address of the device-source that owns this serial.
func (r *CommandRouter) ForwardToDevice(sourceAddr, serial, command string, conn net.Conn) error {
	client, err := r.getOrCreateDSClient(sourceAddr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.ForwardToDevice(ctx)
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

	done := make(chan error, 1)

	// TCP → gRPC (request direction — half-close aware)
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				stream.CloseSend()
				if err != io.EOF {
					// Real error — cancel to unblock response direction
					cancel()
				}
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&dspb.ForwardData{
				Payload: &dspb.ForwardData_Data{Data: data},
			}); err != nil {
				cancel()
				return
			}
		}
	}()

	// gRPC → TCP (response direction — drives teardown)
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

	// Wait for the response direction to finish, then tear down.
	err = <-done
	cancel()
	conn.SetReadDeadline(time.Now())

	if err == io.EOF {
		return nil
	}
	return err
}

// Orchestrator operations — lock management only

func (r *CommandRouter) ListDevices() (*pb.DeviceList, error) {
	return r.orchClient.ListDevices(r.ctx(), &pb.ListDevicesRequest{})
}

func (r *CommandRouter) ReportDevices(devices []*pb.DeviceInfo) error {
	ctx, cancel := r.ctxWithTimeout(10 * time.Second)
	defer cancel()
	_, err := r.orchClient.ReportDevices(ctx, &pb.ReportDevicesRequest{Devices: devices})
	return err
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
	r.dsMu.Lock()
	for addr, conn := range r.dsConns {
		if err := conn.Close(); err != nil {
			slog.Error("failed to close device-source connection", "addr", addr, "error", err)
		}
	}
	r.dsMu.Unlock()
}
