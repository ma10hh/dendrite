package delegatedauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/element-hq/dendrite/setup/config"
)

type fakeProvider struct {
	*httptest.Server
	introspections atomic.Int32
	response       string
	gotAuth        string
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
