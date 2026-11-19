package delegatedauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/element-hq/dendrite/setup/config"
)

type fakeProvider struct {
	*httptest.Server
	introspections atomic.Int32
	response       string
	gotAuth        string
	gotMAS         string
	gotForm        map[string]string
}

func newFakeProvider(t *testing.T, wellKnown string) *fakeProvider {
	f := &fakeProvider{response: `{"active": false}`}
	mux := http.NewServeMux()
	mux.HandleFunc(wellKnown, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.URL + "/",
			"introspection_endpoint": f.URL + "/introspect",
			"token_endpoint":         f.URL + "/token",
		})
	})
	mux.HandleFunc("/introspect", func(w http.ResponseWriter, r *http.Request) {
		f.introspections.Add(1)
		_ = r.ParseForm()
		f.gotAuth = r.Header.Get("Authorization")
		f.gotMAS = r.Header.Get("X-MAS-Supports-Device-Id")
		f.gotForm = map[string]string{}
		for k := range r.PostForm {
			f.gotForm[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write([]byte(f.response))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) config() *config.MSC3861 {
	return &config.MSC3861{Enabled: true, Issuer: f.URL + "/", ClientID: "dendrite", ClientSecret: "s3cret"}
}

func TestParseScope(t *testing.T) {
	tests := []struct {
		scope string
		want  Token
	}{
		{"urn:matrix:client:api:* urn:matrix:client:device:ABCDEF", Token{DeviceID: "ABCDEF", HasAPIScope: true}},
		{"urn:matrix:org.matrix.msc2967.client:api:* urn:matrix:org.matrix.msc2967.client:device:ABCDEF", Token{DeviceID: "ABCDEF", HasAPIScope: true}},
		{"urn:matrix:client:device:ABCDEF", Token{DeviceID: "ABCDEF"}},
		{"urn:matrix:client:api:*", Token{HasAPIScope: true}},
		{"urn:matrix:client:api:* urn:matrix:client:device:A urn:matrix:client:device:B", Token{HasAPIScope: true}},
		{"openid email urn:matrix:client:api:*", Token{HasAPIScope: true}},
		{"", Token{}},
	}
	for _, tc := range tests {
		if got := *parseScope(tc.scope); got != tc.want {
			t.Errorf("parseScope(%q) = %+v, want %+v", tc.scope, got, tc.want)
		}
	}
}

func TestIntrospect(t *testing.T) {
	for _, wellKnown := range []string{wellKnownOIDC, wellKnownOAuthServer} {
		t.Run(wellKnown, func(t *testing.T) {
			f := newFakeProvider(t, wellKnown)
			p := New(f.config())
			ctx := context.Background()

			tok, err := p.Introspect(ctx, "inactive")
			if err != nil || tok != nil {
				t.Fatalf("inactive token: got (%v, %v), want (nil, nil)", tok, err)
			}
			if want := "Basic ZGVuZHJpdGU6czNjcmV0"; f.gotAuth != want {
				t.Errorf("Authorization = %q, want %q", f.gotAuth, want)
			}
			if f.gotForm["token"] != "inactive" {
				t.Errorf("token form value = %q", f.gotForm["token"])
			}

			f.response = `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:* urn:matrix:client:device:ABCDEF"}`
			before := f.introspections.Load()
			for i := 0; i < 3; i++ {
				tok, err = p.Introspect(ctx, "good")
				if err != nil {
					t.Fatal(err)
				}
				if tok == nil || tok.Username != "alice" || tok.DeviceID != "ABCDEF" || !tok.HasAPIScope {
					t.Fatalf("unexpected token %+v", tok)
				}
			}
			if n := f.introspections.Load() - before; n != 1 {
				t.Errorf("introspection endpoint called %d times for 3 lookups, want 1 (cached)", n)
			}
		})
	}
}

func TestIntrospectClientSecretPost(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	cfg := f.config()
	cfg.ClientAuthMethod = config.ClientAuthMethodPost
	p := New(cfg)
	if _, err := p.Introspect(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	if f.gotAuth != "" || f.gotForm["client_id"] != "dendrite" || f.gotForm["client_secret"] != "s3cret" {
		t.Errorf("unexpected client authentication: header %q, form %v", f.gotAuth, f.gotForm)
	}
}

func TestIntrospectExpiredIsNotCached(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	f.response = `{"active": true, "username": "alice", "exp": 1}`
	p := New(f.config())
	for i := 0; i < 2; i++ {
		if _, err := p.Introspect(context.Background(), "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.introspections.Load(); n != 2 {
		t.Errorf("introspection endpoint called %d times, want 2", n)
	}
}

func TestIntrospectProviderDown(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	cfg := f.config()
	f.Close()
	tok, err := New(cfg).Introspect(context.Background(), "tok")
	if err == nil || tok != nil {
		t.Errorf("got (%v, %v), want an error when the provider is unreachable", tok, err)
	}
}

func TestMetadataIssuerMismatch(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	cfg := f.config()
	cfg.Issuer = f.URL + "/other"
	if _, err := New(cfg).Metadata(context.Background()); err == nil {
		t.Error("expected an error for metadata of a different issuer")
	}
}

func TestMetadata(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	md, err := New(f.config()).Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]string
	if err = json.Unmarshal(md, &parsed); err != nil || parsed["token_endpoint"] != f.URL+"/token" {
		t.Errorf("unexpected metadata %s (%v)", md, err)
	}
}

func TestParseScopeSameDeviceUnderBothPrefixes(t *testing.T) {
	got := parseScope("urn:matrix:client:api:* urn:matrix:client:device:ABC urn:matrix:org.matrix.msc2967.client:device:ABC")
	if got.DeviceID != "ABC" || !got.HasAPIScope {
		t.Errorf("got %+v, want device ABC with API scope", got)
	}
}

func TestInactiveTokensAreCached(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	p := New(f.config())
	for i := 0; i < 5; i++ {
		if tok, err := p.Introspect(context.Background(), "dead"); err != nil || tok != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", tok, err)
		}
	}
	if n := f.introspections.Load(); n != 1 {
		t.Errorf("introspection endpoint called %d times, want 1", n)
	}
}

func TestConcurrentIntrospectionsAreCoalesced(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"active": true, "username": "alice"}`))
	}))
	defer srv.Close()
	p := New(&config.MSC3861{Enabled: true, Issuer: srv.URL, ClientID: "c", ClientSecret: "s", IntrospectionEndpoint: srv.URL})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := p.Introspect(context.Background(), "same"); err != nil || tok == nil {
				t.Errorf("got (%v, %v)", tok, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("provider called %d times for 20 concurrent lookups, want 1", n)
	}
}

func TestMetadataFailuresBackOff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := New(&config.MSC3861{Enabled: true, Issuer: srv.URL, ClientID: "c", ClientSecret: "s"})
	for i := 0; i < 5; i++ {
		if _, err := p.Metadata(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
	}
	// One attempt tries both well-known locations.
	if n := calls.Load(); n != 2 {
		t.Errorf("provider called %d times, want 2 (a single attempt)", n)
	}
}

func TestStaleMetadataIsServedWhileProviderIsDown(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	p := New(f.config())
	if _, err := p.Metadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	p.mdMu.Lock()
	p.metadataFetched = time.Now().Add(-2 * metadataTTL)
	p.metadataAttempt = time.Now().Add(-2 * metadataTTL)
	p.mdMu.Unlock()

	start := time.Now()
	md, err := p.Metadata(context.Background())
	if err != nil || md == nil {
		t.Fatalf("got (%s, %v), want the stale metadata", md, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("serving stale metadata took %v, it must not wait for the provider", d)
	}
}

func TestForConfigIsShared(t *testing.T) {
	cfg := &config.MSC3861{Enabled: true, Issuer: "https://auth.example.com/"}
	if ForConfig(cfg) != ForConfig(cfg) {
		t.Error("expected the same provider for the same config")
	}
	if ForConfig(cfg) == ForConfig(&config.MSC3861{}) {
		t.Error("expected different providers for different configs")
	}
}

func TestIntrospectMASMode(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	f.response = `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:* urn:synapse:admin:*", "device_id": "COMPAT1"}`
	p := New(&config.MSC3861{Enabled: true, Issuer: f.URL + "/", SharedSecret: "shared"})
	tok, err := p.Introspect(context.Background(), "tok")
	if err != nil || tok == nil {
		t.Fatalf("got (%v, %v)", tok, err)
	}
	if f.gotAuth != "Bearer shared" || f.gotMAS != "1" {
		t.Errorf("auth %q, X-MAS-Supports-Device-Id %q, want the shared secret as bearer token and the header", f.gotAuth, f.gotMAS)
	}
	if _, ok := f.gotForm["client_secret"]; ok {
		t.Error("client credentials must not be sent in MAS mode")
	}
	if tok.DeviceID != "COMPAT1" || !tok.IsAdmin || !tok.HasAPIScope {
		t.Errorf("unexpected token %+v", tok)
	}
	if !p.MASMode() {
		t.Error("expected MAS mode")
	}
}

func TestInvalidate(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	f.response = `{"active": true, "username": "alice"}`
	p := New(f.config())
	for i := 0; i < 2; i++ {
		if _, err := p.Introspect(context.Background(), "tok"); err != nil {
			t.Fatal(err)
		}
		p.Invalidate("tok")
	}
	if n := f.introspections.Load(); n != 2 {
		t.Errorf("introspection endpoint called %d times, want 2 after invalidating", n)
	}
}

func TestAdminScopeOnlyCountsInMASMode(t *testing.T) {
	f := newFakeProvider(t, wellKnownOIDC)
	f.response = `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:* urn:synapse:admin:*", "device_id": "D"}`
	tok, err := New(f.config()).Introspect(context.Background(), "tok")
	if err != nil || tok == nil || tok.IsAdmin {
		t.Errorf("a generic provider must not make admins: got (%+v, %v)", tok, err)
	}
}
