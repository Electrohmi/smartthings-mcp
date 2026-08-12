package smartthings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	oauthAuthorizeURL = "https://api.smartthings.com/oauth/authorize"
	oauthTokenURL     = "https://api.smartthings.com/oauth/token"

	// refreshMargin is how long before actual expiry a token is treated as
	// expired, so Token() calls don't race a request against expiry.
	refreshMargin = 2 * time.Minute
)

// DefaultScopes covers everything this server's tools use. Devices/scenes
// need both read (r:) and execute (x:) scopes; locations/rooms/rules/hubs
// only need read+write as listed. Adjust if SmartThings adds new tools.
var DefaultScopes = []string{
	"r:devices:*", "x:devices:*",
	"r:locations:*",
	"r:scenes:*", "x:scenes:*",
	"r:rules:*", "w:rules:*",
	"r:hubs:*",
}

// storedTokens is the on-disk representation of an OAuth token pair.
type storedTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// OAuthTokenSource implements TokenSource on top of a SmartThings OAuth2
// authorization_code grant. The initial token pair is obtained via a
// one-time browser consent (ExchangeCode); after that, it refreshes the
// access token in the background before it expires and persists the
// refresh token to disk, since SmartThings rotates it on every use.
type OAuthTokenSource struct {
	clientID     string
	clientSecret string
	storePath    string
	httpClient   *http.Client

	mu     sync.RWMutex
	tokens storedTokens
}

// NewOAuthTokenSource creates a token source and loads any tokens already
// persisted at storePath from a previous run. It is not an error for the
// store to not exist yet — Token() will fail until ExchangeCode is called
// once (via the /oauth/authorize -> /oauth/callback flow).
func NewOAuthTokenSource(clientID, clientSecret, storePath string) (*OAuthTokenSource, error) {
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("SMARTTHINGS_CLIENT_ID and SMARTTHINGS_CLIENT_SECRET are both required for OAuth")
	}
	ts := &OAuthTokenSource{
		clientID:     clientID,
		clientSecret: clientSecret,
		storePath:    storePath,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}
	if err := ts.load(); err != nil {
		return nil, fmt.Errorf("failed to load SmartThings OAuth token store %q: %w", storePath, err)
	}
	return ts, nil
}

// HasRefreshToken reports whether a refresh token is on file, i.e. whether
// the one-time browser consent has already been completed.
func (ts *OAuthTokenSource) HasRefreshToken() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.tokens.RefreshToken != ""
}

// AuthorizeURL builds the SmartThings consent URL to send a browser to in
// order to start the authorization_code flow.
func (ts *OAuthTokenSource) AuthorizeURL(redirectURI, state string) string {
	v := url.Values{
		"client_id":     {ts.clientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {strings.Join(DefaultScopes, " ")},
		"state":         {state},
	}
	return oauthAuthorizeURL + "?" + v.Encode()
}

// ExchangeCode completes the one-time authorization_code flow: it swaps
// the code SmartThings redirected back with for an access/refresh token
// pair and persists them.
func (ts *OAuthTokenSource) ExchangeCode(code, redirectURI string) error {
	return ts.requestToken(url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	})
}

// Token implements TokenSource: it returns the current access token,
// transparently refreshing first if it's missing or close to expiry.
func (ts *OAuthTokenSource) Token() (string, error) {
	ts.mu.RLock()
	access := ts.tokens.AccessToken
	expires := ts.tokens.ExpiresAt
	ts.mu.RUnlock()

	if access != "" && time.Until(expires) > refreshMargin {
		return access, nil
	}
	if err := ts.refresh(); err != nil {
		return "", err
	}
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.tokens.AccessToken, nil
}

// refresh exchanges the current refresh token for a new access/refresh
// pair. SmartThings rotates the refresh token on every use, so the new one
// is always persisted immediately.
func (ts *OAuthTokenSource) refresh() error {
	ts.mu.RLock()
	refreshToken := ts.tokens.RefreshToken
	ts.mu.RUnlock()
	if refreshToken == "" {
		return fmt.Errorf("no SmartThings OAuth authorization on file yet; visit /oauth/authorize once to grant access")
	}
	return ts.requestToken(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (ts *OAuthTokenSource) requestToken(form url.Values) error {
	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(ts.clientID, ts.clientSecret)

	res, err := ts.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("SmartThings OAuth token request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("SmartThings OAuth token request failed: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}

	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return fmt.Errorf("failed to decode SmartThings OAuth token response: %w", err)
	}
	if resp.AccessToken == "" {
		return fmt.Errorf("SmartThings OAuth token response had no access_token")
	}

	ts.mu.Lock()
	ts.tokens.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		ts.tokens.RefreshToken = resp.RefreshToken
	}
	ts.tokens.ExpiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	ts.mu.Unlock()

	return ts.save()
}

// RunBackgroundRefresh refreshes the access token proactively, well before
// it expires, so foreground Token() calls rarely need to block on a live
// HTTP round trip. It blocks until ctx is cancelled, so call it in a
// goroutine. onError (may be nil) is called with any refresh failure; the
// loop keeps retrying on its normal schedule regardless.
func (ts *OAuthTokenSource) RunBackgroundRefresh(ctx context.Context, onError func(error)) {
	for {
		ts.mu.RLock()
		expires := ts.tokens.ExpiresAt
		hasRefresh := ts.tokens.RefreshToken != ""
		ts.mu.RUnlock()

		wait := time.Minute
		if hasRefresh {
			// Refresh well ahead of expiry (SmartThings access tokens are
			// short-lived, e.g. 24h) rather than cutting it close.
			if until := time.Until(expires) - 30*time.Minute; until > wait {
				wait = until
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		ts.mu.RLock()
		hasRefresh = ts.tokens.RefreshToken != ""
		ts.mu.RUnlock()
		if !hasRefresh {
			continue // still waiting on the initial /oauth/authorize consent
		}
		if err := ts.refresh(); err != nil && onError != nil {
			onError(err)
		}
	}
}

func (ts *OAuthTokenSource) load() error {
	data, err := os.ReadFile(ts.storePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st storedTokens
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	ts.mu.Lock()
	ts.tokens = st
	ts.mu.Unlock()
	return nil
}

func (ts *OAuthTokenSource) save() error {
	ts.mu.RLock()
	st := ts.tokens
	ts.mu.RUnlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(ts.storePath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("failed to create SmartThings OAuth token store directory: %w", err)
		}
	}
	// 0600: refresh_token is a long-lived credential.
	return os.WriteFile(ts.storePath, data, 0o600)
}
