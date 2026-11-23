package clientapi

import (
	"context"
	"github.com/element-hq/dendrite/clientapi/routing"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
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

const delayQuery = "org.matrix.msc4140.delay"

func TestDelayedEvents(t *testing.T) {
	alice := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cfg.ClientAPI.DelayedEvents.Enabled = true
		cfg.Global.WellKnownClientName = "https://hs.example.org"
		cfg.ClientAPI.DelayedEvents.MaxDelay = time.Minute
		cfg.ClientAPI.DelayedEvents.MaxPerUser = 3
		cfg.ClientAPI.MatrixRTC.Transports = append(cfg.ClientAPI.MatrixRTC.Transports, struct {
			Type              string `yaml:"type"`
			LiveKitServiceURL string `yaml:"livekit_service_url"`
		}{"livekit", "https://livekit-jwt.example.com"})
		natsInstance := jetstream.NATSInstance{}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, transactions.New(), nil, userAPI, nil, nil, caching.DisableMetrics)
		tokens := map[*test.User]userDevice{alice: {}, bob: {}}
		createAccessTokens(t, tokens, userAPI, ctx, routers)

		room := test.NewRoom(t, alice)
		require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))

		call := func(user *test.User, method, path string, body any) (int, gjson.Result) {
			t.Helper()
			req := test.NewRequest(t, method, path, test.WithJSONBody(t, body))
			req.Header.Set("Authorization", "Bearer "+tokens[user].accessToken)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec.Code, gjson.Parse(rec.Body.String())
		}
		topic := func() string {
			var res rsapi.QueryCurrentStateResponse
			tuple := gomatrixserverlib.StateKeyTuple{EventType: "m.room.topic", StateKey: ""}
			require.NoError(t, rsAPI.QueryCurrentState(ctx, &rsapi.QueryCurrentStateRequest{RoomID: room.ID, StateTuples: []gomatrixserverlib.StateKeyTuple{tuple}}, &res))
			if ev := res.StateEvents[tuple]; ev != nil {
				return gjson.GetBytes(ev.Content(), "topic").String()
			}
			return ""
		}
		setTopic := func(topic string, delayMS int) (int, gjson.Result) {
			path := "/_matrix/client/v3/rooms/" + room.ID + "/state/m.room.topic"
			if delayMS != 0 {
				path += "?" + delayQuery + "=" + strconv.Itoa(delayMS)
			}
			return call(alice, http.MethodPut, path, map[string]any{"topic": topic})
		}
		delayedPath := "/_matrix/client/unstable/org.matrix.msc4140/delayed_events"
		waitFor := func(want string) bool {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if topic() == want {
					return true
				}
				time.Sleep(25 * time.Millisecond)
			}
			return false
		}

		t.Run("the event is sent when the delay ends", func(t *testing.T) {
			code, res := setTopic("later", 400)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.NotEmpty(t, res.Get("delay_id").String())
			assert.Equal(t, "", topic(), "not sent yet")

			code, res = call(alice, http.MethodGet, delayedPath, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Len(t, res.Get("delayed_events").Array(), 1)
			assert.Equal(t, "m.room.topic", res.Get("delayed_events.0.type").String())
			assert.Equal(t, "", res.Get("delayed_events.0.state_key").String())
			assert.Equal(t, int64(400), res.Get("delayed_events.0.delay").Int())

			require.True(t, waitFor("later"), "the event was sent by its timer")
			_, res = call(alice, http.MethodGet, delayedPath, nil)
			assert.Empty(t, res.Get("delayed_events").Array(), "and is not waiting any more")
		})

		t.Run("send, cancel and restart", func(t *testing.T) {
			code, res := setTopic("sent by hand", 30000)
			require.Equal(t, http.StatusOK, code, res.Raw)
			id := res.Get("delay_id").String()
			code, res = call(alice, http.MethodPost, delayedPath+"/"+id, map[string]any{"action": "send"})
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.True(t, waitFor("sent by hand"))
			code, _ = call(alice, http.MethodPost, delayedPath+"/"+id, map[string]any{"action": "send"})
			assert.Equal(t, http.StatusNotFound, code, "it can only be sent once")

			_, res = setTopic("cancelled", 700)
			id = res.Get("delay_id").String()
			code, res = call(alice, http.MethodPost, delayedPath+"/"+id, map[string]any{"action": "cancel"})
			require.Equal(t, http.StatusOK, code, res.Raw)
			time.Sleep(1200 * time.Millisecond)
			assert.Equal(t, "sent by hand", topic(), "a cancelled event is not sent")

			// The old form of the call has the action in the path.
			_, res = setTopic("old form", 30000)
			code, _ = call(alice, http.MethodPost, delayedPath+"/"+res.Get("delay_id").String()+"/cancel", map[string]any{})
			assert.Equal(t, http.StatusOK, code)

			_, res = setTopic("restarted", 1500)
			id = res.Get("delay_id").String()
			time.Sleep(1000 * time.Millisecond)
			code, res = call(alice, http.MethodPost, delayedPath+"/"+id, map[string]any{"action": "restart"})
			require.Equal(t, http.StatusOK, code, res.Raw)
			time.Sleep(1000 * time.Millisecond)
			assert.Equal(t, "sent by hand", topic(), "two seconds after it was set, but one after the restart")
			require.True(t, waitFor("restarted"), "after the restart it is sent")
		})

		t.Run("a newer state event cancels the waiting one", func(t *testing.T) {
			_, res := setTopic("too late", 800)
			require.NotEmpty(t, res.Get("delay_id").String())
			code, _ := setTopic("newer", 0)
			require.Equal(t, http.StatusOK, code)
			time.Sleep(1500 * time.Millisecond)
			assert.Equal(t, "newer", topic())
			_, res = call(alice, http.MethodGet, delayedPath, nil)
			assert.Empty(t, res.Get("delayed_events").Array())
		})

		t.Run("what is refused", func(t *testing.T) {
			for _, bad := range []string{"0", "-5", "soon", "1.5"} {
				code, res := call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+room.ID+"/state/m.room.topic?"+delayQuery+"="+bad, map[string]any{"topic": "x"})
				assert.Equal(t, http.StatusBadRequest, code, "%s: %s", bad, res.Raw)
			}
			code, res := setTopic("too long", 61000)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "M_MAX_DELAY_EXCEEDED", res.Get(`org\.matrix\.msc4140\.errcode`).String())
			assert.Equal(t, int64(60000), res.Get("max_delay").Int())

			code, _ = call(bob, http.MethodPut, "/_matrix/client/v3/rooms/"+room.ID+"/state/m.room.topic?"+delayQuery+"=1000", map[string]any{"topic": "x"})
			assert.Equal(t, http.StatusForbidden, code, "somebody who is not in the room")

			for i := 0; i < 3; i++ {
				code, res = setTopic("fill", 30000)
				require.Equal(t, http.StatusOK, code, res.Raw)
			}
			code, res = setTopic("one too many", 30000)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "M_LIMIT_EXCEEDED", res.Get("errcode").String())
		})

		t.Run("only the owner can use a delayed event", func(t *testing.T) {
			_, res := call(alice, http.MethodGet, delayedPath, nil)
			id := res.Get("delayed_events.0.delay_id").String()
			require.NotEmpty(t, id)
			for _, action := range []string{"send", "cancel", "restart"} {
				code, _ := call(bob, http.MethodPost, delayedPath+"/"+id, map[string]any{"action": action})
				assert.Equal(t, http.StatusNotFound, code, action)
			}
			_, res = call(bob, http.MethodGet, delayedPath, nil)
			assert.Empty(t, res.Get("delayed_events").Array())
			code, _ := call(alice, http.MethodPost, delayedPath+"/"+id, map[string]any{"action": "dance"})
			assert.Equal(t, http.StatusBadRequest, code)
			code, _ = call(alice, http.MethodPost, delayedPath+"/"+id, map[string]any{})
			assert.Equal(t, http.StatusBadRequest, code)
		})

		t.Run("overdue events are sent when the server starts", func(t *testing.T) {
			// Clear what the earlier cases left waiting.
			events, err := userAPI.DelayedEventsForUser(ctx, alice.ID)
			require.NoError(t, err)
			for _, e := range events {
				_, err = userAPI.DeleteDelayedEvent(ctx, e.DelayID)
				require.NoError(t, err)
			}
			empty := ""
			require.NoError(t, userAPI.InsertDelayedEvent(ctx, uapi.DelayedEvent{
				DelayID: "from-before-the-restart", UserID: alice.ID, DeviceID: tokens[alice].deviceID, RoomID: room.ID,
				EventType: "m.room.topic", StateKey: &empty, Content: []byte(`{"topic":"overdue"}`),
				DelayMS: 1000, RunningSinceMS: time.Now().Add(-time.Hour).UnixMilli(),
			}))
			d := routing.NewDelayedEvents(&cfg.ClientAPI, userAPI, rsAPI)
			require.NoError(t, d.Start(ctx))
			require.True(t, waitFor("overdue"))
		})

		t.Run("media servers for calls", func(t *testing.T) {
			code, res := call(alice, http.MethodGet, "/_matrix/client/unstable/org.matrix.msc4143/rtc/transports", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			assert.Equal(t, "livekit", res.Get("rtc_transports.0.type").String())
			assert.Equal(t, "https://livekit-jwt.example.com", res.Get("rtc_transports.0.livekit_service_url").String())

			req := test.NewRequest(t, http.MethodGet, "/_matrix/client/versions")
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			assert.True(t, gjson.Get(rec.Body.String(), `unstable_features.org\.matrix\.msc4140`).Bool(), rec.Body.String())

			rec = httptest.NewRecorder()
			routers.WellKnown.ServeHTTP(rec, test.NewRequest(t, http.MethodGet, "/.well-known/matrix/client"))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, "livekit", gjson.Get(rec.Body.String(), `org\.matrix\.msc4143\.rtc_foci.0.type`).String(), rec.Body.String())
			assert.Equal(t, "https://livekit-jwt.example.com", gjson.Get(rec.Body.String(), `org\.matrix\.msc4143\.rtc_foci.0.livekit_service_url`).String())
		})
	})
}

func TestDelayedEventsOff(t *testing.T) {
	alice := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
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
		tokens := map[*test.User]userDevice{alice: {}}
		createAccessTokens(t, tokens, userAPI, ctx, routers)

		call := func(method, path string, body any) (int, gjson.Result) {
			req := test.NewRequest(t, method, path, test.WithJSONBody(t, body))
			req.Header.Set("Authorization", "Bearer "+tokens[alice].accessToken)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec.Code, gjson.Parse(rec.Body.String())
		}
		// A client that asks for a delay must not be answered by sending the event at once.
		code, res := call(http.MethodPut, "/_matrix/client/v3/rooms/!x:test/state/m.room.topic?"+delayQuery+"=1000", map[string]any{"topic": "x"})
		assert.Equal(t, http.StatusBadRequest, code, res.Raw)
		assert.Equal(t, "M_UNRECOGNIZED", res.Get("errcode").String())
		code, _ = call(http.MethodGet, "/_matrix/client/unstable/org.matrix.msc4140/delayed_events", nil)
		assert.NotEqual(t, http.StatusOK, code, "the endpoints are not there")
		code, _ = call(http.MethodGet, "/_matrix/client/unstable/org.matrix.msc4143/rtc/transports", nil)
		assert.NotEqual(t, http.StatusOK, code, "nor are the transports, without any configured")

		req := test.NewRequest(t, http.MethodGet, "/_matrix/client/versions")
		rec := httptest.NewRecorder()
		routers.Client.ServeHTTP(rec, req)
		assert.False(t, gjson.Get(rec.Body.String(), `unstable_features.org\.matrix\.msc4140`).Bool(), "not advertised")
	})
}
