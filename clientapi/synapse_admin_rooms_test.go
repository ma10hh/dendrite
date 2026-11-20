package clientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/federationapi"
	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	"github.com/element-hq/dendrite/userapi"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSynapseAdminRooms(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t)
	ctx := context.Background()

	alpha := test.NewRoom(t, admin, test.RoomPreset(test.PresetPublicChat))
	alpha.CreateAndInsert(t, admin, spec.MRoomName, map[string]string{"name": "Alpha"}, test.WithStateKey(""))
	alpha.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))
	beta := test.NewRoom(t, admin, test.RoomPreset(test.PresetPrivateChat))
	beta.CreateAndInsert(t, admin, spec.MRoomName, map[string]string{"name": "beta room"}, test.WithStateKey(""))
	beta.CreateAndInsert(t, admin, "m.room.encryption", map[string]string{"algorithm": "m.megolm.v1.aes-sha2"}, test.WithStateKey(""))

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		fsAPI := federationapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, nil, rsAPI, caches, nil, true)
		rsAPI.SetFederationAPI(fsAPI, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		for _, room := range []*test.Room{alpha, beta} {
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", api.DoNotSendToOtherServers, nil, false))
		}
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, caching.DisableMetrics)
		require.NoError(t, rsAPI.PerformPublish(ctx, &api.PerformPublishRequest{RoomID: alpha.ID, Visibility: spec.Public}))
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
			req.Header.Set("Authorization", "Bearer "+tokens[user].accessToken)
			rec := httptest.NewRecorder()
			routers.SynapseAdmin.ServeHTTP(rec, req)
			return rec.Code, gjson.Parse(rec.Body.String())
		}

		t.Run("only admins", func(t *testing.T) {
			code, _ := call(bob, http.MethodGet, "/_synapse/admin/v1/rooms", nil)
			assert.Equal(t, http.StatusForbidden, code)
		})

		t.Run("list", func(t *testing.T) {
			code, res := call(admin, http.MethodGet, "/_synapse/admin/v1/rooms", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, int64(2), res.Get("total_rooms").Int())
			assert.Equal(t, alpha.ID, res.Get("rooms.0.room_id").String(), "by name: Alpha before beta room")
			a := res.Get(`rooms.#(room_id=="` + alpha.ID + `")`)
			assert.Equal(t, "Alpha", a.Get("name").String())
			assert.Equal(t, int64(2), a.Get("joined_members").Int())
			assert.Equal(t, int64(2), a.Get("joined_local_members").Int())
			assert.True(t, a.Get("public").Bool())
			assert.Equal(t, admin.ID, a.Get("creator").String())
			assert.Equal(t, "9", a.Get("version").String())
			assert.True(t, a.Get("federatable").Bool())
			b := res.Get(`rooms.#(room_id=="` + beta.ID + `")`)
			assert.False(t, b.Get("public").Bool())
			assert.Equal(t, "m.megolm.v1.aes-sha2", b.Get("encryption").String())
			assert.Equal(t, "invite", b.Get("join_rules").String())

			_, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms?order_by=joined_members&dir=b&limit=1", nil)
			assert.Equal(t, alpha.ID, res.Get("rooms.0.room_id").String())
			assert.Equal(t, int64(1), res.Get("next_batch").Int())
			_, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms?order_by=joined_members&limit=1&from=1", nil)
			assert.Equal(t, alpha.ID, res.Get("rooms.0.room_id").String())
			assert.Equal(t, int64(0), res.Get("prev_batch").Int())
			assert.False(t, res.Get("next_batch").Exists())
			_, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms?search_term=BETA", nil)
			assert.Equal(t, int64(1), res.Get("total_rooms").Int())
			_, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms?order_by=creator", nil)
			assert.Equal(t, int64(2), res.Get("total_rooms").Int())
			for _, bad := range []string{"order_by=state_events", "order_by=x", "dir=x", "limit=-1"} {
				code, _ = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms?"+bad, nil)
				assert.Equal(t, http.StatusBadRequest, code, bad)
			}
		})

		t.Run("one room, its members and state", func(t *testing.T) {
			code, res := call(admin, http.MethodGet, "/_synapse/admin/v1/rooms/"+alpha.ID, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, "Alpha", res.Get("name").String())
			assert.Greater(t, res.Get("state_events").Int(), int64(5))
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms/"+alpha.ID+"/members", nil)
			require.Equal(t, http.StatusOK, code)
			assert.ElementsMatch(t, []string{admin.ID, bob.ID}, func() (out []string) {
				for _, m := range res.Get("members").Array() {
					out = append(out, m.String())
				}
				return
			}())
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms/"+alpha.ID+"/state", nil)
			require.Equal(t, http.StatusOK, code)
			assert.NotEmpty(t, res.Get(`state.#(type=="m.room.name")`).Raw)
			code, _ = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms/!nothere:test", nil)
			assert.Equal(t, http.StatusNotFound, code)
			code, _ = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms/notaroom", nil)
			assert.Equal(t, http.StatusBadRequest, code)
		})

		t.Run("delete", func(t *testing.T) {
			code, _ := call(admin, http.MethodDelete, "/_synapse/admin/v2/rooms/"+beta.ID, map[string]any{"block": true})
			assert.Equal(t, http.StatusBadRequest, code, "blocking is not supported")
			code, _ = call(admin, http.MethodDelete, "/_synapse/admin/v2/rooms/"+beta.ID, map[string]any{"message": "bye"})
			assert.Equal(t, http.StatusBadRequest, code)

			code, res := call(admin, http.MethodDelete, "/_synapse/admin/v2/rooms/"+beta.ID, map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			deleteID := res.Get("delete_id").String()
			require.NotEmpty(t, deleteID)
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/rooms/"+beta.ID+"/delete_status", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, "complete", res.Get("results.0.status").String())
			code, res = call(admin, http.MethodGet, "/_synapse/admin/v2/rooms/delete_status/"+deleteID, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, "complete", res.Get("status").String())
			assert.Contains(t, res.Get("kicked_users").String(), admin.ID)

			code, res = call(admin, http.MethodGet, "/_synapse/admin/v1/rooms", nil)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, int64(1), res.Get("total_rooms").Int(), "the room is gone")

			code, res = call(admin, http.MethodDelete, "/_synapse/admin/v1/rooms/"+alpha.ID, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Contains(t, res.Get("kicked_users").String(), bob.ID)
			code, _ = call(admin, http.MethodDelete, "/_synapse/admin/v1/rooms/"+alpha.ID, nil)
			assert.Equal(t, http.StatusNotFound, code)
		})
	})
}
