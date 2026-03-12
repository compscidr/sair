package proxy

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// DeviceListTracker polls the orchestrator for the current device list and caches it.
// Also assigns stable transport_id values for each device serial.
type DeviceListTracker struct {
	commandRouter  *CommandRouter
	pollIntervalMs int64

	devices       atomic.Value // []*pb.DeviceInfo
	transportIDs  sync.Map     // serial -> int
	nextTransport atomic.Int32

	stopCh chan struct{}
}

func NewDeviceListTracker(commandRouter *CommandRouter, pollIntervalMs int64) *DeviceListTracker {
	if pollIntervalMs <= 0 {
		pollIntervalMs = 5000
	}
	t := &DeviceListTracker{
		commandRouter:  commandRouter,
		pollIntervalMs: pollIntervalMs,
		stopCh:         make(chan struct{}),
	}
	t.devices.Store([]*pb.DeviceInfo{})
	t.nextTransport.Store(1)
	return t
}

func (t *DeviceListTracker) Start() {
	t.poll()

	go func() {
		ticker := time.NewTicker(time.Duration(t.pollIntervalMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.poll()
			case <-t.stopCh:
				return
			}
		}
	}()
}

func (t *DeviceListTracker) GetDevices() []*pb.DeviceInfo {
	return t.devices.Load().([]*pb.DeviceInfo)
}

// GetTransportID returns the transport_id for a device serial. Returns 0 if unknown.
func (t *DeviceListTracker) GetTransportID(serial string) int {
	if v, ok := t.transportIDs.Load(serial); ok {
		return v.(int)
	}
	return 0
}

// GetSerialByTransportID returns the device serial for a transport_id. Returns "" if unknown.
func (t *DeviceListTracker) GetSerialByTransportID(transportID int) string {
	var result string
	t.transportIDs.Range(func(key, value any) bool {
		if value.(int) == transportID {
			result = key.(string)
			return false
		}
		return true
	})
	return result
}

func (t *DeviceListTracker) poll() {
	deviceList, err := t.commandRouter.ListDevices()
	if err != nil {
		slog.Warn("failed to poll device list", "error", err)
		return
	}

	var allDevices []*pb.DeviceInfo
	for _, source := range deviceList.Sources {
		if source.Online {
			allDevices = append(allDevices, source.Devices...)
		}
	}

	// Assign transport IDs for new devices
	currentSerials := make(map[string]struct{})
	for _, device := range allDevices {
		currentSerials[device.Serial] = struct{}{}
		if _, loaded := t.transportIDs.LoadOrStore(device.Serial, int(t.nextTransport.Add(1)-1)); loaded {
			// Already existed
		}
	}

	// Remove transport IDs for devices that are gone
	t.transportIDs.Range(func(key, _ any) bool {
		if _, exists := currentSerials[key.(string)]; !exists {
			t.transportIDs.Delete(key)
		}
		return true
	})

	t.devices.Store(allDevices)
	slog.Debug("device list updated", "count", len(allDevices))
}

func (t *DeviceListTracker) Stop() {
	close(t.stopCh)
}
