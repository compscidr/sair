package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
)

// ADB smart socket protocol codec.
//
// The ADB client-server protocol uses a simple Length-Type-Value (LTV) text format:
//   - Client sends: 4-char hex length + payload  (e.g. "000Chost:version")
//   - Server replies: "OKAY" or "FAIL" + 4-char hex length + payload

// ReadRequest reads a single ADB request from the stream. Returns "", io.EOF on EOF.
func ReadRequest(r io.Reader) (string, error) {
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(r, lengthBytes); err != nil {
		return "", err
	}

	length, err := strconv.ParseInt(string(lengthBytes), 16, 32)
	if err != nil {
		return "", fmt.Errorf("invalid length hex: %s", string(lengthBytes))
	}

	if length < 0 || length > 1<<20 {
		return "", fmt.Errorf("request length out of range: %d", length)
	}

	if length == 0 {
		return "", nil
	}

	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", err
	}

	return string(payload), nil
}

// WriteOkay writes an OKAY response with no payload.
func WriteOkay(w io.Writer) error {
	_, err := w.Write([]byte("OKAY"))
	return err
}

// WriteOkayWithPayload writes an OKAY response with length-prefixed payload.
// The entire message is sent in a single Write call to reduce the chance of
// the ADB client seeing a partial response between syscalls.
func WriteOkayWithPayload(w io.Writer, payload string) error {
	data := []byte(payload)
	if len(data) > 0xFFFF {
		return fmt.Errorf("payload too large for ADB framing: %d bytes (max 65535)", len(data))
	}
	msg := make([]byte, 0, 4+4+len(data))
	msg = append(msg, "OKAY"...)
	msg = append(msg, fmt.Sprintf("%04X", len(data))...)
	msg = append(msg, data...)
	_, err := w.Write(msg)
	return err
}

// WriteFail writes a FAIL response with error message.
// The entire message is sent in a single Write call to reduce the chance of
// the ADB client seeing a partial response between syscalls.
func WriteFail(w io.Writer, message string) error {
	data := []byte(message)
	if len(data) > 0xFFFF {
		return fmt.Errorf("error message too large for ADB framing: %d bytes (max 65535)", len(data))
	}
	msg := make([]byte, 0, 4+4+len(data))
	msg = append(msg, "FAIL"...)
	msg = append(msg, fmt.Sprintf("%04X", len(data))...)
	msg = append(msg, data...)
	_, err := w.Write(msg)
	return err
}

// WriteRaw writes raw bytes (for streaming shell output).
func WriteRaw(w io.Writer, data []byte) error {
	_, err := w.Write(data)
	return err
}

// Shell v2 protocol packet IDs.
const (
	shellV2Stdout byte = 1
	shellV2Stderr byte = 2
	shellV2Exit   byte = 3
)

// WriteShellV2Packet writes a shell v2 framed packet: 1-byte ID + 4-byte LE length + payload.
func WriteShellV2Packet(w io.Writer, id byte, data []byte) error {
	header := make([]byte, 5)
	header[0] = id
	binary.LittleEndian.PutUint32(header[1:], uint32(len(data)))
	packet := make([]byte, 0, 5+len(data))
	packet = append(packet, header...)
	packet = append(packet, data...)
	_, err := w.Write(packet)
	return err
}

// FormatDeviceLine formats a device list entry as ADB expects: "<serial>\t<state>\n"
func FormatDeviceLine(serial string) string {
	return serial + "\tdevice\n"
}

// FormatDeviceLineLong formats a long device list entry.
func FormatDeviceLineLong(serial, product, model, device string, transportID int) string {
	return fmt.Sprintf("%s\tdevice product:%s model:%s device:%s transport_id:%d\n",
		serial, product, model, device, transportID)
}

