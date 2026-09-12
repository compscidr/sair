package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// AdbConnection handles a single ADB client TCP connection.
//
// Host-mode commands (version, devices, transport selection) are handled
// locally. Once the client selects a transport, all subsequent traffic is
// tunneled transparently to the real ADB server through the device-source.
type AdbConnection struct {
	conn              net.Conn
	commandRouter     *CommandRouter
	deviceListTracker *DeviceListTracker
	allowedSerials    map[string]struct{}       // nil = all, empty = none
	lockLog           *LockLog                  // nil on the bare port: nothing to attribute requests to
	remoteDevices     map[string]*pb.DeviceInfo // granted devices reached through the relay; nil for none

	// remoteTransportID returns a stable transport id for a remote serial,
	// and remoteSerialByTransportID is its reverse (for host:transport-id:).
	// Both set by the scoped port manager right after construction; nil
	// elsewhere (no remote devices possible without them).
	remoteTransportID         func(string) int
	remoteSerialByTransportID func(int) string

	keepAlive bool
}

func NewAdbConnection(
	conn net.Conn,
	commandRouter *CommandRouter,
	deviceListTracker *DeviceListTracker,
	allowedSerials map[string]struct{},
	lockLog *LockLog,
	remoteDevices map[string]*pb.DeviceInfo,
) *AdbConnection {
	return &AdbConnection{
		conn:              conn,
		commandRouter:     commandRouter,
		deviceListTracker: deviceListTracker,
		allowedSerials:    allowedSerials,
		lockLog:           lockLog,
		remoteDevices:     remoteDevices,
	}
}

// tunnel relays the rest of the connection to the device, recording the
// request into the lock's log when this is a scoped-port connection.
//
// A remote device (sourceAddr == "", present in remoteDevices) is reached
// through the relay instead of the local device-source. RelayToDevice waits
// for the orchestrator's accepted message before relaying any bytes, so
// every error it returns before that point -- including errUnknownDevice --
// comes back as a relayRefused, safe to report to the client as FAIL. An
// error after accepted is plain: pumpStreams may already have relayed real
// bytes, and writing FAIL into that stream would corrupt it.
func (c *AdbConnection) tunnel(sourceAddr, serial string) error {
	conn, obs := newTunnelObserver(c.lockLog, serial, c.conn)
	if obs != nil {
		defer obs.finish()
	}
	if sourceAddr == "" {
		if _, ok := c.remoteDevices[serial]; ok {
			err := c.commandRouter.RelayToDevice(serial, "", conn)
			if err != nil {
				var refused relayRefused
				if errors.As(err, &refused) {
					if werr := WriteFail(conn, err.Error()); werr != nil {
						slog.Debug("write error", "remote", c.conn.RemoteAddr(), "error", werr)
					}
				} else {
					slog.Debug("relay tunnel failed", "serial", serial, "error", err)
				}
			}
			return err
		}
	}
	return c.commandRouter.ForwardToDevice(sourceAddr, serial, "", conn)
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

		c.handleHostCommand(request)
		if !c.keepAlive {
			return
		}
	}
}

// sdkOf returns the API level the device-source reported for serial, or
// modernSdk when the device is not visible on this connection or its level
// is unknown, so such a device gets the full feature set.
func (c *AdbConnection) sdkOf(serial string) int32 {
	for _, d := range c.getVisibleDevices() {
		if d.Serial == serial {
			if d.Sdk <= 0 {
				return modernSdk
			}
			return d.Sdk
		}
	}
	return modernSdk
}

// minVisibleSdk is the lowest API level among the devices this connection can
// see: host:features has no serial, so the answer must hold for all of them.
func (c *AdbConnection) minVisibleSdk() int32 {
	min := int32(modernSdk)
	for _, d := range c.getVisibleDevices() {
		if d.Sdk > 0 && d.Sdk < min {
			min = d.Sdk
		}
	}
	return min
}

// getVisibleDevices returns every device this connection may see: the
// tracker's local devices plus any remote (lock-granted, relayed) devices,
// both filtered by allowedSerials. This is the single point that feeds
// host:devices(-l), host:track-devices(-l), sdkOf/minVisibleSdk, and the
// transport handlers' "found" checks, so merging remote devices here is what
// makes all of them see them.
func (c *AdbConnection) getVisibleDevices() []*pb.DeviceInfo {
	all := c.deviceListTracker.GetDevices()
	var devices []*pb.DeviceInfo
	if c.allowedSerials == nil {
		devices = append(devices, all...)
		for _, d := range c.remoteDevices {
			devices = append(devices, d)
		}
	} else if len(c.allowedSerials) > 0 {
		for _, d := range all {
			if _, ok := c.allowedSerials[d.Serial]; ok {
				devices = append(devices, d)
			}
		}
		for serial, d := range c.remoteDevices {
			if _, ok := c.allowedSerials[serial]; ok {
				devices = append(devices, d)
			}
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Serial < devices[j].Serial })
	return devices
}

// transportIDOf returns the transport id for a serial, whichever source it
// comes from: the tracker for a local device, this port's remote assignment
// for a relayed one.
func (c *AdbConnection) transportIDOf(serial string) int {
	if id := c.deviceListTracker.GetTransportID(serial); id != 0 {
		return id
	}
	if c.remoteTransportID != nil {
		return c.remoteTransportID(serial)
	}
	return 0
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

	case request == "host:features" || request == "host:host-features":
		c.writeOkayWithPayload(FeaturesForSdk(c.minVisibleSdk()))

	case strings.HasPrefix(request, "host-serial:") && strings.HasSuffix(request, ":features"):
		serial := strings.TrimSuffix(strings.TrimPrefix(request, "host-serial:"), ":features")
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		c.writeOkayWithPayload(FeaturesForSdk(c.sdkOf(serial)))

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
				c.transportIDOf(d.Serial),
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
					c.transportIDOf(d.Serial),
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
		serial := strings.TrimPrefix(request, "host:transport:")
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		c.handleTransport(serial)

	case request == "host:transport-any":
		devices := c.getVisibleDevices()
		if len(devices) == 0 {
			c.writeFail("no devices available — use sair-acquire")
			return
		}
		c.handleTransport(devices[0].Serial)

	case strings.HasPrefix(request, "host:tport:serial:"):
		serial := strings.TrimPrefix(request, "host:tport:serial:")
		if !c.isSerialAllowed(serial) {
			c.writeFail("device " + serial + " not available — use sair-acquire")
			return
		}
		c.handleTransportWithID(serial)

	case request == "host:tport:any":
		devices := c.getVisibleDevices()
		if len(devices) == 0 {
			c.writeFail("no devices available — use sair-acquire")
			return
		}
		c.handleTransportWithID(devices[0].Serial)

	case strings.HasPrefix(request, "host:transport-id:"):
		idStr := strings.TrimPrefix(request, "host:transport-id:")
		transportID := 0
		fmt.Sscanf(idStr, "%d", &transportID)
		if transportID == 0 {
			c.writeFail("invalid transport id")
			return
		}
		serial := c.deviceListTracker.GetSerialByTransportID(transportID)
		if serial == "" && c.remoteSerialByTransportID != nil {
			serial = c.remoteSerialByTransportID(transportID)
		}
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
	sourceAddr := c.deviceListTracker.GetSourceAddr(serial)
	if sourceAddr == "" {
		if _, ok := c.remoteDevices[serial]; !ok {
			c.writeFail("no device-source registered for " + serial)
			return
		}
	}

	c.writeOkay()

	// tport protocol: send transport ID as 8-byte little-endian after OKAY
	transportID := int64(c.transportIDOf(serial))
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(transportID))
	if _, err := c.conn.Write(buf); err != nil {
		slog.Error("failed to write transport ID", "serial", serial, "error", err)
		return
	}
	slog.Debug("transport (tport) — starting tunnel", "serial", serial, "transportID", transportID)

	if err := c.tunnel(sourceAddr, serial); err != nil {
		slog.Error("tunnel failed", "serial", serial, "error", err)
	}
}

func (c *AdbConnection) handleTransport(serial string) {
	sourceAddr := c.deviceListTracker.GetSourceAddr(serial)
	if sourceAddr == "" {
		if _, ok := c.remoteDevices[serial]; !ok {
			c.writeFail("no device-source registered for " + serial)
			return
		}
	}

	c.writeOkay()
	slog.Debug("transport — starting tunnel", "serial", serial)

	if err := c.tunnel(sourceAddr, serial); err != nil {
		slog.Error("tunnel failed", "serial", serial, "error", err)
	}
}
