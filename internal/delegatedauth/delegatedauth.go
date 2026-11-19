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

	"github.com/element-hq/dendrite/setup/config"
)

const (
	metadataTTL          = time.Hour
	introspectionTTL     = time.Minute
	maxCacheEntries      = 10000
	requestTimeout       = 10 * time.Second
	maxResponseBytes     = 1 << 20
	stableScopePrefix    = "urn:matrix:client:"
	unstableScopePrefix  = "urn:matrix:org.matrix.msc2967.client:"
	apiScopeSuffix       = "api:*"
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
}

type introspectionResponse struct {
	Active   bool   `json:"active"`
	Scope    string `json:"scope"`
	Username string `json:"username"`
	Expiry   int64  `json:"exp"`
}

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

	mu    sync.Mutex
	cache map[string]cacheEntry
}

func New(cfg *config.MSC3861) *Provider {
	return &Provider{
		cfg:    cfg,
		client: &http.Client{Timeout: requestTimeout},
		cache:  map[string]cacheEntry{},
	}
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

func (p *Provider) loadMetadata(ctx context.Context) (json.RawMessage, string, error) {
	p.mdMu.Lock()
	defer p.mdMu.Unlock()
	if p.metadata != nil && time.Since(p.metadataFetched) < metadataTTL {
		return p.metadata, p.introspectionEP, nil
	}

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
		p.metadata = raw
		p.introspectionEP = parsed.IntrospectionEndpoint
		p.metadataFetched = time.Now()
		return p.metadata, p.introspectionEP, nil
	}
	if p.metadata != nil {
		// Keep serving stale metadata rather than failing every request while the provider is down.
		return p.metadata, p.introspectionEP, nil
	}
	return nil, "", lastErr
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
	p.mu.Lock()
	if e, ok := p.cache[key]; ok {
		if time.Now().Before(e.expires) {
			p.mu.Unlock()
			return e.token, nil
		}
		delete(p.cache, key)
	}
	p.mu.Unlock()

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
	if p.cfg.ClientAuthMethod == config.ClientAuthMethodPost {
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.cfg.ClientAuthMethod != config.ClientAuthMethodPost {
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
		return nil, nil
	}

	token := parseScope(ir.Scope)
	token.Username = ir.Username

	expires := time.Now().Add(introspectionTTL)
	if ir.Expiry > 0 {
		if exp := time.Unix(ir.Expiry, 0); exp.Before(expires) {
			expires = exp
		}
	}
	p.mu.Lock()
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
	p.mu.Unlock()
	return token, nil
}

func parseScope(scope string) *Token {
	t := &Token{}
	devices := 0
	for _, s := range strings.Fields(scope) {
		for _, prefix := range []string{stableScopePrefix, unstableScopePrefix} {
			rest, ok := strings.CutPrefix(s, prefix)
			if !ok {
				continue
			}
			if rest == apiScopeSuffix {
				t.HasAPIScope = true
			} else if id, ok := strings.CutPrefix(rest, deviceScopeSuffix); ok {
				devices++
				t.DeviceID = id
			}
		}
	}
	if devices != 1 || len(t.DeviceID) > maxDeviceIDLength {
		t.DeviceID = ""
	}
	return t
}

func cacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
