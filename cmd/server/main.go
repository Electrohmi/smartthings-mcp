package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/langowarny/smartthings-mcp/internal/app"
)

func main() {
	var transport string
	var host string
	var port int
	flag.StringVar(&transport, "transport", "stream", "Transport to use: stdio|sse|stream")
	flag.StringVar(&host, "host", "0.0.0.0", "Host to bind (default 0.0.0.0)")
	flag.IntVar(&port, "port", 8081, "Port to listen on for SSE transport")
	flag.Parse()

	cfg := app.Config{
		Transport:   transport,
		Host:        host,
		Port:        port,
		Token:       os.Getenv("SMARTTHINGS_TOKEN"),
		BaseURL:     os.Getenv("ST_BASE_URL"),
		AccessToken: os.Getenv("MCP_ACCESS_TOKEN"),
	}

	application, err := app.NewApplication(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize application: %v\n", err)
		os.Exit(1)
	}

	if err := application.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start application: %v\n", err)
		os.Exit(1)
	}

	application.Wait()
}
