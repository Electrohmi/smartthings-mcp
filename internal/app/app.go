package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

	// OAuth2 config. When ClientID/ClientSecret are both set, the server
	// authenticates to SmartThings via OAuth (access token refreshed
	// automatically, persisted at TokenStorePath) instead of the static
	// Token (personal access token, which expires after 24h for tokens
	// issued after 2024-12-30). RedirectURI must match what's registered
	// for the SmartApp and be reachable by the browser completing the
	// one-time consent — typically the server's public URL + /oauth/callback.
	ClientID       string
	ClientSecret   string
	RedirectURI    string
	TokenStorePath string
}

type Application struct {
	cfg        Config
	logger     *zap.SugaredLogger
	server     *mcp.Server
	httpServer *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}

	oauthSource *smartthings.OAuthTokenSource

	oauthStateMu sync.Mutex
	oauthState   string
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
//
// /oauth/* is always exempt: SmartThings' own redirect back to
// /oauth/callback has no way to carry our mcpAccessToken, and neither
// endpoint exposes device data or control — /oauth/authorize just redirects
// to SmartThings' own login-gated consent screen, and /oauth/callback is
// protected by the one-time state+code exchange instead.
func isAuthorized(r *http.Request, expected string) bool {
	if expected == "" || strings.HasPrefix(r.URL.Path, "/oauth/") {
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

// newSmartThingsClient builds a SmartThings API client. When OAuth is
// configured it always uses the shared, auto-refreshing OAuth token source
// (queryToken is ignored — mixing a per-request static token with a
// managed OAuth token doesn't make sense and would bypass the point of
// OAuth). Otherwise it falls back to the static token (config default or
// a per-request override via query parameter, for the sse/stream paths).
func (a *Application) newSmartThingsClient(queryToken, queryBaseURL string) *smartthings.Client {
	baseURL := queryBaseURL
	if baseURL == "" {
		baseURL = a.cfg.BaseURL
	}
	if a.oauthSource != nil {
		return smartthings.NewClientWithTokenSource(a.oauthSource, baseURL)
	}
	token := queryToken
	if token == "" {
		token = a.cfg.Token
	}
	if token == "" {
		a.logger.Warn("No SmartThings token provided in request or config; tools will be discoverable but execution will fail.")
	}
	return smartthings.NewClient(token, baseURL)
}

func (a *Application) Start() error {
	a.logger.Info("Starting SmartThings MCP Server...")

	if a.cfg.ClientID != "" || a.cfg.ClientSecret != "" {
		if a.cfg.ClientID == "" || a.cfg.ClientSecret == "" {
			return fmt.Errorf("SMARTTHINGS_CLIENT_ID and SMARTTHINGS_CLIENT_SECRET must both be set to use OAuth")
		}
		if a.cfg.RedirectURI == "" {
			return fmt.Errorf("SMARTTHINGS_REDIRECT_URI is required when using OAuth (SMARTTHINGS_CLIENT_ID is set)")
		}
		oauthSource, err := smartthings.NewOAuthTokenSource(a.cfg.ClientID, a.cfg.ClientSecret, a.cfg.TokenStorePath)
		if err != nil {
			return fmt.Errorf("failed to initialize SmartThings OAuth: %w", err)
		}
		a.oauthSource = oauthSource
		if oauthSource.HasRefreshToken() {
			a.logger.Info("SmartThings OAuth: loaded existing token store, refreshing in the background")
		} else {
			a.logger.Warnf("SmartThings OAuth: no token on file yet — visit %s once in a browser to grant access", strings.TrimSuffix(a.cfg.RedirectURI, "/oauth/callback")+"/oauth/authorize")
		}
		go oauthSource.RunBackgroundRefresh(a.ctx, func(err error) {
			a.logger.Errorf("SmartThings OAuth background refresh failed (will retry): %v", err)
		})
	}

	// Initialize SmartThings Client
	stClient := a.newSmartThingsClient(a.cfg.Token, a.cfg.BaseURL)

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
			// Initialize SmartThings Client for this session. See
			// newSmartThingsClient: the query overrides are ignored when
			// OAuth is configured.
			stClient := a.newSmartThingsClient(
				r.URL.Query().Get("SMARTTHINGS_TOKEN"),
				r.URL.Query().Get("ST_BASE_URL"),
			)

			// Initialize MCP Server for this session. LocationID is intentionally
			// fixed server-side config only (not overridable via query params),
			// so the single-location scoping can't be bypassed by a caller.
			return srv.NewMCPServer(a.logger, stClient, a.cfg.LocationID)
		}, nil)

		mux := http.NewServeMux()
		mux.Handle("/mcp", sseHandler)
		mux.HandleFunc("/oauth/authorize", a.oauthAuthorizeHandler)
		mux.HandleFunc("/oauth/callback", a.oauthCallbackHandler)
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
			// Initialize SmartThings Client for this session. See
			// newSmartThingsClient: the query overrides are ignored when
			// OAuth is configured.
			stClient := a.newSmartThingsClient(
				r.URL.Query().Get("smartThingsToken"),
				r.URL.Query().Get("stBaseUrl"),
			)

			// Initialize MCP Server for this session. LocationID is intentionally
			// fixed server-side config only (not overridable via query params),
			// so the single-location scoping can't be bypassed by a caller.
			return srv.NewMCPServer(a.logger, stClient, a.cfg.LocationID)
		}, nil)

		mux := http.NewServeMux()
		mux.HandleFunc("/oauth/authorize", a.oauthAuthorizeHandler)
		mux.HandleFunc("/oauth/callback", a.oauthCallbackHandler)
		mux.Handle("/", streamHandler)

		authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isAuthorized(r, a.cfg.AccessToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
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

// oauthAuthorizeHandler starts the one-time SmartThings OAuth consent flow
// by redirecting the browser to SmartThings' own login-gated authorize
// page, tagged with a fresh CSRF state value checked in oauthCallbackHandler.
func (a *Application) oauthAuthorizeHandler(w http.ResponseWriter, r *http.Request) {
	if a.oauthSource == nil {
		http.Error(w, "OAuth is not configured on this server (set SMARTTHINGS_CLIENT_ID/SMARTTHINGS_CLIENT_SECRET)", http.StatusNotFound)
		return
	}
	state, err := randomState()
	if err != nil {
		http.Error(w, "failed to generate state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.oauthStateMu.Lock()
	a.oauthState = state
	a.oauthStateMu.Unlock()
	http.Redirect(w, r, a.oauthSource.AuthorizeURL(a.cfg.RedirectURI, state), http.StatusFound)
}

// oauthCallbackHandler completes the flow: SmartThings redirects the
// browser here with a one-time code (and the state we handed it) after the
// account owner approves the consent screen.
func (a *Application) oauthCallbackHandler(w http.ResponseWriter, r *http.Request) {
	if a.oauthSource == nil {
		http.Error(w, "OAuth is not configured on this server", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		http.Error(w, fmt.Sprintf("SmartThings denied authorization: %s (%s)", errParam, q.Get("error_description")), http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	a.oauthStateMu.Lock()
	expected := a.oauthState
	a.oauthState = "" // one-time use, whether or not it matches
	a.oauthStateMu.Unlock()
	if expected == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(expected)) != 1 {
		http.Error(w, "invalid or expired state parameter — restart the flow at /oauth/authorize", http.StatusBadRequest)
		return
	}

	if err := a.oauthSource.ExchangeCode(code, a.cfg.RedirectURI); err != nil {
		a.logger.Errorf("SmartThings OAuth code exchange failed: %v", err)
		http.Error(w, "failed to exchange code for tokens: "+err.Error(), http.StatusBadGateway)
		return
	}

	a.logger.Info("SmartThings OAuth: authorization complete, token stored")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><body><h1>SmartThings connected</h1><p>You can close this window.</p></body></html>`)
}

// randomState generates a CSRF state value for the OAuth authorize request.
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
