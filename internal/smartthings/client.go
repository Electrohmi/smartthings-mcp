package smartthings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	BaseURL = "https://api.smartthings.com"
)

// Client is a minimal SmartThings REST API client.
// It is **not** feature-complete—only endpoints needed by the MCP server.

// TokenSource supplies the bearer token for each API call. Implementations
// may return a fixed value (a personal access token) or manage a live
// OAuth2 access/refresh token pair (see OAuthTokenSource).
type TokenSource interface {
	Token() (string, error)
}

// staticToken is a TokenSource that always returns the same value — used
// for the legacy SMARTTHINGS_TOKEN (personal access token) flow.
type staticToken string

func (s staticToken) Token() (string, error) { return string(s), nil }

type Client struct {
	tokenSource TokenSource
	baseURL     string
	http        *http.Client
}

// NewClient returns a new SmartThings API client authenticated with a
// static token (a personal access token). Personal access tokens issued
// after 2024-12-30 expire after 24 hours; for anything long-running, use
// NewClientWithTokenSource with an OAuthTokenSource instead.
func NewClient(token, baseURL string) *Client {
	return NewClientWithTokenSource(staticToken(token), baseURL)
}

// NewClientWithTokenSource returns a new SmartThings API client that pulls
// its bearer token from ts on every request, allowing the token to be
// rotated (e.g. by an OAuthTokenSource's background refresh) without
// re-creating the client.
func NewClientWithTokenSource(ts TokenSource, baseURL string) *Client {
	if baseURL == "" {
		baseURL = BaseURL
	}
	return &Client{
		tokenSource: ts,
		baseURL:     baseURL,
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Device represents SmartThings device metadata.
type Device struct {
	DeviceID   string `json:"deviceId"`
	LocationID string `json:"locationId"`
	Name       string `json:"name"`
	Label      string `json:"label"`
	DeviceType string `json:"deviceTypeName"`
	Components []struct {
		ID           string `json:"id"`
		Capabilities []struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		} `json:"capabilities"`
		// Categories classify what a component actually is (e.g. "Light",
		// "SmartPlug", "Thermostat") and are a far more reliable filter for
		// "what kind of device is this" than capability IDs — capabilities
		// like "switch" are shared by lights, plugs, and many appliances.
		Categories []struct {
			Name string `json:"name"`
		} `json:"categories,omitempty"`
	} `json:"components,omitempty"`
}

// Location represents a SmartThings location.
type Location struct {
	LocationID string `json:"locationId"`
	Name       string `json:"name"`
	// TimeZoneID (e.g. "Asia/Seoul") is the location's configured timezone.
	// Every timestamp value elsewhere in the SmartThings API (device
	// status/history, scene dates, etc.) is UTC — use this to convert to
	// local time before presenting it to a user.
	TimeZoneID string `json:"timeZoneId,omitempty"`
}

// Scene represents a SmartThings scene.
type Scene struct {
	SceneID          string   `json:"sceneId"`
	SceneName        string   `json:"sceneName"`
	LocationID       string   `json:"locationId"`
	CreatedDate      flexTime `json:"createdDate"`
	LastExecutedDate flexTime `json:"lastExecutedDate"`
}

// flexTime unmarshals a SmartThings date field that may come back as an
// RFC3339 string (the documented format), a bare epoch-millisecond number
// (observed on some scenes, likely ones that have migrated to routines
// under the hood), or null/omitted (left as the zero value). The stock
// encoding/json time.Time only accepts the first case and errors on the
// others with "Time.UnmarshalJSON: input is not a JSON string", which was
// enough to fail decoding the entire scene list as soon as one such entry
// showed up.
type flexTime time.Time

func (t *flexTime) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" || s == `""` {
		*t = flexTime(time.Time{})
		return nil
	}
	if len(s) > 0 && s[0] == '"' {
		var parsed time.Time
		if err := json.Unmarshal(data, &parsed); err != nil {
			return err
		}
		*t = flexTime(parsed)
		return nil
	}
	var ms int64
	if err := json.Unmarshal(data, &ms); err != nil {
		return fmt.Errorf("flexTime: unsupported value %s: %w", s, err)
	}
	*t = flexTime(time.UnixMilli(ms))
	return nil
}

func (t flexTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t))
}

// GetLocation returns metadata for a single location by ID.
func (c *Client) GetLocation(id string) (*Location, error) {
	var loc Location
	if err := c.get(fmt.Sprintf("/v1/locations/%s", id), &loc); err != nil {
		return nil, err
	}
	return &loc, nil
}

// DeviceStatus wraps /status response (partial).
type DeviceStatus map[string]interface{}

// ListDevices fetches devices.
func (c *Client) ListDevices() ([]Device, error) {
	var resp struct {
		Items []Device `json:"items"`
	}
	if err := c.get("/v1/devices", &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// GetDevice returns metadata for a device.
func (c *Client) GetDevice(id string) (*Device, error) {
	var d Device
	if err := c.get(fmt.Sprintf("/v1/devices/%s", id), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// GetDeviceStatus returns live status of a device.
func (c *Client) GetDeviceStatus(id string) (DeviceStatus, error) {
	var status DeviceStatus
	if err := c.get(fmt.Sprintf("/v1/devices/%s/status", id), &status); err != nil {
		return nil, err
	}
	return status, nil
}

// GetDevicesStatusConcurrent fetches status for multiple devices in
// parallel, bounded by maxConcurrency (defaults to 8), instead of the
// caller issuing one sequential get_device_status round trip per device.
// Devices whose status fetch fails are reported in the errs map rather than
// aborting the whole batch.
func (c *Client) GetDevicesStatusConcurrent(deviceIDs []string, maxConcurrency int) (statuses map[string]DeviceStatus, errs map[string]error) {
	if maxConcurrency <= 0 {
		maxConcurrency = 8
	}

	type result struct {
		id     string
		status DeviceStatus
		err    error
	}

	sem := make(chan struct{}, maxConcurrency)
	results := make(chan result, len(deviceIDs))
	var wg sync.WaitGroup

	for _, id := range deviceIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status, err := c.GetDeviceStatus(id)
			results <- result{id: id, status: status, err: err}
		}(id)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	statuses = make(map[string]DeviceStatus, len(deviceIDs))
	errs = make(map[string]error)
	for r := range results {
		if r.err != nil {
			errs[r.id] = r.err
		} else {
			statuses[r.id] = r.status
		}
	}
	return statuses, errs
}

// SendDeviceCommand issues a command.
func (c *Client) SendDeviceCommand(id string, body interface{}) error {
	return c.post(fmt.Sprintf("/v1/devices/%s/commands", id), body, nil)
}

// ListLocations returns locations.
func (c *Client) ListLocations() ([]Location, error) {
	var resp struct {
		Items []Location `json:"items"`
	}
	if err := c.get("/v1/locations", &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

type ExecuteSceneResponse struct {
	SceneID string `json:"sceneId"`
	Status  string `json:"status"`
}

// ListDevicesByLocation fetches devices filtered by location ID.
func (c *Client) ListDevicesByLocation(locationID string) ([]Device, error) {
	var resp struct {
		Items []Device `json:"items"`
	}
	path := fmt.Sprintf("/v1/devices?locationId=%s", locationID)
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ExecuteScene triggers a scene.
func (c *Client) ExecuteScene(sceneID string) (*ExecuteSceneResponse, error) {
	var res ExecuteSceneResponse
	if err := c.post(fmt.Sprintf("/v1/scenes/%s/execute", sceneID), nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ListScenes returns scenes.
func (c *Client) ListScenes() ([]Scene, error) {
	var resp struct {
		Items []Scene `json:"items"`
	}
	if err := c.get("/v1/scenes", &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// Room represents a SmartThings room.
type Room struct {
	RoomID          string `json:"roomId"`
	LocationID      string `json:"locationId"`
	Name            string `json:"name"`
	BackgroundImage string `json:"backgroundImage,omitempty"`
}

// ListRooms returns rooms for a location.
func (c *Client) ListRooms(locationID string) ([]Room, error) {
	var resp struct {
		Items []Room `json:"items"`
	}
	if err := c.get(fmt.Sprintf("/v1/locations/%s/rooms", locationID), &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// CreateRoom creates a room in a location.
func (c *Client) CreateRoom(locationID, name string) (*Room, error) {
	req := map[string]string{
		"name": name,
	}
	var room Room
	if err := c.post(fmt.Sprintf("/v1/locations/%s/rooms", locationID), req, &room); err != nil {
		return nil, err
	}
	return &room, nil
}

// DeleteRoom deletes a room.
func (c *Client) DeleteRoom(locationID, roomID string) error {
	return c.delete(fmt.Sprintf("/v1/locations/%s/rooms/%s", locationID, roomID))
}

// Rule represents a SmartThings rule.
type Rule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Additional fields like actions can be added if needed, keeping it simple for now.
}

// ListRules returns rules, optionally filtered to a single location.
func (c *Client) ListRules(locationID string) ([]Rule, error) {
	var resp struct {
		Items []Rule `json:"items"`
	}
	path := "/v1/rules"
	if locationID != "" {
		path += "?locationId=" + locationID
	}
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// Hub represents a SmartThings hub.
type Hub struct {
	HubID           string `json:"hubId"`
	Name            string `json:"name"`
	FirmwareVersion string `json:"firmwareVersion"`
}

// HubHealth represents the health status of a hub.
type HubHealth struct {
	State string `json:"state"` // e.g., "ONLINE", "OFFLINE"
}

// ListHubs returns list of hubs.
func (c *Client) ListHubs() ([]Hub, error) {
	var resp struct {
		Items []Hub `json:"items"`
	}
	if err := c.get("/v1/hubs", &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// GetHubHealth returns health status of a hub.
func (c *Client) GetHubHealth(hubID string) (*HubHealth, error) {
	var health HubHealth
	if err := c.get(fmt.Sprintf("/v1/hubs/%s/health", hubID), &health); err != nil {
		return nil, err
	}
	return &health, nil
}

// Subscription represents a SmartThings subscription.
type Subscription struct {
	ID               string `json:"id"`
	InstalledAppID   string `json:"installedAppId"`
	SourceType       string `json:"sourceType"` // e.g. DEVICE, CAPABILITY, MODE
	Capability       string `json:"capability,omitempty"`
	Attribute        string `json:"attribute,omitempty"`
	Value            string `json:"value,omitempty"`
	StateChangeOnly  bool   `json:"stateChangeOnly,omitempty"`
	SubscriptionName string `json:"subscriptionName,omitempty"`
}

// ListSubscriptions returns subscriptions for an installed app.
func (c *Client) ListSubscriptions(installedAppID string) ([]Subscription, error) {
	var resp struct {
		Items []Subscription `json:"items"`
	}
	if err := c.get(fmt.Sprintf("/v1/installedapps/%s/subscriptions", installedAppID), &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// CreateSubscriptionRequest defines the payload for creating a subscription.
type CreateSubscriptionRequest struct {
	SourceType string `json:"sourceType"`
	Device     *struct {
		DeviceID        string `json:"deviceId"`
		ComponentID     string `json:"componentId,omitempty"`
		Capability      string `json:"capability"`
		Attribute       string `json:"attribute"`
		StateChangeOnly bool   `json:"stateChangeOnly,omitempty"`
	} `json:"device,omitempty"`
	// Simplified for device subscriptions for now
}

// CreateSubscription creates a new subscription.
func (c *Client) CreateSubscription(installedAppID string, req CreateSubscriptionRequest) (*Subscription, error) {
	var sub Subscription
	if err := c.post(fmt.Sprintf("/v1/installedapps/%s/subscriptions", installedAppID), req, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// DeleteSubscription deletes a subscription.
func (c *Client) DeleteSubscription(installedAppID, subscriptionID string) error {
	return c.delete(fmt.Sprintf("/v1/installedapps/%s/subscriptions/%s", installedAppID, subscriptionID))
}

// Schedule represents a SmartThings schedule.
type Schedule struct {
	ScheduleID string `json:"scheduleId"`
	Name       string `json:"name"`
	Cron       *struct {
		Expression string `json:"expression"`
		Timezone   string `json:"timezone"`
	} `json:"cron,omitempty"`
}

// ListSchedules returns schedules for an installed app.
func (c *Client) ListSchedules(installedAppID string) ([]Schedule, error) {
	var resp struct {
		Items []Schedule `json:"items"`
	}
	if err := c.get(fmt.Sprintf("/v1/installedapps/%s/schedules", installedAppID), &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// CreateScheduleRequest defines the payload for creating a schedule.
type CreateScheduleRequest struct {
	Name string `json:"name"`
	Cron *struct {
		Expression string `json:"expression"`
		Timezone   string `json:"timezone"`
	} `json:"cron,omitempty"`
	// Simplified for cron schedules
}

// CreateSchedule creates a new schedule.
func (c *Client) CreateSchedule(installedAppID string, req CreateScheduleRequest) (*Schedule, error) {
	var sch Schedule
	if err := c.post(fmt.Sprintf("/v1/installedapps/%s/schedules", installedAppID), req, &sch); err != nil {
		return nil, err
	}
	return &sch, nil
}

// DeleteSchedule deletes a schedule.
func (c *Client) DeleteSchedule(installedAppID, scheduleID string) error {
	return c.delete(fmt.Sprintf("/v1/installedapps/%s/schedules/%s", installedAppID, scheduleID))
}

// GetDeviceHistory returns recent event history for a device via
// SmartThings' history API. Unlike most other endpoints this lives at
// /v1/history/devices (not /v1/devices/{id}/history — that path doesn't
// exist and was returning 406 on every call) and requires locationId as a
// query parameter alongside deviceId, so the device is looked up first to
// resolve it. Note SmartThings only makes the last 7 days of history
// queryable regardless of limit.
//
// The exact response shape isn't nailed down by a documented Go type here
// (unlike the other endpoints in this file) since this endpoint has never
// successfully returned real data to verify field names against — decoding
// into a generic map avoids silently dropping fields we guessed wrong,
// unlike a struct that happens to compile but doesn't match reality.
func (c *Client) GetDeviceHistory(deviceID string) ([]map[string]interface{}, error) {
	dev, err := c.GetDevice(deviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve device's location for history: %w", err)
	}
	var resp struct {
		Items []map[string]interface{} `json:"items"`
	}
	path := fmt.Sprintf("/v1/history/devices?locationId=%s&deviceId=%s&limit=20", dev.LocationID, deviceID)
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// CapabilityDefinition represents a SmartThings capability.
type CapabilityDefinition struct {
	ID         string                 `json:"id"`
	Version    int                    `json:"version"`
	Status     string                 `json:"status"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	Commands   map[string]interface{} `json:"commands,omitempty"`
}

// GetCapability returns capability definition.
func (c *Client) GetCapability(capabilityID string, version int) (*CapabilityDefinition, error) {
	var cap CapabilityDefinition
	if err := c.get(fmt.Sprintf("/v1/capabilities/%s/%d", capabilityID, version), &cap); err != nil {
		return nil, err
	}
	return &cap, nil
}

// Helpers
func (c *Client) token() (string, error) {
	if c.tokenSource == nil {
		return "", fmt.Errorf("SmartThings token not configured. Please set the SMARTTHINGS_TOKEN (or SMARTTHINGS_CLIENT_ID/SMARTTHINGS_CLIENT_SECRET for OAuth) environment variable")
	}
	token, err := c.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("SmartThings token unavailable: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("SmartThings token not configured. Please set the SMARTTHINGS_TOKEN (or SMARTTHINGS_CLIENT_ID/SMARTTHINGS_CLIENT_SECRET for OAuth) environment variable")
	}
	return token, nil
}

func (c *Client) get(path string, out interface{}) error {
	token, err := c.token()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("SmartThings API error: %s (path: %s)", res.Status, path)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c *Client) post(path string, body interface{}, out interface{}) error {
	token, err := c.token()
	if err != nil {
		return err
	}

	var buf *bytes.Buffer
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		buf = bytes.NewBuffer(data)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, buf)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("SmartThings API error: %s (path: %s)", res.Status, path)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

func (c *Client) delete(path string) error {
	token, err := c.token()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("SmartThings API error: %s (path: %s)", res.Status, path)
	}
	return nil
}
