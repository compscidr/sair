package devicesource

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

var serialPattern = regexp.MustCompile(`^[a-zA-Z0-9._:()\-]+$`)

// Server implements the DeviceSource gRPC service.
type Server struct {
	pb.UnimplementedDeviceSourceServer
	adbPort int
}

func NewServer() *Server {
	port := 5038
	if v := os.Getenv("ADB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	return &Server{adbPort: port}
}

func (s *Server) adbCmd(args ...string) *exec.Cmd {
	fullArgs := append([]string{"-P", strconv.Itoa(s.adbPort)}, args...)
	return exec.Command("adb", fullArgs...)
}

func (s *Server) getSerialNumbers() ([]string, error) {
	cmd := s.adbCmd("devices")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}

	var serials []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == "device" {
			serials = append(serials, parts[0])
		}
	}
	return serials, nil
}

func validateSerial(serial string) error {
	if serial == "" || !serialPattern.MatchString(serial) {
		return status.Errorf(codes.InvalidArgument, "invalid device serial: %s", serial)
	}
	return nil
}

func (s *Server) getDeviceInfo(serial string) (*pb.Device, error) {
	if err := validateSerial(serial); err != nil {
		return nil, err
	}

	getProp := func(prop string) (string, error) {
		cmd := s.adbCmd("-s", serial, "shell", "getprop", prop)
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}

	model, _ := getProp("ro.product.model")
	manufacturer, _ := getProp("ro.product.manufacturer")
	releaseStr, _ := getProp("ro.build.version.release")
	sdkStr, _ := getProp("ro.build.version.sdk")

	release, _ := strconv.Atoi(releaseStr)
	sdk, _ := strconv.Atoi(sdkStr)

	return &pb.Device{
		Serialno:     serial,
		Manufacturer: manufacturer,
		Model:        model,
		Release:      int32(release),
		Sdk:          int32(sdk),
	}, nil
}

func (s *Server) GetDevices(_ context.Context, _ *emptypb.Empty) (*pb.Devices, error) {
	serials, err := s.getSerialNumbers()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list devices: %v", err)
	}

	devices := &pb.Devices{}
	for _, serial := range serials {
		device, err := s.getDeviceInfo(serial)
		if err != nil {
			slog.Warn("failed to get device info", "serial", serial, "error", err)
			continue
		}
		devices.Devices = append(devices.Devices, device)
	}
	return devices, nil
}

func (s *Server) EnqueueCommand(req *pb.Command, stream pb.DeviceSource_EnqueueCommandServer) error {
	return s.streamProcess(stream.Context(), exec.Command("bash", "-c", req.Cmd), 1800, stream)
}

func (s *Server) ExecOnDevice(req *pb.DeviceCommand, stream pb.DeviceSource_ExecOnDeviceServer) error {
	if err := validateSerial(req.Serial); err != nil {
		return err
	}

	args := []string{"-P", strconv.Itoa(s.adbPort), "-s", req.Serial, "shell"}
	args = append(args, strings.Fields(req.Command)...)
	slog.Info("ExecOnDevice", "args", strings.Join(args, " "))

	timeout := int(req.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 1800
	}

	cmd := exec.Command("adb", args...)
	return s.streamProcess(stream.Context(), cmd, timeout, stream)
}

type commandResultStream interface {
	Send(*pb.CommandResult) error
}

func (s *Server) streamProcess(ctx context.Context, cmd *exec.Cmd, timeoutSeconds int, stream commandResultStream) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start: %v", err)
	}

	// Stream stdout and stderr concurrently
	done := make(chan error, 2)

	streamReader := func(reader io.Reader, isStderr bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text() + "\n"
			result := &pb.CommandResult{}
			if isStderr {
				result.Stderr = line
			} else {
				result.Stdout = line
			}
			if err := stream.Send(result); err != nil {
				done <- err
				return
			}
		}
		done <- scanner.Err()
	}

	go streamReader(stdout, false)
	go streamReader(stderr, true)

	// Wait for both readers
	for i := 0; i < 2; i++ {
		<-done
	}

	// Wait for process with timeout
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case <-time.After(time.Duration(timeoutSeconds) * time.Second):
		cmd.Process.Kill()
		slog.Warn("process timed out", "timeout", timeoutSeconds)
		stream.Send(&pb.CommandResult{
			Stderr:   fmt.Sprintf("Command timed out after %ds", timeoutSeconds),
			ExitCode: -1,
		})
	case err := <-waitDone:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		stream.Send(&pb.CommandResult{ExitCode: int32(exitCode)})
	case <-ctx.Done():
		cmd.Process.Kill()
	}

	return nil
}

func (s *Server) ForwardToDevice(stream pb.DeviceSource_ForwardToDeviceServer) error {
	// First message must be setup
	firstMsg, err := stream.Recv()
	if err != nil {
		return err
	}
	setup := firstMsg.GetSetup()
	if setup == nil {
		return status.Error(codes.InvalidArgument, "first message must be setup")
	}
	if err := validateSerial(setup.Serial); err != nil {
		return err
	}

	slog.Info("ForwardToDevice: connecting to ADB server", "serial", setup.Serial, "command", setup.InitialCommand)

	// Connect to real ADB server
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", s.adbPort))
	if err != nil {
		return status.Errorf(codes.Internal, "failed to connect to ADB server: %v", err)
	}
	defer conn.Close()

	// Establish transport to the device
	if err := sendAdbLtv(conn, "host:transport:"+setup.Serial); err != nil {
		return status.Errorf(codes.Internal, "failed to send transport: %v", err)
	}
	if err := readAdbOkay(conn); err != nil {
		return err
	}

	// Send the initial command (e.g., "sync:")
	if err := sendAdbLtv(conn, setup.InitialCommand); err != nil {
		return status.Errorf(codes.Internal, "failed to send initial command: %v", err)
	}

	// Bidirectional forwarding
	done := make(chan error, 2)

	// ADB → gRPC
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				done <- err
				return
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&pb.ForwardData{
				Payload: &pb.ForwardData_Data{Data: data},
			}); err != nil {
				done <- err
				return
			}
		}
	}()

	// gRPC → ADB
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if data := msg.GetData(); data != nil {
				if _, err := conn.Write(data); err != nil {
					done <- err
					return
				}
			}
		}
	}()

	// Wait for either direction to finish
	<-done
	return nil
}

func sendAdbLtv(conn net.Conn, command string) error {
	lengthHex := fmt.Sprintf("%04X", len(command))
	if _, err := conn.Write([]byte(lengthHex)); err != nil {
		return err
	}
	_, err := conn.Write([]byte(command))
	return err
}

func readAdbOkay(conn net.Conn) error {
	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return status.Errorf(codes.Internal, "ADB server closed connection")
	}
	if string(resp) != "OKAY" {
		// Read the error message
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, lenBuf); err == nil {
			length := binary.BigEndian.Uint16([]byte{0, 0}) // parse hex
			if l, err := strconv.ParseInt(string(lenBuf), 16, 32); err == nil {
				length = uint16(l)
				msg := make([]byte, length)
				io.ReadFull(conn, msg)
				return status.Errorf(codes.Internal, "ADB transport failed: %s: %s", string(resp), string(msg))
			}
		}
		return status.Errorf(codes.Internal, "ADB transport failed: %s", string(resp))
	}
	return nil
}
