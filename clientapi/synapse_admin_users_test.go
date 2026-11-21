package clientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	"github.com/element-hq/dendrite/userapi"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSynapseAdminUsers(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		natsInstance := jetstream.NATSInstance{}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, caching.DisableMetrics)
		tokens := map[*test.User]userDevice{admin: {}, bob: {}}
		createAccessTokens(t, tokens, userAPI, ctx, routers)

		call := func(user *test.User, method, path string, body any) (int, gjson.Result) {
			t.Helper()
			var req *http.Request
			if body != nil {
				req = test.NewRequest(t, method, path, test.WithJSONBody(t, body))
			} else {
				req = test.NewRequest(t, method, path)
			}
			if user != nil {
				req.Header.Set("Authorization", "Bearer "+tokens[user].accessToken)
			}
			rec := httptest.NewRecorder()
			routers.SynapseAdmin.ServeHTTP(rec, req)
			return rec.Code, gjson.Parse(rec.Body.String())
		}

		t.Run("only admins", func(t *testing.T) {
			code, _ := call(bob, http.MethodGet, "/_synapse/admin/v2/users", nil)
			assert.Equal(t, http.StatusForbidden, code)
			code, _ = call(nil, http.MethodGet, "/_synapse/admin/v2/users", nil)
			assert.Equal(t, http.StatusUnauthorized, code)
		})

		t.Run("list", func(t *testing.T) {
			code, res := call(admin, http.MethodGet, "/_synapse/admin/v2/users", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(3), res.Get("total").Int(), "alice, bob and the account the server sends notices from")
			assert.Equal(t, int64(1), res.Get(`users.#(name=="`+admin.ID+`").admin`).Int())
			assert.Equal(t, int64(0), res.Get(`users.#(name=="`+bob.ID+`").admin`).Int())
			assert.False(t, res.Get("next_token").Exists(), "all of them fit on the page")

			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users?limit=1", nil)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, "1", res.Get("next_token").String())
			assert.Len(t, res.Get("users").Array(), 1)
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users?limit=1&from=2", nil)
			assert.Equal(t, http.StatusOK, code)
			assert.False(t, res.Get("next_token").Exists(), "the last page has no next token")

			for _, bad := range []string{"limit=-1", "from=x", "order_by=x", "dir=sideways", "guests=maybe"} {
				code, _ = call(admin, http.MethodGet, "/_synapse/admin/v2/users?"+bad, nil)
				assert.Equal(t, http.StatusBadRequest, code, bad)
			}
		})

		newUser := "@newuser:" + string(cfg.Global.ServerName)
		t.Run("create, change and look at a user", func(t *testing.T) {
			code, res := call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+newUser, map[string]any{"password": "a-good-password-1", "displayname": "New", "admin": false})
			require.Equal(t, http.StatusCreated, code, res.Raw)
			assert.Equal(t, newUser, res.Get("name").String())
			assert.Equal(t, "New", res.Get("displayname").String())

			code, res = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+newUser, map[string]any{"admin": true, "displayname": "Newer"})
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(1), res.Get("admin").Int())
			assert.Equal(t, "Newer", res.Get("displayname").String())

			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users/"+newUser, nil)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, int64(1), res.Get("admin").Int())
			assert.True(t, res.Get("threepids").IsArray())

			code, res = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+newUser, map[string]any{"deactivated": true})
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(1), res.Get("deactivated").Int())
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users?deactivated=true&name=newer", nil)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, int64(1), res.Get("total").Int(), "deactivated users are listed on request")
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users?name=newer", nil)
			assert.Equal(t, int64(0), res.Get("total").Int())
		})

		t.Run("errors", func(t *testing.T) {
			code, _ := call(admin, http.MethodGet, "/_synapse/admin/v2/users/@nobody:"+string(cfg.Global.ServerName), nil)
			assert.Equal(t, http.StatusNotFound, code)
			code, _ = call(admin, http.MethodGet, "/_synapse/admin/v2/users/@someone:elsewhere.example", nil)
			assert.Equal(t, http.StatusBadRequest, code, "only local users")
			code, _ = call(admin, http.MethodPut, "/_synapse/admin/v2/users/@_bad:"+string(cfg.Global.ServerName), map[string]any{"password": "a-good-password-1"})
			assert.Equal(t, http.StatusBadRequest, code, "usernames are validated")
			code, _ = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+bob.ID, map[string]any{"password": "weak"})
			assert.Equal(t, http.StatusBadRequest, code)

			// What registration refuses, an administrator cannot create either: the numeric names are the guests'.
			code, res := call(admin, http.MethodPut, "/_synapse/admin/v2/users/@12345:"+string(cfg.Global.ServerName), map[string]any{"password": "a-good-password-1"})
			assert.Equal(t, http.StatusBadRequest, code, "numeric usernames are reserved: %s", res.Raw)
			assert.Equal(t, "M_INVALID_USERNAME", res.Get("errcode").String())

			// The exclusive namespaces of the application services are theirs.
			cfg.ClientAPI.Derived = &config.Derived{}
			cfg.ClientAPI.Derived.ExclusiveApplicationServicesUsernameRegexp = regexp.MustCompile(`^@bridge_.*:` + regexp.QuoteMeta(string(cfg.Global.ServerName)) + `$`)
			code, res = call(admin, http.MethodPut, "/_synapse/admin/v2/users/@bridge_ghost:"+string(cfg.Global.ServerName), map[string]any{"password": "a-good-password-1"})
			assert.Equal(t, http.StatusBadRequest, code, "the namespace of an application service: %s", res.Raw)
			assert.Equal(t, "M_EXCLUSIVE", res.Get("errcode").String())

			// An administrator does not take their own rights or account away: nobody may be left to use the API.
			code, _ = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+admin.ID, map[string]any{"admin": false})
			assert.Equal(t, http.StatusBadRequest, code, "no self-demotion")
			code, _ = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+admin.ID, map[string]any{"deactivated": true})
			assert.Equal(t, http.StatusBadRequest, code, "no self-deactivation")
			code, _ = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+admin.ID, map[string]any{"admin": true, "displayname": "Still admin"})
			assert.Equal(t, http.StatusOK, code, "changing oneself otherwise is fine")
		})

		t.Run("password, devices and rooms", func(t *testing.T) {
			code, res := call(admin, http.MethodPost, "/_synapse/admin/v1/reset_password/"+bob.ID, map[string]any{"new_password": "another-good-password-1"})
			require.Equal(t, http.StatusOK, code, res.Raw)

			// The reset logged bob out, so he has no devices; a new one is made through the API of the user.
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users/"+bob.ID+"/devices", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(0), res.Get("total").Int())
			code, _ = call(admin, http.MethodPost, "/_synapse/admin/v2/users/"+bob.ID+"/delete_devices", map[string]any{"devices": []string{"X"}})
			assert.Equal(t, http.StatusOK, code)
			code, _ = call(admin, http.MethodPost, "/_synapse/admin/v2/users/"+bob.ID+"/delete_devices", map[string]any{})
			assert.Equal(t, http.StatusBadRequest, code)

			code, res = call(admin, http.MethodGet, "/_synapse/admin/v1/users/"+bob.ID+"/joined_rooms", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(0), res.Get("total").Int())

			code, res = call(admin, http.MethodPost, "/_synapse/admin/v1/deactivate/"+bob.ID, map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, "no-support", res.Get("id_server_unbind_result").String())
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/users/"+bob.ID, nil)
			assert.Equal(t, int64(1), res.Get("deactivated").Int())
		})

		t.Run("the authentication service owns passwords and deactivation", func(t *testing.T) {
			cfg.ClientAPI.MSC3861.Enabled = true
			defer func() { cfg.ClientAPI.MSC3861.Enabled = false }()
			code, _ := call(admin, http.MethodPost, "/_synapse/admin/v1/reset_password/"+admin.ID, map[string]any{"new_password": "another-good-password-1"})
			assert.Equal(t, http.StatusNotFound, code)
			code, _ = call(admin, http.MethodPost, "/_synapse/admin/v1/deactivate/"+admin.ID, map[string]any{})
			assert.Equal(t, http.StatusNotFound, code)
			code, _ = call(admin, http.MethodPut, "/_synapse/admin/v2/users/"+admin.ID, map[string]any{"password": "another-good-password-1"})
			assert.Equal(t, http.StatusBadRequest, code)
		})
	})
}

func TestSynapseAdminRegistrationTokensAndVersion(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		cfg.ClientAPI.RegistrationRequiresToken = true
		defer closeFn()
		natsInstance := jetstream.NATSInstance{}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, caching.DisableMetrics)
		tokens := map[*test.User]userDevice{admin: {}}
		createAccessTokens(t, tokens, userAPI, ctx, routers)

		call := func(method, path string, body any) (int, gjson.Result) {
			t.Helper()
			req := test.NewRequest(t, method, path)
			if body != nil {
				req = test.NewRequest(t, method, path, test.WithJSONBody(t, body))
			}
			req.Header.Set("Authorization", "Bearer "+tokens[admin].accessToken)
			rec := httptest.NewRecorder()
			routers.SynapseAdmin.ServeHTTP(rec, req)
			return rec.Code, gjson.Parse(rec.Body.String())
		}

		code, res := call(http.MethodPost, "/_synapse/admin/v1/registration_tokens/new", map[string]any{"token": "abcdef123", "uses_allowed": 3})
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "abcdef123", res.Get("token").String())
		assert.Equal(t, int64(3), res.Get("uses_allowed").Int())
		code, res = call(http.MethodGet, "/_synapse/admin/v1/registration_tokens", nil)
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "abcdef123", res.Get("registration_tokens.0.token").String())
		code, res = call(http.MethodPut, "/_synapse/admin/v1/registration_tokens/abcdef123", map[string]any{"uses_allowed": 5})
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, int64(5), res.Get("uses_allowed").Int())
		code, _ = call(http.MethodDelete, "/_synapse/admin/v1/registration_tokens/abcdef123", nil)
		assert.Equal(t, http.StatusOK, code)
		code, _ = call(http.MethodGet, "/_synapse/admin/v1/registration_tokens/abcdef123", nil)
		assert.Equal(t, http.StatusNotFound, code)

		code, res = call(http.MethodGet, "/_synapse/admin/v1/server_version", nil)
		require.Equal(t, http.StatusOK, code)
		assert.Contains(t, res.Get("server_version").String(), "Dendrite")
	})
}
