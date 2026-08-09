package server

import (
	"sync"
	"time"

	"github.com/langowarny/smartthings-mcp/internal/smartthings"
)

// deviceLocationCache remembers which location a device belongs to, so
// repeated location-scoping checks (see checkDeviceLocation) don't need a
// fresh SmartThings API round trip for devices already seen via
// list_devices/list_devices_with_status in this session. Entries expire
// after ttl so a device moved between locations is eventually re-verified.
type deviceLocationCache struct {
	mu      sync.RWMutex
	entries map[string]deviceLocationEntry
	ttl     time.Duration
}

type deviceLocationEntry struct {
	locationID string
	expiry     time.Time
}

func newDeviceLocationCache(ttl time.Duration) *deviceLocationCache {
	return &deviceLocationCache{
		entries: make(map[string]deviceLocationEntry),
		ttl:     ttl,
	}
}

func (c *deviceLocationCache) get(deviceID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[deviceID]
	if !ok || time.Now().After(e.expiry) {
		return "", false
	}
	return e.locationID, true
}

func (c *deviceLocationCache) set(deviceID, locationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[deviceID] = deviceLocationEntry{locationID: locationID, expiry: time.Now().Add(c.ttl)}
}

func (c *deviceLocationCache) setMany(devices []smartthings.Device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := time.Now().Add(c.ttl)
	for _, d := range devices {
		c.entries[d.DeviceID] = deviceLocationEntry{locationID: d.LocationID, expiry: exp}
	}
}
