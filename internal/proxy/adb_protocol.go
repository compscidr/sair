package proxy

import (
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

	if length == 0 {
		return "", nil
	}

	payload := make([]byte, length)
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
func WriteOkayWithPayload(w io.Writer, payload string) error {
	if _, err := w.Write([]byte("OKAY")); err != nil {
		return err
	}
	return writeLengthPrefixed(w, payload)
}

// WriteFail writes a FAIL response with error message.
func WriteFail(w io.Writer, message string) error {
	if _, err := w.Write([]byte("FAIL")); err != nil {
		return err
	}
	return writeLengthPrefixed(w, message)
}

// WriteRaw writes raw bytes (for streaming shell output).
func WriteRaw(w io.Writer, data []byte) error {
	_, err := w.Write(data)
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

func writeLengthPrefixed(w io.Writer, data string) error {
	bytes := []byte(data)
	lengthHex := fmt.Sprintf("%04X", len(bytes))
	if _, err := w.Write([]byte(lengthHex)); err != nil {
		return err
	}
	_, err := w.Write(bytes)
	return err
}
