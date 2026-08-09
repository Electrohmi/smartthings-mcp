package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/langowarny/smartthings-mcp/internal/smartthings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterResources registers SmartThings resources (devices, status, locations)
// and wires them into the MCP server. defaultLocationID, when non-empty,
// applies the same single-location scoping as RegisterTools.
func RegisterResources(s *mcp.Server, client *smartthings.Client, defaultLocationID string, locCache *deviceLocationCache) {
	const mimeJSON = "application/json"

	// Simple cache keyed by URI → cacheEntry
	type cacheEntry struct {
		data   string
		expiry time.Time
	}

	var (
		cacheTTL   = 10 * time.Second
		cacheStore sync.Map // map[string]cacheEntry
	)

	getFromCache := func(uri string) (string, bool) {
		if v, ok := cacheStore.Load(uri); ok {
			ce := v.(cacheEntry)
			if time.Now().Before(ce.expiry) {
				return ce.data, true
			}
			cacheStore.Delete(uri)
		}
		return "", false
	}

	putCache := func(uri string, data string) {
		cacheStore.Store(uri, cacheEntry{data: data, expiry: time.Now().Add(cacheTTL)})
	}

	// Helper to wrap JSON string into ResourceContents slice.
	wrap := func(uri, jsonText string) []*mcp.ResourceContents {
		return []*mcp.ResourceContents{
			{
				URI:      uri,
				MIMEType: mimeJSON,
				Text:     jsonText,
			},
		}
	}

	// Template: st://devices/{device_id}
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "st://devices/{device_id}",
		Name:        "SmartThings Device",
		Description: "SmartThings device metadata (static configuration).",
		MIMEType:    mimeJSON,
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		if cached, ok := getFromCache(uri); ok {
			return &mcp.ReadResourceResult{Contents: wrap(uri, cached)}, nil
		}

		deviceID, err := extractDeviceID(uri)
		if err != nil {
			return nil, err
		}
		d, err := client.GetDevice(deviceID)
		if err != nil {
			return nil, err
		}
		locCache.set(deviceID, d.LocationID)
		if defaultLocationID != "" && d.LocationID != defaultLocationID {
			return nil, fmt.Errorf("access denied: device is outside the configured SmartThings location")
		}
		bytes, _ := json.Marshal(d)
		jsonText := string(bytes)
		putCache(uri, jsonText)
		return &mcp.ReadResourceResult{Contents: wrap(uri, jsonText)}, nil
	})

	// Template: st://devices/{device_id}/status
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "st://devices/{device_id}/status",
		Name:        "SmartThings Device Status",
		Description: "Live status map for a SmartThings device.",
		MIMEType:    mimeJSON,
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		if cached, ok := getFromCache(uri); ok {
			return &mcp.ReadResourceResult{Contents: wrap(uri, cached)}, nil
		}

		deviceID, err := extractDeviceID(uri)
		if err != nil {
			return nil, err
		}
		if err := checkDeviceLocation(client, locCache, defaultLocationID, deviceID); err != nil {
			return nil, err
		}
		status, err := client.GetDeviceStatus(deviceID)
		if err != nil {
			return nil, err
		}
		bytes, _ := json.Marshal(status)
		jsonText := string(bytes)
		putCache(uri, jsonText)
		return &mcp.ReadResourceResult{Contents: wrap(uri, jsonText)}, nil
	})

	// Template: st://locations/{location_id}
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "st://locations/{location_id}",
		Name:        "SmartThings Location",
		Description: "SmartThings location metadata.",
		MIMEType:    mimeJSON,
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		if cached, ok := getFromCache(uri); ok {
			return &mcp.ReadResourceResult{Contents: wrap(uri, cached)}, nil
		}

		locID, err := extractLocationID(uri)
		if err != nil {
			return nil, err
		}
		if defaultLocationID != "" && locID != defaultLocationID {
			return nil, fmt.Errorf("access denied: this server is scoped to a single SmartThings location")
		}
		loc, err := client.GetLocation(locID)
		if err != nil {
			return nil, err
		}
		bytes, _ := json.Marshal(loc)
		jsonText := string(bytes)
		putCache(uri, jsonText)
		return &mcp.ReadResourceResult{Contents: wrap(uri, jsonText)}, nil
	})

	// Template: st://locations
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "st://locations",
		Name:        "SmartThings Locations",
		Description: "List of all SmartThings locations.",
		MIMEType:    mimeJSON,
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		if cached, ok := getFromCache(uri); ok {
			return &mcp.ReadResourceResult{Contents: wrap(uri, cached)}, nil
		}

		var locs []smartthings.Location
		if defaultLocationID != "" {
			loc, err := client.GetLocation(defaultLocationID)
			if err != nil {
				return nil, err
			}
			locs = []smartthings.Location{*loc}
		} else {
			var err error
			locs, err = client.ListLocations()
			if err != nil {
				return nil, err
			}
		}
		bytes, _ := json.Marshal(locs)
		jsonText := string(bytes)
		putCache(uri, jsonText)
		return &mcp.ReadResourceResult{Contents: wrap(uri, jsonText)}, nil
	})
}

// extractDeviceID parses st://devices/{id} or st://devices/{id}/status and returns the id part.
func extractDeviceID(uri string) (string, error) {
	const prefix = "st://devices/"
	if !strings.HasPrefix(uri, prefix) {
		return "", fmt.Errorf("invalid device uri: %s", uri)
	}
	rest := strings.TrimPrefix(uri, prefix)
	// Trim optional suffix
	rest = strings.TrimSuffix(rest, "/status")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", fmt.Errorf("missing device id in uri: %s", uri)
	}
	return rest, nil
}

// extractLocationID parses st://locations/{id} and returns the id.
func extractLocationID(uri string) (string, error) {
	const prefix = "st://locations/"
	if !strings.HasPrefix(uri, prefix) {
		return "", fmt.Errorf("invalid location uri: %s", uri)
	}
	id := strings.TrimPrefix(uri, prefix)
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		return "", fmt.Errorf("missing location id in uri: %s", uri)
	}
	return id, nil
}
