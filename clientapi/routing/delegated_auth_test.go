package routing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
