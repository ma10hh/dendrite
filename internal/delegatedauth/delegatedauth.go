// Package delegatedauth implements the resource server side of MSC3861: access
// tokens are issued by an external OAuth 2.0 / OpenID Connect provider and
// validated through token introspection (RFC 7662).
package delegatedauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/element-hq/dendrite/setup/config"
)

const (
	metadataTTL           = time.Hour
	metadataRetryInterval = 30 * time.Second
	introspectionTTL      = time.Minute
	// Inactive tokens are cached briefly so that a client retrying with a dead
	// token, or a flood of random tokens, does not become a flood of requests
	// to the provider.
	inactiveTokenTTL     = 10 * time.Second
	maxCacheEntries      = 10000
	requestTimeout       = 10 * time.Second
	maxResponseBytes     = 1 << 20
	stableScopePrefix    = "urn:matrix:client:"
	unstableScopePrefix  = "urn:matrix:org.matrix.msc2967.client:"
	apiScopeSuffix       = "api:*"
	adminScope           = "urn:synapse:admin:*"
	deviceScopeSuffix    = "device:"
	maxDeviceIDLength    = 255
	wellKnownOIDC        = "/.well-known/openid-configuration"
	wellKnownOAuthServer = "/.well-known/oauth-authorization-server"
)

// Token is the result of a successful introspection of an active access token.
type Token struct {
	// Username is the localpart (or full user ID) of the Matrix user the token belongs to.
	Username string
	// DeviceID is the device requested through the urn:matrix:client:device:<id> scope.
	// Empty if the token has no (or more than one) device scope.
	DeviceID string
	// HasAPIScope is true if the token grants full client-server API access.
	HasAPIScope bool
	// IsAdmin is true if the token carries the urn:synapse:admin:* scope used by MAS for server admins.
	IsAdmin bool
}

type introspectionResponse struct {
	Active   bool   `json:"active"`
	Scope    string `json:"scope"`
	Username string `json:"username"`
	Expiry   int64  `json:"exp"`
	// MAS sends the device ID explicitly for compatibility sessions, which have no device scope.
	DeviceID string `json:"device_id"`
}

// cacheEntry holds the outcome of an introspection. A nil token means "inactive".
type cacheEntry struct {
	token   *Token
	expires time.Time
}

type Provider struct {
	cfg    *config.MSC3861
	client *http.Client

	mdMu            sync.Mutex
	metadata        json.RawMessage
	introspectionEP string
	metadataFetched time.Time
	metadataAttempt time.Time
	metadataErr     error
	mdFlight        singleflight.Group

	mu     sync.Mutex
	cache  map[string]cacheEntry
	flight singleflight.Group
}

var (
	providersMu sync.Mutex
	providers   = map[*config.MSC3861]*Provider{}
)

// ForConfig returns the provider shared by everything using the given config, so that
// the metadata and token caches exist only once per process.
func ForConfig(cfg *config.MSC3861) *Provider {
	providersMu.Lock()
	defer providersMu.Unlock()
	p, ok := providers[cfg]
	if !ok {
		p = New(cfg)
		providers[cfg] = p
	}
	return p
}

func New(cfg *config.MSC3861) *Provider {
	return &Provider{
		cfg:    cfg,
		client: &http.Client{Timeout: requestTimeout},
		cache:  map[string]cacheEntry{},
	}
}

// MASMode is true if the provider is Matrix Authentication Service, which manages accounts and devices itself.
func (p *Provider) MASMode() bool {
	return p.cfg.MASMode()
}

// Issuer returns the configured issuer identifier.
func (p *Provider) Issuer() string {
	return p.cfg.Issuer
}

// Metadata returns the authorization server metadata document of the provider (RFC 8414).
func (p *Provider) Metadata(ctx context.Context) (json.RawMessage, error) {
	md, _, err := p.loadMetadata(ctx)
	return md, err
}

// loadMetadata returns the cached provider metadata. Stale metadata is served immediately
// while it is refreshed in the background, and failed refreshes are not retried more
// often than metadataRetryInterval, so an unreachable provider cannot stall requests.
func (p *Provider) loadMetadata(ctx context.Context) (json.RawMessage, string, error) {
	p.mdMu.Lock()
	md, endpoint, lastErr := p.metadata, p.introspectionEP, p.metadataErr
	fresh := md != nil && time.Since(p.metadataFetched) < metadataTTL
	backingOff := time.Since(p.metadataAttempt) < metadataRetryInterval
	p.mdMu.Unlock()

	if fresh {
		return md, endpoint, nil
	}
	detached := context.WithoutCancel(ctx)
	if md != nil {
		if !backingOff {
			p.mdFlight.DoChan("metadata", func() (any, error) { return nil, p.fetchMetadata(detached) })
		}
		return md, endpoint, nil
	}
	if backingOff && lastErr != nil {
		return nil, "", lastErr
	}
	if _, err, _ := p.mdFlight.Do("metadata", func() (any, error) { return nil, p.fetchMetadata(detached) }); err != nil {
		return nil, "", err
	}
	p.mdMu.Lock()
	defer p.mdMu.Unlock()
	return p.metadata, p.introspectionEP, nil
}

func (p *Provider) fetchMetadata(ctx context.Context) error {
	issuer := strings.TrimRight(p.cfg.Issuer, "/")
	var lastErr error
	for _, suffix := range []string{wellKnownOIDC, wellKnownOAuthServer} {
		raw, err := p.get(ctx, issuer+suffix)
		if err != nil {
			lastErr = err
			continue
		}
		var parsed struct {
			Issuer                string `json:"issuer"`
			IntrospectionEndpoint string `json:"introspection_endpoint"`
		}
		if err = json.Unmarshal(raw, &parsed); err != nil {
			lastErr = fmt.Errorf("invalid metadata from %s: %w", issuer+suffix, err)
			continue
		}
		if strings.TrimRight(parsed.Issuer, "/") != issuer {
			lastErr = fmt.Errorf("metadata from %s has issuer %q, expected %q", issuer+suffix, parsed.Issuer, p.cfg.Issuer)
			continue
		}
		p.mdMu.Lock()
		p.metadata = raw
		p.introspectionEP = parsed.IntrospectionEndpoint
		p.metadataFetched = time.Now()
		p.metadataAttempt = p.metadataFetched
		p.metadataErr = nil
		p.mdMu.Unlock()
		return nil
	}
	p.mdMu.Lock()
	p.metadataAttempt = time.Now()
	p.metadataErr = lastErr
	p.mdMu.Unlock()
	return lastErr
}

func (p *Provider) get(ctx context.Context, u string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// Introspect validates an access token with the provider. It returns (nil, nil) if
// the token is not active. An error means the provider could not be asked, which must
// not be treated as the token being invalid.
func (p *Provider) Introspect(ctx context.Context, accessToken string) (*Token, error) {
	key := cacheKey(accessToken)
	if token, ok := p.cached(key); ok {
		return token, nil
	}
	// Concurrent lookups of the same token share one request to the provider.
	detached := context.WithoutCancel(ctx)
	v, err, _ := p.flight.Do(key, func() (any, error) {
		return p.introspectRemote(detached, accessToken, key)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Token), nil
}

// Invalidate forgets what is known about a token, for example because its device was deleted.
func (p *Provider) Invalidate(accessToken string) {
	p.mu.Lock()
	delete(p.cache, cacheKey(accessToken))
	p.mu.Unlock()
}

func (p *Provider) cached(key string) (*Token, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(p.cache, key)
		return nil, false
	}
	return e.token, true
}

func (p *Provider) store(key string, token *Token, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cache) >= maxCacheEntries {
		now := time.Now()
		for k, e := range p.cache {
			if now.After(e.expires) {
				delete(p.cache, k)
			}
		}
		if len(p.cache) >= maxCacheEntries {
			p.cache = map[string]cacheEntry{}
		}
	}
	p.cache[key] = cacheEntry{token: token, expires: expires}
}

func (p *Provider) introspectRemote(ctx context.Context, accessToken, key string) (*Token, error) {
	endpoint := p.cfg.IntrospectionEndpoint
	if endpoint == "" {
		var err error
		if _, endpoint, err = p.loadMetadata(ctx); err != nil {
			return nil, fmt.Errorf("fetching provider metadata: %w", err)
		}
		if endpoint == "" {
			return nil, errors.New("provider metadata has no introspection_endpoint and none is configured")
		}
	}

	form := url.Values{"token": {accessToken}, "token_type_hint": {"access_token"}}
	if p.cfg.SharedSecret == "" && p.cfg.ClientAuthMethod == config.ClientAuthMethodPost {
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	switch {
	case p.cfg.SharedSecret != "":
		req.Header.Set("Authorization", "Bearer "+p.cfg.SharedSecret)
		req.Header.Set("X-MAS-Supports-Device-Id", "1")
	case p.cfg.ClientAuthMethod != config.ClientAuthMethodPost:
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token introspection failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token introspection returned HTTP %d", resp.StatusCode)
	}
	var ir introspectionResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&ir); err != nil {
		return nil, fmt.Errorf("invalid token introspection response: %w", err)
	}
	if !ir.Active {
		p.store(key, nil, time.Now().Add(inactiveTokenTTL))
		return nil, nil
	}

	token := parseScope(ir.Scope)
	token.Username = ir.Username
	if ir.DeviceID != "" {
		token.DeviceID = ir.DeviceID
		if len(ir.DeviceID) > maxDeviceIDLength {
			token.DeviceID = ""
		}
	}

	expires := time.Now().Add(introspectionTTL)
	if ir.Expiry > 0 {
		if exp := time.Unix(ir.Expiry, 0); exp.Before(expires) {
			expires = exp
		}
	}
	p.store(key, token, expires)
	return token, nil
}

func parseScope(scope string) *Token {
	t := &Token{}
	devices := map[string]struct{}{}
	for _, s := range strings.Fields(scope) {
		for _, prefix := range []string{stableScopePrefix, unstableScopePrefix} {
			rest, ok := strings.CutPrefix(s, prefix)
			if !ok {
				continue
			}
			if rest == apiScopeSuffix {
				t.HasAPIScope = true
			} else if id, ok := strings.CutPrefix(rest, deviceScopeSuffix); ok {
				devices[id] = struct{}{}
				t.DeviceID = id
			}
		}
		if s == adminScope {
			t.IsAdmin = true
		}
	}
	// The same device may legitimately be listed under both the stable and unstable prefix.
	if len(devices) != 1 || len(t.DeviceID) > maxDeviceIDLength {
		t.DeviceID = ""
	}
	return t
}

func cacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
