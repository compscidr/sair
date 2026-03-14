package proxy

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/compscidr/sair/proto/orchestrator"
)

// DeviceListTracker maintains the device list reported by device-sources.
// Device-sources push their device lists via the proxy's HTTP API; this
// tracker caches them and periodically reports to the orchestrator for
// lock management. Also assigns stable transport_id values per serial
// and tracks which source owns each serial for command routing.
type DeviceListTracker struct {
	commandRouter *CommandRouter

	mu      sync.Mutex
	sources map[string]sourceEntry // sourceAddr -> devices

	devices       atomic.Value // []*pb.DeviceInfo
	serialToSource sync.Map    // serial -> sourceAddr
	transportIDs  sync.Map     // serial -> int
	nextTransport atomic.Int32

	reportInterval time.Duration
	stopCh         chan struct{}
}

type sourceEntry struct {
	devices  []*pb.DeviceInfo
	lastSeen time.Time
}

func NewDeviceListTracker(commandRouter *CommandRouter) *DeviceListTracker {
	t := &DeviceListTracker{
		commandRouter:  commandRouter,
		sources:        make(map[string]sourceEntry),
		reportInterval: 10 * time.Second,
		stopCh:         make(chan struct{}),
	}
	t.devices.Store([]*pb.DeviceInfo{})
	t.nextTransport.Store(1)
	return t
}

func (t *DeviceListTracker) Start() {
	// Periodically report devices to orchestrator and reap stale sources
	go func() {
		ticker := time.NewTicker(t.reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.reapAndReport()
			case <-t.stopCh:
				return
			}
		}
	}()
}

// UpdateDevices is called when a device-source pushes its device list.
func (t *DeviceListTracker) UpdateDevices(sourceAddr string, devices []*pb.DeviceInfo) {
	t.mu.Lock()
	t.sources[sourceAddr] = sourceEntry{devices: devices, lastSeen: time.Now()}
	t.mu.Unlock()

	t.rebuild()
}

func (t *DeviceListTracker) GetDevices() []*pb.DeviceInfo {
	return t.devices.Load().([]*pb.DeviceInfo)
}

// GetSourceAddr returns the device-source gRPC address that owns this serial.
func (t *DeviceListTracker) GetSourceAddr(serial string) string {
	if v, ok := t.serialToSource.Load(serial); ok {
		return v.(string)
	}
	return ""
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

// rebuild merges all source device lists into the cached device list.
func (t *DeviceListTracker) rebuild() {
	t.mu.Lock()
	var allDevices []*pb.DeviceInfo
	serialSourceMap := make(map[string]string) // serial -> sourceAddr
	for addr, entry := range t.sources {
		for _, device := range entry.devices {
			serialSourceMap[device.Serial] = addr
		}
		allDevices = append(allDevices, entry.devices...)
	}
	t.mu.Unlock()

	// Update serial → source mapping
	for serial, addr := range serialSourceMap {
		t.serialToSource.Store(serial, addr)
	}

	// Assign transport IDs for new devices
	currentSerials := make(map[string]struct{})
	for _, device := range allDevices {
		currentSerials[device.Serial] = struct{}{}
		if _, loaded := t.transportIDs.Load(device.Serial); !loaded {
			t.transportIDs.Store(device.Serial, int(t.nextTransport.Add(1)-1))
		}
	}

	// Remove transport IDs and source mappings for devices that are gone
	t.transportIDs.Range(func(key, _ any) bool {
		if _, exists := currentSerials[key.(string)]; !exists {
			t.transportIDs.Delete(key)
			t.serialToSource.Delete(key)
		}
		return true
	})

	t.devices.Store(allDevices)
	slog.Debug("device list updated", "count", len(allDevices))
}

// reapAndReport removes stale sources and reports current devices to orchestrator.
func (t *DeviceListTracker) reapAndReport() {
	staleThreshold := 30 * time.Second
	now := time.Now()

	var staleAddrs []string
	t.mu.Lock()
	for addr, entry := range t.sources {
		if now.Sub(entry.lastSeen) > staleThreshold {
			slog.Warn("removing stale device source", "addr", addr)
			delete(t.sources, addr)
			staleAddrs = append(staleAddrs, addr)
		}
	}
	t.mu.Unlock()

	for _, addr := range staleAddrs {
		t.commandRouter.RemoveDSClient(addr)
	}

	t.rebuild()

	allDevices := t.GetDevices()
	if err := t.commandRouter.ReportDevices(allDevices); err != nil {
		slog.Warn("failed to report devices to orchestrator", "error", err)
	}
}

func (t *DeviceListTracker) Stop() {
	close(t.stopCh)
}
