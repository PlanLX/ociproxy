package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// authTransport adds Bearer authentication to upstream requests.
// It implements Docker Registry token authentication flow:
// 1. Request without auth -> gets 401 with Www-Authenticate header
// 2. Parse the realm, service, and scope from the header
// 3. Fetch a token from the realm endpoint (with basic auth)
// 4. Retry the original request with the Bearer token
type authTransport struct {
	base       http.RoundTripper
	username   string
	password   string
	hostURL    string
	tokenCache map[string]*tokenEntry
	mu         sync.Mutex
}

type tokenEntry struct {
	token     string
	expiresAt time.Time
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := a.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// If not 401, return as-is
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// Parse Www-Authenticate header
	authHeader := resp.Header.Get("Www-Authenticate")
	if authHeader == "" {
		return resp, nil
	}

	// Drain and close the 401 response body
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	challenge := parseAuthHeader(authHeader)
	if challenge.realm == "" {
		return resp, nil
	}

	// Get token
	token, err := a.getToken(challenge)
	if err != nil {
		return nil, fmt.Errorf("get auth token: %w", err)
	}

	// Retry with token
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+token)

	return a.base.RoundTrip(req2)
}

func (a *authTransport) getToken(challenge authChallenge) (string, error) {
	scope := challenge.scope
	if scope == "" {
		scope = "repository:*:*"
	}

	a.mu.Lock()
	if entry, ok := a.tokenCache[scope]; ok && time.Now().Before(entry.expiresAt) {
		token := entry.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()

	// Build token request URL
	tokenURL := challenge.realm
	u, err := url.Parse(tokenURL)
	if err != nil {
		return "", err
	}

	q := u.Query()
	if challenge.service != "" {
		q.Set("service", challenge.service)
	}
	if challenge.scope != "" {
		q.Set("scope", challenge.scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if a.username != "" || a.password != "" {
		req.SetBasicAuth(a.username, a.password)
	}

	resp, err := a.base.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token request failed: %s: %s", resp.Status, body)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	token := tokenResp.Token
	if token == "" {
		token = tokenResp.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("no token in response")
	}

	expires := 5 * time.Minute
	if tokenResp.ExpiresIn > 0 {
		expires = time.Duration(tokenResp.ExpiresIn-30) * time.Second
	}

	a.mu.Lock()
	a.tokenCache[scope] = &tokenEntry{
		token:     token,
		expiresAt: time.Now().Add(expires),
	}
	a.mu.Unlock()

	return token, nil
}

type authChallenge struct {
	realm   string
	service string
	scope   string
}

func parseAuthHeader(header string) authChallenge {
	// Expected: Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/busybox:pull"
	if !strings.HasPrefix(header, "Bearer ") {
		return authChallenge{}
	}

	params := header[len("Bearer "):]
	var ch authChallenge

	// Parse key="value" pairs
	for _, part := range splitParams(params) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(kv[1], "\"")
		switch key {
		case "realm":
			ch.realm = val
		case "service":
			ch.service = val
		case "scope":
			ch.scope = val
		}
	}

	return ch
}

func splitParams(s string) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			result = append(result, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, strings.TrimSpace(s[start:]))
	}
	return result
}
