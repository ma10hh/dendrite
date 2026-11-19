package routing

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/element-hq/dendrite/internal/delegatedauth"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// What an appservice may still do with an endpoint that is disabled for everybody else.
type appserviceAccess int

const (
	// The endpoint is not available.
	appserviceDenied appserviceAccess = iota
	// The endpoint works as usual. There is no user interactive authentication for appservices.
	appserviceAllowed
	// Only registering users without logging them in works (MSC4190).
	appserviceRegisterOnly
	// Logging in is refused with M_APPSERVICE_LOGIN_UNSUPPORTED (MSC4190).
	appserviceLoginRefused
)

// Endpoints which only make sense when Dendrite handles authentication itself. With
// MSC3861 they are the responsibility of the external provider and answer M_UNRECOGNIZED.
var delegatedAuthDisabledEndpoints = []struct {
	methods     map[string]bool
	path        *regexp.Regexp
	appservices appserviceAccess
}{
	{map[string]bool{http.MethodPost: true}, regexp.MustCompile(`^/login$`), appserviceLoginRefused},
	{nil, regexp.MustCompile(`^/login(/.*)?$`), appserviceDenied},
	{map[string]bool{http.MethodPost: true}, regexp.MustCompile(`^/register$`), appserviceRegisterOnly},
	{nil, regexp.MustCompile(`^/register(/.*)?$`), appserviceDenied},
	{nil, regexp.MustCompile(`^/logout(/all)?$`), appserviceDenied},
	{nil, regexp.MustCompile(`^/account/(password|deactivate)(/.*)?$`), appserviceDenied},
	{nil, regexp.MustCompile(`^/account/3pid/.+$`), appserviceDenied},
	{map[string]bool{http.MethodPost: true}, regexp.MustCompile(`^/account/3pid$`), appserviceDenied},
	{nil, regexp.MustCompile(`^/delete_devices$`), appserviceAllowed},
	{map[string]bool{http.MethodDelete: true}, regexp.MustCompile(`^/devices/[^/]+$`), appserviceAllowed},
	{nil, regexp.MustCompile(`^/auth/[^/]+/fallback/web$`), appserviceDenied},
}

var versionedPathRegexp = regexp.MustCompile(`^/_matrix/client/(?:r0|v3)(/.*)$`)

const errAppserviceLoginUnsupported spec.MatrixErrorCode = "M_APPSERVICE_LOGIN_UNSUPPORTED"

// delegatedAuthEndpoint tells whether an endpoint is disabled while authentication is delegated,
// and what appservices may still do with it.
func delegatedAuthEndpoint(method, path string) (disabled bool, appservices appserviceAccess) {
	m := versionedPathRegexp.FindStringSubmatch(path)
	if m == nil {
		return false, appserviceDenied
	}
	for _, e := range delegatedAuthDisabledEndpoints {
		if e.path.MatchString(m[1]) && (e.methods == nil || e.methods[method]) {
			// Let CORS preflights through; the browser needs them to read the error.
			return method != http.MethodOptions, e.appservices
		}
	}
	return false, appserviceDenied
}

func isDelegatedAuthDisabledEndpoint(method, path string) bool {
	disabled, _ := delegatedAuthEndpoint(method, path)
	return disabled
}

// isAppserviceRequest is true if the request is made with the token of a registered appservice.
func isAppserviceRequest(req *http.Request, cfg *config.ClientAPI) bool {
	if cfg == nil || cfg.Derived == nil {
		return false
	}
	token := req.URL.Query().Get("access_token")
	if bearer, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok {
		token = bearer
	}
	if token == "" {
		return false
	}
	for _, as := range cfg.Derived.ApplicationServices {
		if as.ASToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(as.ASToken)) == 1 {
			return true
		}
	}
	return false
}

// registersWithoutLogin checks that a registration is the one an appservice may still make: for a
// user, without creating a device and an access token.
func registersWithoutLogin(req *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	var r struct {
		Type         string `json:"type"`
		InhibitLogin *bool  `json:"inhibit_login"`
	}
	return json.Unmarshal(body, &r) == nil && r.Type == "m.login.application_service" && r.InhibitLogin != nil && *r.InhibitLogin
}

// delegatedAuthMiddleware answers requests for the endpoints disabled by MSC3861.
func delegatedAuthMiddleware(cfg *config.ClientAPI) func(http.Handler) http.Handler {
	respond := func(name string, code int, body any) http.Handler {
		return httputil.MakeExternalAPI(name, func(*http.Request) util.JSONResponse {
			return util.JSONResponse{Code: code, JSON: body}
		})
	}
	unrecognized := respond("delegated_auth_disabled", http.StatusNotFound,
		spec.Unrecognized("This endpoint is not available: authentication is delegated to an external provider"))
	loginRefused := respond("delegated_auth_appservice_login", http.StatusBadRequest, spec.MatrixError{
		ErrCode: errAppserviceLoginUnsupported,
		Err:     "Appservices cannot log in while authentication is delegated, use PUT /devices/{deviceId} and the device_id parameter",
	})
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			disabled, appservices := delegatedAuthEndpoint(req.Method, req.URL.Path)
			if !disabled {
				next.ServeHTTP(w, req)
				return
			}
			if !isAppserviceRequest(req, cfg) {
				unrecognized.ServeHTTP(w, req)
				return
			}
			switch appservices {
			case appserviceAllowed:
				next.ServeHTTP(w, req)
			case appserviceRegisterOnly:
				if registersWithoutLogin(req) {
					next.ServeHTTP(w, req)
					return
				}
				loginRefused.ServeHTTP(w, req)
			case appserviceLoginRefused:
				loginRefused.ServeHTTP(w, req)
			default:
				unrecognized.ServeHTTP(w, req)
			}
		})
	}
}

const authMetadataCacheControl = "public, max-age=3600"

// authMetadata serves the authorization server metadata (MSC2965).
func authMetadata(provider *delegatedauth.Provider) http.Handler {
	return httputil.MakeExternalAPI("auth_metadata", func(req *http.Request) util.JSONResponse {
		if provider == nil {
			return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.Unrecognized("Next-generation auth is not enabled")}
		}
		metadata, err := provider.Metadata(req.Context())
		if err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("failed to fetch the authorization server metadata")
			return util.JSONResponse{Code: http.StatusBadGateway, JSON: spec.Unknown("Failed to fetch the authorization server metadata")}
		}
		return util.JSONResponse{
			Code:    http.StatusOK,
			JSON:    metadata,
			Headers: map[string]string{"Cache-Control": authMetadataCacheControl},
		}
	})
}

// authIssuer serves just the issuer, as used by the earlier revisions of MSC2965.
func authIssuer(provider *delegatedauth.Provider) http.Handler {
	return httputil.MakeExternalAPI("auth_issuer", func(req *http.Request) util.JSONResponse {
		if provider == nil {
			return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.Unrecognized("Next-generation auth is not enabled")}
		}
		return util.JSONResponse{
			Code:    http.StatusOK,
			JSON:    map[string]string{"issuer": provider.Issuer()},
			Headers: map[string]string{"Cache-Control": authMetadataCacheControl},
		}
	})
}
