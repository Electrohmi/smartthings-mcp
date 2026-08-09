package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/langowarny/smartthings-mcp/internal/smartthings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterTools registers SmartThings tools with MCP server.
// defaultLocationID, when non-empty, restricts every tool to that single
// SmartThings location: other locations are hidden from list_locations and
// any attempt to read or command a device/scene/room outside it is rejected.
func RegisterTools(s *mcp.Server, client *smartthings.Client, defaultLocationID string, locCache *deviceLocationCache) {
	// list_devices
	s.AddTool(&mcp.Tool{
		Name:        "list_devices",
		Description: "List SmartThings devices",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location_id": map[string]interface{}{
					"type":        "string",
					"description": "Optional location ID to filter devices. If not provided, returns devices from all locations.",
				},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		loc, _ := args["location_id"].(string)
		loc, err := resolveLocation(loc, defaultLocationID)
		if err != nil {
			return errorResult(err.Error()), nil
		}

		var devices []smartthings.Device
		if loc != "" {
			devices, err = client.ListDevicesByLocation(loc)
		} else {
			devices, err = client.ListDevices()
		}
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{
						Text: fmt.Sprintf("Error: %v", err),
					},
				},
			}, nil
		}
		locCache.setMany(devices)
		data, _ := json.Marshal(devices)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.8,
					},
				},
			},
		}, nil
	})

	// list_devices_with_status
	s.AddTool(&mcp.Tool{
		Name: "list_devices_with_status",
		Description: "List SmartThings devices together with their live status, fetched in parallel server-side. " +
			"Prefer this over list_devices + get_device_status per device when you need the status of many devices " +
			"(e.g. 'what lights are on') — it's one round trip instead of one per device. Pass 'category' (and/or " +
			"'capability') to narrow which devices are included whenever you only care about one kind of device — " +
			"on accounts with many devices or complex appliances, fetching status for everything is slow and can " +
			"produce a very large response.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location_id": map[string]interface{}{
					"type":        "string",
					"description": "Optional location ID to filter devices. If not provided, returns devices from all locations.",
				},
				"category": map[string]interface{}{
					"type":        "string",
					"description": "Optional device category to filter by (e.g. 'Light', 'SmartPlug', 'Thermostat', 'AirConditioner'). Prefer this over 'capability' when you're looking for a kind of device — capabilities like 'switch' are shared by lights, plugs, and many appliances (a fridge/washer/AC can all declare 'switch' on some component), so it over-matches. Case-insensitive.",
				},
				"capability": map[string]interface{}{
					"type":        "string",
					"description": "Optional capability ID to filter devices by (e.g. 'switch', 'switchLevel', 'thermostatMode'). Combined with 'category' if both are given. Prefer 'category' alone when you just want 'devices of kind X' — it's more precise.",
				},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		loc, _ := args["location_id"].(string)
		loc, err := resolveLocation(loc, defaultLocationID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		capability, _ := args["capability"].(string)
		category, _ := args["category"].(string)

		var devices []smartthings.Device
		if loc != "" {
			devices, err = client.ListDevicesByLocation(loc)
		} else {
			devices, err = client.ListDevices()
		}
		if err != nil {
			return errorResult(err.Error()), nil
		}
		locCache.setMany(devices)

		if capability != "" || category != "" {
			filtered := devices[:0:0]
			for _, d := range devices {
				if (capability == "" || deviceHasCapability(d, capability)) &&
					(category == "" || deviceHasCategory(d, category)) {
					filtered = append(filtered, d)
				}
			}
			devices = filtered
		}

		ids := make([]string, len(devices))
		for i, d := range devices {
			ids[i] = d.DeviceID
		}
		statuses, statusErrs := client.GetDevicesStatusConcurrent(ids, 8)

		// Deliberately omits the full capability/component list (see Device):
		// for accounts with complex appliances that alone can run tens of KB
		// per device, and it's rarely needed alongside status. Use get_device
		// or list_device_capabilities for that.
		type deviceWithStatus struct {
			DeviceID    string                   `json:"deviceId"`
			LocationID  string                   `json:"locationId"`
			Name        string                   `json:"name"`
			Label       string                   `json:"label"`
			DeviceType  string                   `json:"deviceTypeName"`
			Status      smartthings.DeviceStatus `json:"status,omitempty"`
			StatusError string                   `json:"statusError,omitempty"`
		}
		out := make([]deviceWithStatus, len(devices))
		for i, d := range devices {
			dws := deviceWithStatus{
				DeviceID:   d.DeviceID,
				LocationID: d.LocationID,
				Name:       d.Name,
				Label:      d.Label,
				DeviceType: d.DeviceType,
			}
			if st, ok := statuses[d.DeviceID]; ok {
				dws.Status = st
			} else if statusErr, ok := statusErrs[d.DeviceID]; ok {
				dws.StatusError = statusErr.Error()
			}
			out[i] = dws
		}

		data, _ := json.Marshal(out)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.9,
					},
				},
			},
		}, nil
	})

	// get_device
	s.AddTool(&mcp.Tool{
		Name:        "get_device",
		Description: "Get SmartThings device metadata",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"device_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the device to retrieve (e.g., UUID)",
				},
			},
			"required": []string{"device_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		id, _ := args["device_id"].(string)
		if id == "" {
			return errorResult("device_id required"), nil
		}
		d, err := client.GetDevice(id)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		locCache.set(id, d.LocationID)
		if defaultLocationID != "" && d.LocationID != defaultLocationID {
			return errorResult("access denied: device is outside the configured SmartThings location"), nil
		}
		data, _ := json.Marshal(d)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.9,
					},
				},
			},
		}, nil
	})

	// get_device_status
	s.AddTool(&mcp.Tool{
		Name:        "get_device_status",
		Description: "Get SmartThings device status",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"device_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the device to check status for",
				},
			},
			"required": []string{"device_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		id, ok := args["device_id"].(string)
		if !ok {
			return errorResult("device_id is required"), nil
		}
		if err := checkDeviceLocation(client, locCache, defaultLocationID, id); err != nil {
			return errorResult(err.Error()), nil
		}
		status, err := client.GetDeviceStatus(id)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(status)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.9, // Status is high priority
					},
				},
			},
		}, nil
	})

	// list_device_capabilities
	s.AddTool(&mcp.Tool{
		Name:        "list_device_capabilities",
		Description: "List capabilities supported by a SmartThings device",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"device_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the device to list capabilities for",
				},
			},
			"required": []string{"device_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		id, _ := args["device_id"].(string)
		if id == "" {
			return errorResult("device_id required"), nil
		}
		dev, err := client.GetDevice(id)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		locCache.set(id, dev.LocationID)
		if defaultLocationID != "" && dev.LocationID != defaultLocationID {
			return errorResult("access denied: device is outside the configured SmartThings location"), nil
		}
		capSet := make(map[string]struct{})
		for _, comp := range dev.Components {
			for _, capab := range comp.Capabilities {
				capSet[capab.ID] = struct{}{}
			}
		}
		caps := make([]string, 0, len(capSet))
		for c := range capSet {
			caps = append(caps, c)
		}
		data, _ := json.Marshal(caps)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("assistant")}, // Capabilities are mostly for the assistant
						Priority: 0.5,
					},
				},
			},
		}, nil
	})

	// send_device_command
	s.AddTool(&mcp.Tool{
		Name:        "send_device_command",
		Description: "Send command to SmartThings device",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"device_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the device to command",
				},
				"component": map[string]interface{}{
					"type":        "string",
					"description": "The component ID (default: main). Use 'main' for most single-component devices.",
				},
				"capability": map[string]interface{}{
					"type":        "string",
					"description": "The capability ID (e.g., switch, audioVolume). Must be supported by the device.",
				},
				"command": map[string]interface{}{
					"type":        "string",
					"description": "The command to execute (e.g., on, off, setVolume).",
				},
				"arguments": map[string]interface{}{
					"type":        "array",
					"description": "List of arguments for the command (e.g. [50] for setVolume). Optional.",
				},
			},
			"required": []string{"device_id", "capability", "command"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		id, _ := args["device_id"].(string)
		component, _ := args["component"].(string)
		if component == "" {
			component = "main"
		}
		capability, _ := args["capability"].(string)
		command, _ := args["command"].(string)
		arguments, _ := args["arguments"].([]interface{})

		if err := checkDeviceLocation(client, locCache, defaultLocationID, id); err != nil {
			return errorResult(err.Error()), nil
		}

		body := map[string]interface{}{
			"commands": []interface{}{
				map[string]interface{}{
					"component":  component,
					"capability": capability,
					"command":    command,
					"arguments":  arguments,
				},
			},
		}
		if err := client.SendDeviceCommand(id, body); err != nil {
			return errorResult(err.Error()), nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "ok",
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 1.0, // Command confirmation is critical
					},
				},
			},
		}, nil
	})

	// list_locations
	s.AddTool(&mcp.Tool{
		Name:        "list_locations",
		Description: "List SmartThings locations",
		InputSchema: map[string]interface{}{
			"type": "object",
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var locs []smartthings.Location
		if defaultLocationID != "" {
			loc, err := client.GetLocation(defaultLocationID)
			if err != nil {
				return errorResult(err.Error()), nil
			}
			locs = []smartthings.Location{*loc}
		} else {
			var err error
			locs, err = client.ListLocations()
			if err != nil {
				return errorResult(err.Error()), nil
			}
		}
		data, _ := json.Marshal(locs)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("assistant")},
						Priority: 0.6,
					},
				},
			},
		}, nil
	})

	// execute_scene
	s.AddTool(&mcp.Tool{
		Name:        "execute_scene",
		Description: "Execute SmartThings scene",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"scene_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the scene to execute",
				},
			},
			"required": []string{"scene_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		sceneID, _ := args["scene_id"].(string)
		if defaultLocationID != "" {
			scenes, err := client.ListScenes()
			if err != nil {
				return errorResult(err.Error()), nil
			}
			found := false
			for _, sc := range scenes {
				if sc.SceneID == sceneID && sc.LocationID == defaultLocationID {
					found = true
					break
				}
			}
			if !found {
				return errorResult("access denied: scene is outside the configured SmartThings location"), nil
			}
		}
		resp, err := client.ExecuteScene(sceneID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		out, _ := json.Marshal(resp)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(out),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 1.0,
					},
				},
			},
		}, nil
	})

	// list_scenes
	s.AddTool(&mcp.Tool{
		Name:        "list_scenes",
		Description: "List SmartThings scenes",
		InputSchema: map[string]interface{}{
			"type": "object",
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		scenes, err := client.ListScenes()
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if defaultLocationID != "" {
			filtered := make([]smartthings.Scene, 0, len(scenes))
			for _, sc := range scenes {
				if sc.LocationID == defaultLocationID {
					filtered = append(filtered, sc)
				}
			}
			scenes = filtered
		}
		data, _ := json.Marshal(scenes)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.6,
					},
				},
			},
		}, nil
	})

	// list_rooms
	s.AddTool(&mcp.Tool{
		Name:        "list_rooms",
		Description: "List rooms in a SmartThings location",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the location to list rooms for",
				},
			},
			"required": []string{"location_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		locID, _ := args["location_id"].(string)
		locID, err := resolveLocation(locID, defaultLocationID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if locID == "" {
			return errorResult("location_id is required"), nil
		}
		rooms, err := client.ListRooms(locID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(rooms)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("assistant")},
						Priority: 0.6,
					},
				},
			},
		}, nil
	})

	// create_room
	s.AddTool(&mcp.Tool{
		Name:        "create_room",
		Description: "Create a new room in a SmartThings location",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the location to create room in",
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "The name of the new room (e.g., 'Living Room', 'Office')",
				},
			},
			"required": []string{"location_id", "name"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		locID, _ := args["location_id"].(string)
		name, _ := args["name"].(string)
		locID, err := resolveLocation(locID, defaultLocationID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if locID == "" || name == "" {
			return errorResult("location_id and name are required"), nil
		}
		room, err := client.CreateRoom(locID, name)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(room)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.8,
					},
				},
			},
		}, nil
	})

	// delete_room
	s.AddTool(&mcp.Tool{
		Name:        "delete_room",
		Description: "Delete a room from a SmartThings location",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the location",
				},
				"room_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the room to delete",
				},
			},
			"required": []string{"location_id", "room_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		locID, _ := args["location_id"].(string)
		roomID, _ := args["room_id"].(string)
		locID, err := resolveLocation(locID, defaultLocationID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if locID == "" || roomID == "" {
			return errorResult("location_id and room_id are required"), nil
		}
		if err := client.DeleteRoom(locID, roomID); err != nil {
			return errorResult(err.Error()), nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "ok",
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.8,
					},
				},
			},
		}, nil
	})

	// list_rules
	s.AddTool(&mcp.Tool{
		Name:        "list_rules",
		Description: "List SmartThings automation rules",
		InputSchema: map[string]interface{}{
			"type": "object",
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rules, err := client.ListRules(defaultLocationID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(rules)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("assistant")},
						Priority: 0.6,
					},
				},
			},
		}, nil
	})

	// list_hubs
	s.AddTool(&mcp.Tool{
		Name:        "list_hubs",
		Description: "List SmartThings hubs",
		InputSchema: map[string]interface{}{
			"type": "object",
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hubs, err := client.ListHubs()
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(hubs)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.7,
					},
				},
			},
		}, nil
	})

	// get_hub_health
	s.AddTool(&mcp.Tool{
		Name:        "get_hub_health",
		Description: "Get health status of a SmartThings hub",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"hub_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the hub",
				},
			},
			"required": []string{"hub_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		hubID, _ := args["hub_id"].(string)
		if hubID == "" {
			return errorResult("hub_id is required"), nil
		}
		health, err := client.GetHubHealth(hubID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(health)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.9,
					},
				},
			},
		}, nil
	})

	// list_subscriptions
	s.AddTool(&mcp.Tool{
		Name:        "list_subscriptions",
		Description: "List subscriptions for an installed app",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"installed_app_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the installed app",
				},
			},
			"required": []string{"installed_app_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		appID, _ := args["installed_app_id"].(string)
		if appID == "" {
			return errorResult("installed_app_id is required"), nil
		}
		subs, err := client.ListSubscriptions(appID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(subs)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("assistant")},
						Priority: 0.6,
					},
				},
			},
		}, nil
	})

	// create_subscription
	s.AddTool(&mcp.Tool{
		Name:        "create_subscription",
		Description: "Create a subscription for device events",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"installed_app_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the installed app",
				},
				"device_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the device to subscribe to",
				},
				"capability": map[string]interface{}{
					"type":        "string",
					"description": "The capability to monitor (e.g., switch, audioVolume)",
				},
				"attribute": map[string]interface{}{
					"type":        "string",
					"description": "The attribute to monitor (e.g., switch, volume)",
				},
			},
			"required": []string{"installed_app_id", "device_id", "capability", "attribute"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		appID, _ := args["installed_app_id"].(string)
		deviceID, _ := args["device_id"].(string)
		capability, _ := args["capability"].(string)
		attribute, _ := args["attribute"].(string)

		subReq := smartthings.CreateSubscriptionRequest{
			SourceType: "DEVICE",
			Device: &struct {
				DeviceID        string `json:"deviceId"`
				ComponentID     string `json:"componentId,omitempty"`
				Capability      string `json:"capability"`
				Attribute       string `json:"attribute"`
				StateChangeOnly bool   `json:"stateChangeOnly,omitempty"`
			}{
				DeviceID:        deviceID,
				Capability:      capability,
				Attribute:       attribute,
				StateChangeOnly: true,
			},
		}

		sub, err := client.CreateSubscription(appID, subReq)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(sub)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: string(data),
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.8,
					},
				},
			},
		}, nil
	})

	// delete_subscription
	s.AddTool(&mcp.Tool{
		Name:        "delete_subscription",
		Description: "Delete a subscription",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"installed_app_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the installed app",
				},
				"subscription_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the subscription to delete",
				},
			},
			"required": []string{"installed_app_id", "subscription_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		appID, _ := args["installed_app_id"].(string)
		subID, _ := args["subscription_id"].(string)
		if err := client.DeleteSubscription(appID, subID); err != nil {
			return errorResult(err.Error()), nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "ok",
					Annotations: &mcp.Annotations{
						Audience: []mcp.Role{mcp.Role("user"), mcp.Role("assistant")},
						Priority: 0.8,
					},
				},
			},
		}, nil
	})

	// list_schedules
	s.AddTool(&mcp.Tool{
		Name:        "list_schedules",
		Description: "List schedules for an installed app",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"installed_app_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the installed app",
				},
			},
			"required": []string{"installed_app_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		appID, _ := args["installed_app_id"].(string)
		schedules, err := client.ListSchedules(appID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(schedules)
		return successResult(string(data)), nil
	})

	// create_schedule
	s.AddTool(&mcp.Tool{
		Name:        "create_schedule",
		Description: "Create a cron schedule",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"installed_app_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the installed app",
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "The name of the schedule (e.g., 'Morning Routine')",
				},
				"cron_expression": map[string]interface{}{
					"type":        "string",
					"description": "Cron expression for the schedule (e.g., '0 0 7 * * ?' for daily at 7 AM)",
				},
				"timezone": map[string]interface{}{
					"type":        "string",
					"description": "Timezone for the schedule (e.g., 'Asia/Seoul', 'UTC'). Defaults to UTC.",
				},
			},
			"required": []string{"installed_app_id", "name", "cron_expression"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		appID, _ := args["installed_app_id"].(string)
		name, _ := args["name"].(string)
		cronExpr, _ := args["cron_expression"].(string)
		timezone, _ := args["timezone"].(string)
		if timezone == "" {
			timezone = "UTC"
		}

		schReq := smartthings.CreateScheduleRequest{
			Name: name,
			Cron: &struct {
				Expression string `json:"expression"`
				Timezone   string `json:"timezone"`
			}{
				Expression: cronExpr,
				Timezone:   timezone,
			},
		}

		sch, err := client.CreateSchedule(appID, schReq)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(sch)
		return successResult(string(data)), nil
	})

	// delete_schedule
	s.AddTool(&mcp.Tool{
		Name:        "delete_schedule",
		Description: "Delete a schedule",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"installed_app_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the installed app",
				},
				"schedule_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the schedule to delete",
				},
			},
			"required": []string{"installed_app_id", "schedule_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		appID, _ := args["installed_app_id"].(string)
		schID, _ := args["schedule_id"].(string)
		if err := client.DeleteSchedule(appID, schID); err != nil {
			return errorResult(err.Error()), nil
		}
		return successResult("ok"), nil
	})

	// get_device_history
	s.AddTool(&mcp.Tool{
		Name:        "get_device_history",
		Description: "Get recent event history for a device",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"device_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the device to retrieve history for",
				},
			},
			"required": []string{"device_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		deviceID, _ := args["device_id"].(string)
		if deviceID == "" {
			return errorResult("device_id is required"), nil
		}
		if err := checkDeviceLocation(client, locCache, defaultLocationID, deviceID); err != nil {
			return errorResult(err.Error()), nil
		}
		history, err := client.GetDeviceHistory(deviceID)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(history)
		return successResult(string(data)), nil
	})

	// get_capability
	s.AddTool(&mcp.Tool{
		Name:        "get_capability",
		Description: "Get definition of a SmartThings capability",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"capability_id": map[string]interface{}{
					"type":        "string",
					"description": "The unique identifier of the capability (e.g., 'switch', 'audioVolume')",
				},
				"version": map[string]interface{}{
					"type":        "number",
					"description": "The version of the capability (default: 1)",
				},
			},
			"required": []string{"capability_id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult("invalid arguments"), nil
		}
		capID, _ := args["capability_id"].(string)
		versionFloat, ok := args["version"].(float64)
		version := 1
		if ok {
			version = int(versionFloat)
		}

		if capID == "" {
			return errorResult("capability_id is required"), nil
		}
		capDef, err := client.GetCapability(capID, version)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		data, _ := json.Marshal(capDef)
		return successResult(string(data)), nil
	})
}

// deviceHasCapability reports whether any component of the device declares
// the given capability ID.
func deviceHasCapability(d smartthings.Device, capability string) bool {
	for _, comp := range d.Components {
		for _, cap := range comp.Capabilities {
			if cap.ID == capability {
				return true
			}
		}
	}
	return false
}

// deviceHasCategory reports whether any component of the device is
// classified under the given category name (case-insensitive).
func deviceHasCategory(d smartthings.Device, category string) bool {
	for _, comp := range d.Components {
		for _, cat := range comp.Categories {
			if strings.EqualFold(cat.Name, category) {
				return true
			}
		}
	}
	return false
}

// resolveLocation validates a caller-supplied location_id against the
// server's configured default location. If no default is configured, the
// caller-supplied value passes through unchanged (including empty, meaning
// "all locations" where the tool supports that). If a default is
// configured, an empty caller value resolves to the default, and any other
// value must match it exactly.
func resolveLocation(provided, configured string) (string, error) {
	if configured == "" {
		return provided, nil
	}
	if provided != "" && provided != configured {
		return "", fmt.Errorf("access denied: this server is scoped to a single SmartThings location")
	}
	return configured, nil
}

// checkDeviceLocation rejects a device that falls outside the configured
// default location. It is a no-op when no default is set. The device's
// location is looked up from cache when available (populated by
// list_devices/list_devices_with_status); only on a cache miss does it cost
// an extra SmartThings API round trip.
func checkDeviceLocation(client *smartthings.Client, cache *deviceLocationCache, configured, deviceID string) error {
	if configured == "" {
		return nil
	}
	if locID, ok := cache.get(deviceID); ok {
		if locID != configured {
			return fmt.Errorf("access denied: device is outside the configured SmartThings location")
		}
		return nil
	}
	dev, err := client.GetDevice(deviceID)
	if err != nil {
		return err
	}
	cache.set(deviceID, dev.LocationID)
	if dev.LocationID != configured {
		return fmt.Errorf("access denied: device is outside the configured SmartThings location")
	}
	return nil
}

func successResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: text,
			},
		},
	}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: msg,
			},
		},
	}
}
