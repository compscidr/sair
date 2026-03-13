package proxy

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestReadRequest(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "host:version",
			input: "000Chost:version",
			want:  "host:version",
		},
		{
			name:  "host:devices",
			input: "000Chost:devices",
			want:  "host:devices",
		},
		{
			name:  "empty payload",
			input: "0000",
			want:  "",
		},
		{
			name:  "host:transport with serial",
			input: "001Bhost:transport:ABC123DEF456",
			want:  "host:transport:ABC123DEF456",
		},
		{
			name:    "EOF",
			input:   "",
			wantErr: true,
		},
		{
			name:    "truncated length",
			input:   "00",
			wantErr: true,
		},
		{
			name:    "truncated payload",
			input:   "000Chost:",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadRequest(bytes.NewReader([]byte(tt.input)))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteOkay(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteOkay(&buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "OKAY" {
		t.Errorf("got %q, want %q", got, "OKAY")
	}
}

func TestWriteOkayWithPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteOkayWithPayload(&buf, "0029"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	want := "OKAY00040029"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteFail(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFail(&buf, "device not found"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	want := "FAIL0010device not found"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteRaw(t *testing.T) {
	var buf bytes.Buffer
	data := []byte("hello world")
	if err := WriteRaw(&buf, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("got %q, want %q", buf.Bytes(), data)
	}
}

func TestFormatDeviceLine(t *testing.T) {
	got := FormatDeviceLine("ABC123")
	want := "ABC123\tdevice\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatDeviceLineLong(t *testing.T) {
	got := FormatDeviceLineLong("ABC123", "Pixel_6", "Pixel_6", "Pixel_6", 3)
	want := "ABC123\tdevice product:Pixel_6 model:Pixel_6 device:Pixel_6 transport_id:3\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadRequestRoundTrip(t *testing.T) {
	// Simulate what an ADB client sends and what the proxy reads
	commands := []string{
		"host:version",
		"host:devices",
		"host:devices-l",
		"host:transport:SERIAL123",
		"shell:ls -la",
		"sync:",
	}
	for _, cmd := range commands {
		var buf bytes.Buffer
		// Write the command in ADB LTV format
		lengthHex := []byte(padHex(len(cmd)))
		buf.Write(lengthHex)
		buf.Write([]byte(cmd))

		got, err := ReadRequest(&buf)
		if err != nil {
			t.Fatalf("ReadRequest(%q): %v", cmd, err)
		}
		if got != cmd {
			t.Errorf("ReadRequest round-trip: got %q, want %q", got, cmd)
		}
	}
}

func padHex(n int) string {
	s := "0000" + hexStr(n)
	return s[len(s)-4:]
}

func hexStr(n int) string {
	const hex = "0123456789ABCDEF"
	if n == 0 {
		return "0"
	}
	var result []byte
	for n > 0 {
		result = append([]byte{hex[n%16]}, result...)
		n /= 16
	}
	return string(result)
}

func TestProtocolExchange(t *testing.T) {
	// Test a full host:version exchange
	clientWriter := &bytes.Buffer{}
	serverWriter := &bytes.Buffer{}

	// Client sends host:version
	cmd := "host:version"
	clientWriter.Write([]byte("000C"))
	clientWriter.Write([]byte(cmd))

	// Server reads request
	request, err := ReadRequest(clientWriter)
	if err != nil {
		t.Fatal(err)
	}
	if request != cmd {
		t.Fatalf("got %q, want %q", request, cmd)
	}

	// Server responds with version
	if err := WriteOkayWithPayload(serverWriter, "0029"); err != nil {
		t.Fatal(err)
	}

	// Verify response format
	resp := serverWriter.Bytes()
	if string(resp[:4]) != "OKAY" {
		t.Errorf("expected OKAY prefix, got %q", string(resp[:4]))
	}

	// Read the payload length
	payloadLen := resp[4:8]
	if string(payloadLen) != "0004" {
		t.Errorf("expected payload length 0004, got %q", string(payloadLen))
	}

	// Read the payload
	payload := string(resp[8:])
	if payload != "0029" {
		t.Errorf("expected payload 0029, got %q", payload)
	}
}

func TestWriteShellV2Packet(t *testing.T) {
	tests := []struct {
		name string
		id   byte
		data []byte
	}{
		{"stdout", shellV2Stdout, []byte("hello world\n")},
		{"stderr", shellV2Stderr, []byte("error msg\n")},
		{"exit zero", shellV2Exit, []byte{0}},
		{"exit nonzero", shellV2Exit, []byte{1}},
		{"empty stdout", shellV2Stdout, []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteShellV2Packet(&buf, tt.id, tt.data); err != nil {
				t.Fatal(err)
			}

			got := buf.Bytes()
			if len(got) != 5+len(tt.data) {
				t.Fatalf("packet length: got %d, want %d", len(got), 5+len(tt.data))
			}
			if got[0] != tt.id {
				t.Errorf("packet ID: got %d, want %d", got[0], tt.id)
			}
			payloadLen := binary.LittleEndian.Uint32(got[1:5])
			if payloadLen != uint32(len(tt.data)) {
				t.Errorf("payload length: got %d, want %d", payloadLen, len(tt.data))
			}
			if !bytes.Equal(got[5:], tt.data) {
				t.Errorf("payload: got %q, want %q", got[5:], tt.data)
			}
		})
	}
}

func TestWriteShellV2MultiplePackets(t *testing.T) {
	var buf bytes.Buffer

	// Write stdout + stderr + exit sequence
	if err := WriteShellV2Packet(&buf, shellV2Stdout, []byte("out\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteShellV2Packet(&buf, shellV2Stderr, []byte("err\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteShellV2Packet(&buf, shellV2Exit, []byte{0}); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	// Packet 1: stdout "out\n" = 5 + 4 = 9 bytes
	// Packet 2: stderr "err\n" = 5 + 4 = 9 bytes
	// Packet 3: exit 0 = 5 + 1 = 6 bytes
	wantLen := 9 + 9 + 6
	if len(data) != wantLen {
		t.Fatalf("total length: got %d, want %d", len(data), wantLen)
	}

	// Verify each packet ID
	if data[0] != shellV2Stdout {
		t.Errorf("packet 1 ID: got %d, want %d", data[0], shellV2Stdout)
	}
	if data[9] != shellV2Stderr {
		t.Errorf("packet 2 ID: got %d, want %d", data[9], shellV2Stderr)
	}
	if data[18] != shellV2Exit {
		t.Errorf("packet 3 ID: got %d, want %d", data[18], shellV2Exit)
	}
}

func TestReadRequestMultipleSequential(t *testing.T) {
	// Verify reading multiple sequential requests from the same stream
	var buf bytes.Buffer
	buf.Write([]byte("000Chost:version"))
	buf.Write([]byte("000Chost:devices"))

	req1, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if req1 != "host:version" {
		t.Errorf("req1: got %q, want %q", req1, "host:version")
	}

	req2, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if req2 != "host:devices" {
		t.Errorf("req2: got %q, want %q", req2, "host:devices")
	}

	// Third read should return EOF
	_, err = ReadRequest(&buf)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}
