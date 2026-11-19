package routing

import (
	"net/http"
	"regexp"

	"github.com/element-hq/dendrite/internal/delegatedauth"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// Endpoints which only make sense when Dendrite handles authentication itself. With
// MSC3861 they are the responsibility of the external provider and answer M_UNRECOGNIZED.
var delegatedAuthDisabledEndpoints = []struct {
	methods map[string]bool
	path    *regexp.Regexp
}{
	{nil, regexp.MustCompile(`^/login(/.*)?$`)},
	{nil, regexp.MustCompile(`^/register(/.*)?$`)},
	{nil, regexp.MustCompile(`^/logout(/all)?$`)},
	{nil, regexp.MustCompile(`^/account/(password|deactivate)(/.*)?$`)},
	{nil, regexp.MustCompile(`^/account/3pid/.+$`)},
	{map[string]bool{http.MethodPost: true}, regexp.MustCompile(`^/account/3pid$`)},
	{nil, regexp.MustCompile(`^/delete_devices$`)},
	{map[string]bool{http.MethodDelete: true}, regexp.MustCompile(`^/devices/[^/]+$`)},
	{nil, regexp.MustCompile(`^/auth/[^/]+/fallback/web$`)},
}

var versionedPathRegexp = regexp.MustCompile(`^/_matrix/client/(?:r0|v3)(/.*)$`)

func isDelegatedAuthDisabledEndpoint(method, path string) bool {
	m := versionedPathRegexp.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	for _, e := range delegatedAuthDisabledEndpoints {
		if e.path.MatchString(m[1]) && (e.methods == nil || e.methods[method]) {
			// Let CORS preflights through; the browser needs them to read the error.
			return method != http.MethodOptions
		}
	}
	return false
}

// delegatedAuthMiddleware answers requests for the endpoints disabled by MSC3861.
func delegatedAuthMiddleware(next http.Handler) http.Handler {
	disabled := httputil.MakeExternalAPI("delegated_auth_disabled", func(req *http.Request) util.JSONResponse {
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.Unrecognized("This endpoint is not available: authentication is delegated to an external provider"),
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if isDelegatedAuthDisabledEndpoint(req.Method, req.URL.Path) {
			disabled.ServeHTTP(w, req)
			return
		}
		next.ServeHTTP(w, req)
	})
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
