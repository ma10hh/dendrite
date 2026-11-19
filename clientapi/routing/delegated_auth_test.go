package routing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/element-hq/dendrite/setup/config"
	"github.com/matrix-org/util"
)

func TestDelegatedAuthDisabledEndpoints(t *testing.T) {
	tests := []struct {
		method, path string
		disabled     bool
	}{
		{http.MethodGet, "/_matrix/client/v3/login", true},
		{http.MethodPost, "/_matrix/client/r0/login", true},
		{http.MethodGet, "/_matrix/client/v3/login/sso/redirect", true},
		{http.MethodPost, "/_matrix/client/v3/register", true},
		{http.MethodPost, "/_matrix/client/v3/register/email/requestToken", true},
		{http.MethodPost, "/_matrix/client/v3/logout", true},
		{http.MethodPost, "/_matrix/client/v3/logout/all", true},
		{http.MethodPost, "/_matrix/client/v3/account/password", true},
		{http.MethodPost, "/_matrix/client/v3/account/deactivate", true},
		{http.MethodPost, "/_matrix/client/v3/account/3pid", true},
		{http.MethodPost, "/_matrix/client/v3/account/3pid/delete", true},
		{http.MethodGet, "/_matrix/client/v3/account/3pid", false},
		{http.MethodGet, "/_matrix/client/v3/account/whoami", false},
		{http.MethodPost, "/_matrix/client/v3/delete_devices", true},
		{http.MethodDelete, "/_matrix/client/v3/devices/ABC", true},
		{http.MethodGet, "/_matrix/client/v3/devices/ABC", false},
		{http.MethodPut, "/_matrix/client/v3/devices/ABC", false},
		{http.MethodGet, "/_matrix/client/v3/devices", false},
		{http.MethodGet, "/_matrix/client/v3/auth/m.login.recaptcha/fallback/web", true},
		{http.MethodOptions, "/_matrix/client/v3/login", false},
		{http.MethodPost, "/_matrix/client/v3/createRoom", false},
		{http.MethodGet, "/_matrix/client/v3/capabilities", false},
	}
	for _, tc := range tests {
		if got := isDelegatedAuthDisabledEndpoint(tc.method, tc.path); got != tc.disabled {
			t.Errorf("%s %s: disabled = %v, want %v", tc.method, tc.path, got, tc.disabled)
		}
	}
}

func TestDelegatedAuthMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := delegatedAuthMiddleware(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "M_UNRECOGNIZED") {
		t.Errorf("login: got %d %s, want 404 M_UNRECOGNIZED", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/account/whoami", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("whoami: got %d, want the request passed through", rec.Code)
	}
}

func TestAuthMetadataDisabled(t *testing.T) {
	for _, h := range []http.Handler{authMetadata(nil), authIssuer(nil)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/auth_metadata", nil))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "M_UNRECOGNIZED") {
			t.Errorf("got %d %s, want 404 M_UNRECOGNIZED", rec.Code, rec.Body)
		}
	}
}

func TestCrossSigningResetApproval(t *testing.T) {
	if crossSigningResetAllowed("@approval-test:example.com") {
		t.Fatal("nothing was approved yet")
	}
	allowCrossSigningReset("@approval-test:example.com")
	if !crossSigningResetAllowed("@approval-test:example.com") {
		t.Error("the approval was not recorded")
	}
	if crossSigningResetAllowed("@someone-else:example.com") {
		t.Error("the approval must only apply to the approved user")
	}
	crossSigningResets.Lock()
	crossSigningResets.until["@approval-test:example.com"] = time.Now().Add(-time.Second)
	crossSigningResets.Unlock()
	if crossSigningResetAllowed("@approval-test:example.com") {
		t.Error("the approval must expire")
	}
}

func TestMASSharedSecretIsChecked(t *testing.T) {
	m := &masAPI{cfg: &config.ClientAPI{MSC3861: config.MSC3861{Enabled: true, SharedSecret: "secret"}}}
	h := m.fromMAS("test", func(*http.Request) util.JSONResponse { return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}} })
	for _, tc := range []struct {
		header string
		want   int
	}{
		{"", http.StatusForbidden},
		{"secret", http.StatusForbidden},
		{"Bearer wrong", http.StatusForbidden},
		{"Bearer secret ", http.StatusForbidden},
		{"Bearer secret", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/_synapse/mas/query_user", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("Authorization %q: got %d, want %d", tc.header, rec.Code, tc.want)
		}
	}
	// An unset secret must never match, not even an empty bearer token.
	m.cfg.MSC3861.SharedSecret = ""
	req := httptest.NewRequest(http.MethodGet, "/_synapse/mas/query_user", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty secret accepted: got %d", rec.Code)
	}
}
