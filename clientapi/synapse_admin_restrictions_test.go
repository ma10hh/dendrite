package clientapi

import (
	"context"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/internal/transactions"
	"github.com/element-hq/dendrite/roomserver"
	rsapi "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	"github.com/element-hq/dendrite/userapi"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSynapseAdminRestrictions(t *testing.T) {
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
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, transactions.New(), nil, userAPI, nil, nil, caching.DisableMetrics)
		tokens := map[*test.User]userDevice{admin: {}, bob: {}}
		createAccessTokens(t, tokens, userAPI, ctx, routers)

		call := func(router http.Handler, user *test.User, method, path string, body any) (int, gjson.Result) {
			t.Helper()
			var req *http.Request
			if body != nil {
				req = test.NewRequest(t, method, path, test.WithJSONBody(t, body))
			} else {
				req = test.NewRequest(t, method, path)
			}
			req.Header.Set("Authorization", "Bearer "+tokens[user].accessToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			return rec.Code, gjson.Parse(rec.Body.String())
		}
		adminCall := func(method, path string, body any) (int, gjson.Result) {
			return call(routers.SynapseAdmin, admin, method, path, body)
		}

		room := test.NewRoom(t, bob)
		require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
		send := func(txn string) (int, gjson.Result) {
			return call(routers.Client, bob, http.MethodPut, "/_matrix/client/v3/rooms/"+room.ID+"/send/m.room.message/"+txn, map[string]any{"msgtype": "m.text", "body": "hi"})
		}
		known := func(eventID string) bool {
			var res rsapi.QueryEventsByIDResponse
			require.NoError(t, rsAPI.QueryEventsByID(ctx, &rsapi.QueryEventsByIDRequest{RoomID: room.ID, EventIDs: []string{eventID}}, &res))
			return len(res.Events) == 1
		}

		t.Run("shadow ban", func(t *testing.T) {
			code, res := send("before")
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.True(t, known(res.Get("event_id").String()), "an event of a user who is not banned is sent")

			code, res = adminCall(http.MethodPost, "/_synapse/admin/v1/users/"+bob.ID+"/shadow_ban", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			code, res = adminCall(http.MethodGet, "/_synapse/admin/v2/users/"+bob.ID, nil)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, int64(1), res.Get("shadow_banned").Int())

			code, res = send("during")
			require.Equal(t, http.StatusOK, code, "the user is told that it worked: %s", res.Raw)
			fake := res.Get("event_id").String()
			assert.NotEmpty(t, fake)
			assert.False(t, known(fake), "nothing was sent")

			code, res = adminCall(http.MethodDelete, "/_synapse/admin/v1/users/"+bob.ID+"/shadow_ban", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			code, res = send("after")
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.True(t, known(res.Get("event_id").String()), "the ban is lifted")

			code, _ = adminCall(http.MethodPost, "/_synapse/admin/v1/users/"+admin.ID+"/shadow_ban", nil)
			assert.Equal(t, http.StatusBadRequest, code, "administrators cannot be shadow banned")
			code, _ = adminCall(http.MethodPost, "/_synapse/admin/v1/users/@nobody:"+string(cfg.Global.ServerName)+"/shadow_ban", nil)
			assert.Equal(t, http.StatusNotFound, code)
		})

		t.Run("rate limit override", func(t *testing.T) {
			path := "/_synapse/admin/v1/users/" + bob.ID + "/override_ratelimit"
			code, res := adminCall(http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, "{}", res.Raw, "none set")

			code, res = adminCall(http.MethodPost, path, map[string]any{"messages_per_second": 2.5, "burst_count": 10})
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, 2.5, res.Get("messages_per_second").Float())
			code, res = adminCall(http.MethodGet, path, nil)
			assert.Equal(t, int64(10), res.Get("burst_count").Int())

			code, res = adminCall(http.MethodPost, path, map[string]any{"messages_per_second": 0, "burst_count": 0})
			require.Equal(t, http.StatusOK, code, "both 0 means no limit: %s", res.Raw)
			for _, bad := range []map[string]any{{"messages_per_second": -1, "burst_count": 1}, {"messages_per_second": 1, "burst_count": 0}} {
				code, _ = adminCall(http.MethodPost, path, bad)
				assert.Equal(t, http.StatusBadRequest, code)
			}

			code, _ = adminCall(http.MethodDelete, path, nil)
			assert.Equal(t, http.StatusOK, code)
			_, res = adminCall(http.MethodGet, path, nil)
			assert.Equal(t, "{}", res.Raw, "removed")
		})

		t.Run("account validity", func(t *testing.T) {
			path := "/_synapse/admin/v1/account_validity/validity"
			past := time.Now().Add(-time.Hour).UnixMilli()
			code, res := adminCall(http.MethodPost, path, map[string]any{"user_id": bob.ID, "expiration_ts": past})
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, past, res.Get("expiration_ts").Int())

			code, res = call(routers.Client, bob, http.MethodGet, "/_matrix/client/v3/account/whoami", nil)
			assert.Equal(t, http.StatusForbidden, code, res.Raw)
			assert.Equal(t, "ORG_MATRIX_EXPIRED_ACCOUNT", res.Get("errcode").String())

			future := time.Now().Add(time.Hour).UnixMilli()
			code, _ = adminCall(http.MethodPost, path, map[string]any{"user_id": bob.ID, "expiration_ts": future})
			require.Equal(t, http.StatusOK, code)
			code, _ = call(routers.Client, bob, http.MethodGet, "/_matrix/client/v3/account/whoami", nil)
			assert.Equal(t, http.StatusOK, code, "renewed")

			code, _ = adminCall(http.MethodPost, path, map[string]any{"user_id": bob.ID, "expiration_ts": past})
			require.Equal(t, http.StatusOK, code)
			code, _ = adminCall(http.MethodPost, path, map[string]any{"user_id": bob.ID, "expiration_ts": 0})
			require.Equal(t, http.StatusOK, code)
			code, _ = call(routers.Client, bob, http.MethodGet, "/_matrix/client/v3/account/whoami", nil)
			assert.Equal(t, http.StatusOK, code, "0 means never")

			for _, bad := range []map[string]any{
				{"user_id": bob.ID}, {"user_id": bob.ID, "expiration_ts": -1},
				{"expiration_ts": 1}, {"user_id": bob.ID, "expiration_ts": future, "enable_renewal_emails": true},
				{"user_id": admin.ID, "expiration_ts": future},
			} {
				code, _ = adminCall(http.MethodPost, path, bad)
				assert.Equal(t, http.StatusBadRequest, code, "%v", bad)
			}
			code, _ = adminCall(http.MethodPost, path, map[string]any{"user_id": "@nobody:" + string(cfg.Global.ServerName), "expiration_ts": future})
			assert.Equal(t, http.StatusNotFound, code)
		})

		t.Run("user by third party identifier", func(t *testing.T) {
			localpart, serverName, _ := splitUserID(t, bob.ID)
			require.NoError(t, userAPI.PerformSaveThreePIDAssociation(ctx, &uapi.PerformSaveThreePIDAssociationRequest{
				ThreePID: "bob@example.org", Localpart: localpart, ServerName: serverName, Medium: "email",
			}, &struct{}{}))
			code, res := adminCall(http.MethodGet, "/_synapse/admin/v1/threepid/email/users/bob@example.org", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, bob.ID, res.Get("user_id").String())
			code, _ = adminCall(http.MethodGet, "/_synapse/admin/v1/threepid/email/users/nobody@example.org", nil)
			assert.Equal(t, http.StatusNotFound, code)
		})
	})
}

func splitUserID(t *testing.T, userID string) (string, spec.ServerName, error) {
	t.Helper()
	localpart, serverName, err := gomatrixserverlib.SplitID('@', userID)
	require.NoError(t, err)
	return localpart, serverName, nil
}
