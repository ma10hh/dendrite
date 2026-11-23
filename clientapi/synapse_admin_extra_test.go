package clientapi

import (
	"github.com/tidwall/gjson"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/test"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSynapseAdminExtra(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, nil, admin, bob)
		defer closeFn()
		as := func(method, path string, body any) (int, gjson.Result) {
			return h.call(h.routers.SynapseAdmin, admin, method, path, body)
		}
		server := string(h.cfg.Global.ServerName)

		t.Run("only admins", func(t *testing.T) {
			for _, p := range []string{"/users/" + bob.ID + "/admin", "/users/" + bob.ID + "/pushers", "/users/" + bob.ID + "/accountdata", "/username_available?username=x", "/scheduled_tasks", "/background_updates/status"} {
				code, _ := h.call(h.routers.SynapseAdmin, bob, http.MethodGet, "/_synapse/admin/v1"+p, nil)
				assert.Equal(t, http.StatusForbidden, code, p)
			}
		})

		t.Run("admin flag", func(t *testing.T) {
			code, res := as(http.MethodGet, "/_synapse/admin/v1/users/"+bob.ID+"/admin", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.False(t, res.Get("admin").Bool())
			code, _ = as(http.MethodPut, "/_synapse/admin/v1/users/"+bob.ID+"/admin", map[string]any{"admin": true})
			require.Equal(t, http.StatusOK, code)
			_, res = as(http.MethodGet, "/_synapse/admin/v1/users/"+bob.ID+"/admin", nil)
			assert.True(t, res.Get("admin").Bool())
			code, _ = as(http.MethodPut, "/_synapse/admin/v1/users/"+bob.ID+"/admin", map[string]any{"admin": false})
			assert.Equal(t, http.StatusOK, code)
			code, _ = as(http.MethodPut, "/_synapse/admin/v1/users/"+admin.ID+"/admin", map[string]any{"admin": false})
			assert.Equal(t, http.StatusBadRequest, code, "no self-demotion")
			code, _ = as(http.MethodPut, "/_synapse/admin/v1/users/"+bob.ID+"/admin", map[string]any{})
			assert.Equal(t, http.StatusBadRequest, code)
			code, _ = as(http.MethodGet, "/_synapse/admin/v1/users/@nobody:"+server+"/admin", nil)
			assert.Equal(t, http.StatusNotFound, code)
		})

		t.Run("pushers and account data", func(t *testing.T) {
			code, res := as(http.MethodGet, "/_synapse/admin/v1/users/"+bob.ID+"/pushers", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(0), res.Get("total").Int())
			assert.True(t, res.Get("pushers").IsArray())

			require.NoError(t, h.userAPI.InputAccountData(h.ctx, &uapi.InputAccountDataRequest{
				UserID: bob.ID, DataType: "org.example.setting", AccountData: []byte(`{"a":1}`),
			}, &uapi.InputAccountDataResponse{}))
			code, res = as(http.MethodGet, "/_synapse/admin/v1/users/"+bob.ID+"/accountdata", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(1), res.Get(`account_data.global.org\.example\.setting.a`).Int())
			assert.True(t, res.Get("account_data.rooms").IsObject())
		})

		t.Run("whois and username availability", func(t *testing.T) {
			code, res := as(http.MethodGet, "/_synapse/admin/v1/whois/"+bob.ID, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, bob.ID, res.Get("user_id").String())

			code, res = as(http.MethodGet, "/_synapse/admin/v1/username_available?username=somebodynew", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.True(t, res.Get("available").Bool())
			local, _, _ := splitUserID(t, bob.ID)
			code, res = as(http.MethodGet, "/_synapse/admin/v1/username_available?username="+local, nil)
			assert.Equal(t, http.StatusBadRequest, code, "taken: %s", res.Raw)
			code, _ = as(http.MethodGet, "/_synapse/admin/v1/username_available", nil)
			assert.Equal(t, http.StatusBadRequest, code)
		})

		t.Run("what Dendrite has nothing for", func(t *testing.T) {
			code, res := as(http.MethodGet, "/_synapse/admin/v1/background_updates/status", nil)
			require.Equal(t, http.StatusOK, code)
			assert.True(t, res.Get("enabled").Bool())
			assert.True(t, res.Get("current_updates").IsObject())
			code, res = as(http.MethodGet, "/_synapse/admin/v1/background_updates/enabled", nil)
			require.Equal(t, http.StatusOK, code)
			assert.True(t, res.Get("enabled").Bool())
			code, res = as(http.MethodGet, "/_synapse/admin/v1/scheduled_tasks", nil)
			require.Equal(t, http.StatusOK, code)
			assert.Empty(t, res.Get("scheduled_tasks").Array())
			code, res = as(http.MethodGet, "/_synapse/admin/v1/experimental_features/"+bob.ID, nil)
			require.Equal(t, http.StatusOK, code)
			assert.True(t, res.Get("features").IsObject())
			code, _ = as(http.MethodPut, "/_synapse/admin/v1/experimental_features/"+bob.ID, map[string]any{"features": map[string]bool{"x": true}})
			assert.Equal(t, http.StatusBadRequest, code, "a change is refused, not ignored")
		})
	})
}
