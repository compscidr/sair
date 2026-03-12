package proxy

import (
	"testing"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

func TestDeviceListTrackerTransportIDs(t *testing.T) {
	// Test transport ID assignment without needing a real orchestrator.
	// We test the sync.Map-based transport ID logic directly.

	tracker := &DeviceListTracker{
		stopCh: make(chan struct{}),
	}
	tracker.devices.Store(([]*pb.DeviceInfo)(nil))
	tracker.nextTransport.Store(1)

	// Simulate assigning transport IDs
	serials := []string{"DEVICE_A", "DEVICE_B", "DEVICE_C"}
	for _, serial := range serials {
		tracker.transportIDs.LoadOrStore(serial, int(tracker.nextTransport.Add(1)-1))
	}

	// Verify each serial gets a unique, sequential transport ID
	for i, serial := range serials {
		tid := tracker.GetTransportID(serial)
		expected := i + 1
		if tid != expected {
			t.Errorf("GetTransportID(%q) = %d, want %d", serial, tid, expected)
		}
	}

	// Verify reverse lookup
	for i, serial := range serials {
		got := tracker.GetSerialByTransportID(i + 1)
		if got != serial {
			t.Errorf("GetSerialByTransportID(%d) = %q, want %q", i+1, got, serial)
		}
	}

	// Unknown serial returns 0
	if got := tracker.GetTransportID("UNKNOWN"); got != 0 {
		t.Errorf("GetTransportID(UNKNOWN) = %d, want 0", got)
	}

	// Unknown transport ID returns ""
	if got := tracker.GetSerialByTransportID(999); got != "" {
		t.Errorf("GetSerialByTransportID(999) = %q, want empty", got)
	}
}

func TestDeviceListTrackerStableIDs(t *testing.T) {
	tracker := &DeviceListTracker{
		stopCh: make(chan struct{}),
	}
	tracker.devices.Store(([]*pb.DeviceInfo)(nil))
	tracker.nextTransport.Store(1)

	// First assignment
	tracker.transportIDs.LoadOrStore("DEVICE_A", int(tracker.nextTransport.Add(1)-1))
	firstID := tracker.GetTransportID("DEVICE_A")

	// Subsequent LoadOrStore should NOT change the ID
	tracker.transportIDs.LoadOrStore("DEVICE_A", int(tracker.nextTransport.Add(1)-1))
	secondID := tracker.GetTransportID("DEVICE_A")

	if firstID != secondID {
		t.Errorf("transport ID changed: %d → %d", firstID, secondID)
	}
}
