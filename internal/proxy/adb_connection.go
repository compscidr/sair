package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

type connectionMode int

const (
	hostMode      connectionMode = iota
	transportMode
)

// AdbConnection handles a single ADB client TCP connection.
//
// Implements the state machine:
//
//	HOST_MODE → (host:transport:<serial>) → TRANSPORT_MODE
type AdbConnection struct {
	conn              net.Conn
	commandRouter     *CommandRouter
	deviceListTracker *DeviceListTracker
	allowedSerials    map[string]struct{} // nil = all, empty = none

	mode            connectionMode
	transportSerial string
	keepAlive       bool
}

func NewAdbConnection(
	conn net.Conn,
	commandRouter *CommandRouter,
	deviceListTracker *DeviceListTracker,
	allowedSerials map[string]struct{},
) *AdbConnection {
	return &AdbConnection{
		conn:              conn,
		commandRouter:     commandRouter,
		deviceListTracker: deviceListTracker,
		allowedSerials:    allowedSerials,
		mode:              hostMode,
	}
}

func (c *AdbConnection) Handle() {
	remoteAddr := c.conn.RemoteAddr()
	slog.Info("new ADB connection", "remote", remoteAddr)
	defer func() {
		c.conn.Close()
		slog.Debug("ADB connection closed", "remote", remoteAddr)
	}()

	for {
		request, err := ReadRequest(c.conn)
		if err != nil {
			if err != io.EOF {
				slog.Debug("connection error", "remote", remoteAddr, "error", err)
			}
			return
		}

		switch c.mode {
		case hostMode:
			c.handleHostCommand(request)
			if !c.keepAlive {
				return
			}
		case transportMode:
			c.handleTransportCommand(request)
			return
		}
	}
}

func (c *AdbConnection) getVisibleDevices() []*pb.DeviceInfo {
	all := c.deviceListTracker.GetDevices()
	if c.allowedSerials == nil {
		return all
	}
	if len(c.allowedSerials) == 0 {
		return nil
	}
	var filtered []*pb.DeviceInfo
	for _, d := range all {
		if _, ok := c.allowedSerials[d.Serial]; ok {
			filtered = append(filtered, d)
		}
	}
	return filtered
}

// writeOkay writes an OKAY response, logging any write errors.
func (c *AdbConnection) writeOkay() {
	if err := WriteOkay(c.conn); err != nil {
		slog.Debug("write error", "remote", c.conn.RemoteAddr(), "error", err)
	}
}

// writeOkayWithPayload writes an OKAY+payload response, logging any write errors.
func (c *AdbConnection) writeOkayWithPayload(payload string) {
	if err := WriteOkayWithPayload(c.conn, payload); err != nil {
		slog.Debug("write error", "remote", c.conn.RemoteAddr(), "error", err)
	}
}

// writeFail writes a FAIL response, logging any write errors.
func (c *AdbConnection) writeFail(message string) {
	if err := WriteFail(c.conn, message); err != nil {
		slog.Debug("write error", "remote", c.conn.RemoteAddr(), "error", err)
	}
}

func (c *AdbConnection) isSerialAllowed(serial string) bool {
	if c.allowedSerials == nil {
		return true
	}
	_, ok := c.allowedSerials[serial]
	return ok
}

func (c *AdbConnection) handleHostCommand(request string) {
	slog.Info("HOST command", "request", request)

	switch {
	case request == "host:version":
		c.writeOkayWithPayload("0029")

	case request == "host:features" || request == "host:host-features" ||
		(strings.HasPrefix(request, "host-serial:") && strings.HasSuffix(request, ":features")):
		c.writeOkayWithPayload(
			"cmd,stat_v2,ls_v2,fixed_push_mkdir,apex,abb,fixed_push_symlink_timestamp,abb_exec,remount_shell,track_app,sendrecv_v2,sendrecv_v2_brotli,sendrecv_v2_lz4,sendrecv_v2_zstd,sendrecv_v2_dry_run_send,openscreen_mdns")

	case request == "host:devices" || request == "host:devices-short":
		devices := c.getVisibleDevices()
		var sb strings.Builder
		for _, d := range devices {
			sb.WriteString(FormatDeviceLine(d.Serial))
		}
		c.writeOkayWithPayload(sb.String())

	case request == "host:devices-l":
		devices := c.getVisibleDevices()
		var sb strings.Builder
		for _, d := range devices {
			model := strings.ReplaceAll(d.Model, " ", "_")
			sb.WriteString(FormatDeviceLineLong(
				d.Serial, model, model, model,
				c.deviceListTracker.GetTransportID(d.Serial),
			))
		}
		c.writeOkayWithPayload(sb.String())

	case request == "host:track-devices" || request == "host:track-devices-l":
		c.keepAlive = true
		useLong := strings.HasSuffix(request, "-l")
		devices := c.getVisibleDevices()

		var sb strings.Builder
		for _, d := range devices {
			if useLong {
				model := strings.ReplaceAll(d.Model, " ", "_")
				sb.WriteString(FormatDeviceLineLong(
					d.Serial, model, model, model,
					c.deviceListTracker.GetTransportID(d.Serial),
				))
			} else {
				sb.WriteString(FormatDeviceLine(d.Serial))
			}
		}

		c.writeOkay()
		// TODO: send updates when devices change instead of just the initial snapshot
		// Send length-prefixed device list as a single write
		data := []byte(sb.String())
		lengthHex := fmt.Sprintf("%04X", len(data))
		msg := make([]byte, 0, len(lengthHex)+len(data))
		msg = append(msg, lengthHex...)
		msg = append(msg, data...)
		if _, err := c.conn.Write(msg); err != nil {
			slog.Debug("write error", "remote", c.conn.RemoteAddr(), "error", err)
			return
		}

		// Hold connection open until client disconnects
		buf := make([]byte, 1)
		for {
			if _, err := c.conn.Read(buf); err != nil {
				return
			}
		}

	case request == "host:kill":
		c.writeOkay()
		slog.Info("received host:kill — ignoring (proxy stays running)")

	case strings.HasPrefix(request, "host:transport:"):
		c.keepAlive = true
		serial := strings.TrimPrefix(request, "host:transport:")
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		c.handleTransport(serial)

	case request == "host:transport-any":
		c.keepAlive = true
		devices := c.getVisibleDevices()
		if len(devices) == 0 {
			c.writeFail("no devices available — use sair-acquire")
			return
		}
		c.handleTransport(devices[0].Serial)

	case strings.HasPrefix(request, "host:tport:serial:"):
		c.keepAlive = true
		serial := strings.TrimPrefix(request, "host:tport:serial:")
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		c.handleTransportWithID(serial)

	case request == "host:tport:any":
		c.keepAlive = true
		devices := c.getVisibleDevices()
		if len(devices) == 0 {
			c.writeFail("no devices available — use sair-acquire")
			return
		}
		c.handleTransportWithID(devices[0].Serial)

	case strings.HasPrefix(request, "host:transport-id:"):
		c.keepAlive = true
		idStr := strings.TrimPrefix(request, "host:transport-id:")
		transportID := 0
		fmt.Sscanf(idStr, "%d", &transportID)
		if transportID == 0 {
			c.writeFail("invalid transport id")
			return
		}
		serial := c.deviceListTracker.GetSerialByTransportID(transportID)
		if serial == "" {
			c.writeFail(fmt.Sprintf("device not found for transport id %d", transportID))
			return
		}
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		c.handleTransport(serial)

	case strings.HasPrefix(request, "host-serial:") && strings.Contains(request, ":wait-for-"):
		rest := strings.TrimPrefix(request, "host-serial:")
		waitIdx := strings.Index(rest, ":wait-for-")
		if waitIdx <= 0 {
			c.writeFail("malformed host-serial wait-for command: " + request)
			return
		}
		serial := rest[:waitIdx]
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		devices := c.getVisibleDevices()
		found := false
		for _, d := range devices {
			if d.Serial == serial {
				found = true
				break
			}
		}
		if !found {
			c.writeFail("unknown device: " + serial)
			return
		}
		c.writeOkay()
		c.writeOkay()

	case strings.HasPrefix(request, "host-serial:"):
		rest := strings.TrimPrefix(request, "host-serial:")
		colonIdx := strings.Index(rest, ":")
		if colonIdx > 0 {
			command := rest[colonIdx+1:]
			slog.Warn("unsupported host-serial command", "command", command)
			c.writeFail("unsupported command: " + request)
		} else {
			c.writeFail("malformed host-serial command: " + request)
		}

	case strings.HasPrefix(request, "host:"):
		slog.Warn("unsupported host command", "request", request)
		c.writeFail("unsupported command: " + request)

	default:
		slog.Warn("unexpected command in HOST_MODE", "request", request)
		c.writeFail("expected host: command")
	}
}

func (c *AdbConnection) handleTransportWithID(serial string) {
	c.transportSerial = serial
	c.mode = transportMode
	c.writeOkay()

	// tport protocol: send transport ID as 8-byte little-endian after OKAY
	transportID := int64(c.deviceListTracker.GetTransportID(serial))
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(transportID))
	if _, err := c.conn.Write(buf); err != nil {
		slog.Error("failed to write transport ID", "serial", serial, "error", err)
	}
	slog.Debug("transport (tport)", "serial", serial, "transportID", transportID)
}

func (c *AdbConnection) handleTransport(serial string) {
	c.transportSerial = serial
	c.mode = transportMode
	c.writeOkay()
	slog.Debug("transport", "serial", serial)
}

func (c *AdbConnection) handleTransportCommand(request string) {
	slog.Debug("TRANSPORT command", "serial", c.transportSerial, "request", request)

	switch {
	case strings.HasPrefix(request, "shell:"):
		command := strings.TrimPrefix(request, "shell:")
		c.writeOkay()

		if err := c.commandRouter.ExecuteOnDevice(c.transportSerial, command, func(data []byte) {
			WriteRaw(c.conn, data)
		}); err != nil {
			slog.Error("shell command failed", "serial", c.transportSerial, "error", err)
		}

	default:
		// Forward unsupported commands (sync:, reboot:, etc.) to the
		// real ADB server via bidirectional gRPC streaming through device-source.
		slog.Info("forwarding transport command to device-source", "serial", c.transportSerial, "request", request)
		if err := c.commandRouter.ForwardToDevice(c.transportSerial, request, c.conn, c.conn); err != nil {
			slog.Error("forward failed", "serial", c.transportSerial, "request", request, "error", err)
			c.writeFail("forward failed: " + err.Error())
		}
	}
}
