package server

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/langowarny/smartthings-mcp/internal/smartthings"
)

// NewMCPServer creates and initializes a new MCP server. defaultLocationID,
// when non-empty, restricts all tools/resources to that single SmartThings
// location.
func NewMCPServer(logger *zap.SugaredLogger, client *smartthings.Client, defaultLocationID string) *mcp.Server {
	// Initialize the server implementation info
	impl := &mcp.Implementation{
		Name:    "SmartThings MCP",
		Version: "0.1.0",
	}

	// Create the server instance
	s := mcp.NewServer(impl, &mcp.ServerOptions{
		HasTools:     true,
		HasResources: true,
		HasPrompts:   true,
	})

	// Caches which location each device belongs to for this session, so
	// checkDeviceLocation doesn't need an extra SmartThings API round trip
	// for every location-scoped device lookup once the device has already
	// been seen via list_devices/list_devices_with_status.
	locCache := newDeviceLocationCache(5 * time.Minute)

	// Register tools
	RegisterTools(s, client, defaultLocationID, locCache)

	// Register resources
	RegisterResources(s, client, defaultLocationID, locCache)

	// Register prompts
	RegisterPrompts(s)

	return s
}
