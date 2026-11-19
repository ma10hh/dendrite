package syncapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/api"
	rsapi "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSyncStateAfter(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		user := test.NewUser(t)
		room := test.NewRoom(t, user)
		alice := userapi.Device{ID: "ALICEID", UserID: user.ID, AccessToken: "ALICE_BEARER_TOKEN", DisplayName: "Alice", AccountType: userapi.AccountTypeUser}

		cfg, processCtx, close := testrig.CreateConfig(t, dbType)
		defer close()
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)

		topic := room.CreateAndInsert(t, user, "m.room.topic", map[string]string{"topic": "first"}, test.WithStateKey(""))
		lost := room.CreateAndInsert(t, user, "m.room.name", map[string]string{"name": "lost the conflict"}, test.WithStateKey(""))
		AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, &syncUserAPI{accounts: []userapi.Device{alice}}, &syncRoomserverAPI{rooms: []*test.Room{room}}, caches, caching.DisableMetrics)

		sync := func(query map[string]string) gjson.Result {
			t.Helper()
			query["access_token"] = alice.AccessToken
			query["timeout"] = "0"
			w := httptest.NewRecorder()
			routers.Client.ServeHTTP(w, test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(query)))
			if w.Code != 200 {
				t.Fatalf("sync %v: HTTP %d %s", query, w.Code, w.Body)
			}
			return gjson.ParseBytes(w.Body.Bytes())
		}
		// waitFor repeats the sync until the room's timeline has the event: the events arrive through NATS.
		waitFor := func(query map[string]string, eventID string) gjson.Result {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for {
				res := sync(copyQuery(query))
				for _, ev := range res.Get("rooms.join." + gjson.Escape(room.ID) + ".timeline.events").Array() {
					if ev.Get("event_id").String() == eventID {
						return res
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for %s with %v, last response: %s", eventID, query, res.Raw)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		roomSection := func(res gjson.Result) gjson.Result { return res.Get("rooms.join." + gjson.Escape(room.ID)) }
		stateIDs := func(section gjson.Result, key string) []string {
			var ids []string
			for _, ev := range section.Get(gjson.Escape(key) + ".events").Array() {
				ids = append(ids, ev.Get("event_id").String())
			}
			return ids
		}
		contains := func(ids []string, id string) bool {
			for _, i := range ids {
				if i == id {
					return true
				}
			}
			return false
		}

		// The room as it was before: everything but the topic and the name.
		before := room.Events()[:len(room.Events())-2]
		testrig.MustPublishMsgs(t, jsctx, toNATSMsgs(t, cfg, before...)...)
		first := waitFor(map[string]string{}, before[len(before)-1].EventID())
		since := first.Get("next_batch").String()

		// 1. A state event that took effect: it is in the timeline and, with state_after, also in state_after.
		testrig.MustPublishMsgs(t, jsctx, toNATSMsgs(t, cfg, topic)...)
		plain := waitFor(map[string]string{"since": since}, topic.EventID())
		if got := stateIDs(roomSection(plain), "state"); contains(got, topic.EventID()) {
			t.Errorf("without state_after the state event stays out of `state`, got %v", got)
		}
		if roomSection(plain).Get("state_after").Exists() {
			t.Error("state_after must only be present when asked for")
		}
		for name, tc := range map[string]struct{ param, field string }{
			"stable":   {"use_state_after", "state_after"},
			"unstable": {"org.matrix.msc4222.use_state_after", "org.matrix.msc4222.state_after"},
		} {
			res := waitFor(map[string]string{"since": since, tc.param: "true"}, topic.EventID())
			section := roomSection(res)
			if got := stateIDs(section, tc.field); !contains(got, topic.EventID()) {
				t.Errorf("%s: %s = %v, want it to include the new topic", name, tc.field, got)
			}
			if section.Get("state").Exists() {
				t.Errorf("%s: `state` must be omitted when state_after is used, got %s", name, section.Get("state").Raw)
			}
			other := "state_after"
			if tc.field == other {
				other = "org.matrix.msc4222.state_after"
			}
			if section.Get(gjson.Escape(other)).Exists() {
				t.Errorf("%s: only %s should be present", name, tc.field)
			}
		}
		since = plain.Get("next_batch").String()

		// 2. A state event that lost state resolution is in the timeline but not in the state: state_after is
		// present and empty, and the client must not apply the timeline event.
		testrig.MustPublishMsgs(t, jsctx, testrig.NewOutputEventMsg(t, cfg, lost.RoomID().String(), api.OutputEvent{
			Type:         rsapi.OutputTypeNewRoomEvent,
			NewRoomEvent: &rsapi.OutputNewRoomEvent{Event: lost, HistoryVisibility: lost.Visibility},
		}))
		res := waitFor(map[string]string{"since": since, "use_state_after": "true"}, lost.EventID())
		section := roomSection(res)
		if !section.Get("state_after").Exists() {
			t.Fatalf("state_after must be present even when empty, got %s", section.Raw)
		}
		if got := stateIDs(section, "state_after"); len(got) != 0 {
			t.Errorf("a state event that did not take effect must not be in state_after, got %v", got)
		}
		if !section.Get("state_after.events").IsArray() {
			t.Errorf("state_after.events must be an array, got %s", section.Get("state_after").Raw)
		}

		// 3. An initial sync gives the whole state at the end of the timeline, including state events that are in the timeline.
		full := sync(map[string]string{"use_state_after": "true"})
		section = roomSection(full)
		if got := stateIDs(section, "state_after"); !contains(got, topic.EventID()) || !contains(got, room.Events()[0].EventID()) || contains(got, lost.EventID()) {
			t.Errorf("initial state_after = %v, want the create event and the topic, not the event that lost", got)
		}
		if section.Get("state").Exists() {
			t.Errorf("`state` must be omitted, got %s", section.Get("state").Raw)
		}
		// Without state_after, state events of the timeline are left out of `state`: the topic is in the timeline.
		legacy := sync(map[string]string{})
		if got := stateIDs(roomSection(legacy), "state"); contains(got, topic.EventID()) {
			t.Errorf("without state_after the state events of the timeline stay out of `state`, got %v", got)
		}
		if legacy.Get("rooms.join").Get(gjson.Escape(room.ID)).Get("state_after").Exists() {
			t.Error("state_after must only be present when asked for")
		}
	})
}

func copyQuery(q map[string]string) map[string]string {
	out := make(map[string]string, len(q)+2)
	for k, v := range q {
		out[k] = v
	}
	return out
}
