package app

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	srv "github.com/langowarny/smartthings-mcp/internal/server"
	"github.com/langowarny/smartthings-mcp/internal/smartthings"
)

type Config struct {
	Transport   string
	Host        string
	Port        int
	Token       string
	BaseURL     string
	AccessToken string
	LocationID  string
}

type Application struct {
	cfg        Config
	logger     *zap.SugaredLogger
	server     *mcp.Server
	httpServer *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewApplication(cfg Config) (*Application, error) {
	// Configure logger
	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := zapCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Application{
		cfg:    cfg,
		logger: logger.Sugar(),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}, nil
}

// isAuthorized checks the request against the configured MCP access token.
// The token may be supplied as the "mcpAccessToken" query parameter (useful
// when registering a remote connector URL, e.g. with Claude, which does not
// let you attach custom headers) or as an "Authorization: Bearer" header.
// If no access token is configured, every request is allowed.
func isAuthorized(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}

	provided := r.URL.Query().Get("mcpAccessToken")
	if provided == "" {
		if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			provided = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}

	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (a *Application) Start() error {
	a.logger.Info("Starting SmartThings MCP Server...")

	// Initialize SmartThings Client
	stClient := smartthings.NewClient(a.cfg.Token, a.cfg.BaseURL)

	// Initialize MCP Server
	s := srv.NewMCPServer(a.logger, stClient, a.cfg.LocationID)
	a.server = s

	// Handle Transport
	switch a.cfg.Transport {
	case "stdio":
		go func() {
			defer close(a.done)
			// StdioTransport uses stdin/stdout
			transport := &mcp.StdioTransport{}
			if err := s.Run(a.ctx, transport); err != nil {
				a.logger.Errorf("Stdio server error: %v", err)
			}
		}()
	case "sse":
		port := a.cfg.Port
		if envPort := os.Getenv("PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil {
				port = p
			}
		}
		addr := fmt.Sprintf("%s:%d", a.cfg.Host, port)
		a.logger.Infof("Starting SSE server on %s", addr)
		if a.cfg.AccessToken == "" {
			a.logger.Warn("MCP_ACCESS_TOKEN is not set; the HTTP endpoint is unauthenticated and reachable by anyone who can hit it.")
		}

		sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
			// Parse configuration from query parameters
			token := r.URL.Query().Get("SMARTTHINGS_TOKEN")
			if token == "" {
				token = a.cfg.Token
			}

			baseURL := r.URL.Query().Get("ST_BASE_URL")
			if baseURL == "" {
				baseURL = a.cfg.BaseURL
			}

			// If no token is provided, we can't create a valid client.
			// However, the SDK expects a server to be returned.
			// We'll create one, but tools might fail if they need the token.
			// Ideally, we should probably return nil to signal 400 Bad Request if token is missing,
			// but for now let's fallback or proceed.
			if token == "" {
				a.logger.Warn("No SmartThings token provided in request or config; tools will be discoverable but execution will fail.")
				// We proceed with empty token to allow tool discovery
			}

			// Initialize SmartThings Client for this session
			stClient := smartthings.NewClient(token, baseURL)

			// Initialize MCP Server for this session. LocationID is intentionally
			// fixed server-side config only (not overridable via query params),
			// so the single-location scoping can't be bypassed by a caller.
			return srv.NewMCPServer(a.logger, stClient, a.cfg.LocationID)
		}, nil)

		mux := http.NewServeMux()
		mux.Handle("/mcp", sseHandler)
		mux.Handle("/", sseHandler) // For compatibility

		// CORS middleware
		corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, mcp-session-id, mcp-protocol-version")
			w.Header().Set("Access-Control-Expose-Headers", "mcp-session-id, mcp-protocol-version")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			if !isAuthorized(r, a.cfg.AccessToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			mux.ServeHTTP(w, r)
		})

		a.httpServer = &http.Server{
			Addr:    addr,
			Handler: corsHandler,
		}

		go func() {
			defer close(a.done)
			if err := a.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				a.logger.Errorf("SSE server error: %v", err)
			}
		}()
	case "stream":
		addr := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
		a.logger.Infof("Starting Stream server on %s", addr)
		if a.cfg.AccessToken == "" {
			a.logger.Warn("MCP_ACCESS_TOKEN is not set; the HTTP endpoint is unauthenticated and reachable by anyone who can hit it.")
		}

		streamHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			// Parse configuration from query parameters
			token := r.URL.Query().Get("smartThingsToken")
			if token == "" {
				token = a.cfg.Token
			}

			baseURL := r.URL.Query().Get("stBaseUrl")
			if baseURL == "" {
				baseURL = a.cfg.BaseURL
			}

			if token == "" {
				a.logger.Warn("No SmartThings token provided in request or config; tools will be discoverable but execution will fail.")
				// We proceed with empty token to allow tool discovery
			}

			// Initialize SmartThings Client for this session
			stClient := smartthings.NewClient(token, baseURL)

			// Initialize MCP Server for this session. LocationID is intentionally
			// fixed server-side config only (not overridable via query params),
			// so the single-location scoping can't be bypassed by a caller.
			return srv.NewMCPServer(a.logger, stClient, a.cfg.LocationID)
		}, nil)

		authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isAuthorized(r, a.cfg.AccessToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			streamHandler.ServeHTTP(w, r)
		})

		a.httpServer = &http.Server{
			Addr:    addr,
			Handler: authHandler,
		}

		go func() {
			defer close(a.done)
			if err := a.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				a.logger.Errorf("Stream server error: %v", err)
			}
		}()
	default:
		return fmt.Errorf("unsupported transport: %s", a.cfg.Transport)
	}

	// Handle Shutdown Signals
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-sigChan:
			a.logger.Infof("Received signal %v, shutting down...", sig)
			a.Stop()
		case <-a.ctx.Done():
		}
	}()

	return nil
}

func (a *Application) Stop() {
	a.cancel()

	if a.httpServer != nil {
		a.logger.Info("Shutting down HTTP server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.httpServer.Shutdown(ctx); err != nil {
			a.logger.Errorf("Server forced to shutdown: %v", err)
		}
		a.logger.Info("HTTP server stopped")
	}
}

func (a *Application) Wait() {
	<-a.done
	a.logger.Sync()
}
