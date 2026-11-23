package clientapi

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/clientapi/routing"
	rsapi "github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func enableDelayed(cfg *config.Dendrite) {
	cfg.ClientAPI.DelayedEvents.Enabled = true
	cfg.ClientAPI.DelayedEvents.MaxDelay = time.Minute
	cfg.ClientAPI.DelayedEvents.MaxPerUser = 10
}

// waitFor polls until the condition holds, for a few seconds.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

func TestDelayedEventsAreCancelledWhenInvalid(t *testing.T) {
	alice := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	carol := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	dave := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, enableDelayed, alice, bob, carol, dave)
		defer closeFn()
		ctx := context.Background()

		room := test.NewRoom(t, alice)
		for _, u := range []*test.User{bob, carol} {
			room.CreateAndInsert(t, u, "m.room.member", map[string]any{"membership": "join"}, test.WithStateKey(u.ID))
		}
		h.sendRoom(room)

		schedule := func(u *test.User, key string) string {
			t.Helper()
			code, res := h.call(h.routers.Client, u, http.MethodPut,
				"/_matrix/client/v3/rooms/"+room.ID+"/state/org.matrix.msc3401.call.member/"+key+"?org.matrix.msc4140.delay=30000",
				map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			return res.Get("delay_id").String()
		}
		waiting := func(u *test.User) int {
			events, err := h.userAPI.DelayedEventsForUser(ctx, u.ID)
			require.NoError(t, err)
			return len(events)
		}

		t.Run("the user leaves the room", func(t *testing.T) {
			schedule(bob, "_bob_A")
			schedule(carol, "_carol_A")
			leave := room.CreateAndInsert(t, bob, "m.room.member", map[string]any{"membership": "leave"}, test.WithStateKey(bob.ID))
			require.NoError(t, rsapi.SendEvents(ctx, h.rsAPI, rsapi.KindNew, []*rstypes.HeaderedEvent{leave}, "test", "test", "test", nil, false))
			assert.True(t, waitFor(func() bool { return waiting(bob) == 0 }), "bob's delayed event goes with his leave")
			assert.Equal(t, 1, waiting(carol), "the one of somebody else stays")
		})

		t.Run("the user is banned", func(t *testing.T) {
			ban := room.CreateAndInsert(t, alice, "m.room.member", map[string]any{"membership": "ban"}, test.WithStateKey(carol.ID))
			require.NoError(t, rsapi.SendEvents(ctx, h.rsAPI, rsapi.KindNew, []*rstypes.HeaderedEvent{ban}, "test", "test", "test", nil, false))
			assert.True(t, waitFor(func() bool { return waiting(carol) == 0 }), "and so does the one of somebody who is banned")
		})

		t.Run("the device logs out", func(t *testing.T) {
			room2 := test.NewRoom(t, dave)
			h.sendRoom(room2)
			code, res := h.call(h.routers.Client, dave, http.MethodPut,
				"/_matrix/client/v3/rooms/"+room2.ID+"/state/org.matrix.msc3401.call.member/_dave?org.matrix.msc4140.delay=30000", map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			events, err := h.userAPI.DelayedEventsForUser(ctx, dave.ID)
			require.NoError(t, err)
			require.Len(t, events, 1)
			code, res = h.call(h.routers.Client, dave, http.MethodPost, "/_matrix/client/v3/logout", map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			events, err = h.userAPI.DelayedEventsForUser(ctx, dave.ID)
			require.NoError(t, err)
			assert.Empty(t, events, "a device that is gone cannot keep a call member alive")
		})

		t.Run("the room is purged", func(t *testing.T) {
			schedule(alice, "_alice_A")
			require.Equal(t, 1, waiting(alice))
			require.NoError(t, h.rsAPI.PerformAdminPurgeRoom(ctx, room.ID))
			assert.True(t, waitFor(func() bool { return waiting(alice) == 0 }))
		})
	})
}

func TestDelayedEventsAreCancelledOnDeactivation(t *testing.T) {
	alice := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, enableDelayed, alice)
		defer closeFn()
		ctx := context.Background()
		empty := ""
		require.NoError(t, h.userAPI.InsertDelayedEvent(ctx, uapi.DelayedEvent{
			DelayID: "x", UserID: alice.ID, DeviceID: "gone", RoomID: "!r:test", EventType: "m.room.topic", StateKey: &empty,
			Content: []byte(`{}`), DelayMS: 60000, RunningSinceMS: time.Now().UnixMilli(),
		}))
		localpart, server, _ := gomatrixserverlib.SplitID('@', alice.ID)
		require.NoError(t, h.userAPI.PerformAccountDeactivation(ctx, &uapi.PerformAccountDeactivationRequest{Localpart: localpart, ServerName: server}, &uapi.PerformAccountDeactivationResponse{}))
		events, err := h.userAPI.DelayedEventsForUser(ctx, alice.ID)
		require.NoError(t, err)
		assert.Empty(t, events)
	})
}

// flakyRoomserver fails the first n submissions of events, as a roomserver that has a bad moment.
type flakyRoomserver struct {
	rsapi.ClientRoomserverAPI
	failures atomic.Int32
	calls    atomic.Int32
}

func (f *flakyRoomserver) InputRoomEvents(ctx context.Context, req *rsapi.InputRoomEventsRequest, res *rsapi.InputRoomEventsResponse) {
	f.calls.Add(1)
	if f.failures.Add(-1) >= 0 {
		res.ErrMsg = "the roomserver is busy"
		return
	}
	f.ClientRoomserverAPI.InputRoomEvents(ctx, req, res)
}

func TestDelayedEventsAreRetriedAfterAServerFailure(t *testing.T) {
	alice := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, enableDelayed, alice)
		defer closeFn()
		ctx := context.Background()
		room := test.NewRoom(t, alice)
		h.sendRoom(room)
		flaky := &flakyRoomserver{ClientRoomserverAPI: h.rsAPI}
		flaky.failures.Store(1)
		empty := ""
		require.NoError(t, h.userAPI.InsertDelayedEvent(ctx, uapi.DelayedEvent{
			DelayID: "retry-me", UserID: alice.ID, DeviceID: "D", RoomID: room.ID, EventType: "m.room.topic", StateKey: &empty,
			Content: []byte(`{"topic":"after a failure"}`), DelayMS: 100, RunningSinceMS: time.Now().UnixMilli(),
		}))
		d := routing.NewDelayedEvents(&h.cfg.ClientAPI, h.userAPI, flaky)
		require.NoError(t, d.Start(ctx))

		var tuple = gomatrixserverlib.StateKeyTuple{EventType: "m.room.topic", StateKey: ""}
		topic := func() string {
			var res rsapi.QueryCurrentStateResponse
			require.NoError(t, h.rsAPI.QueryCurrentState(ctx, &rsapi.QueryCurrentStateRequest{RoomID: room.ID, StateTuples: []gomatrixserverlib.StateKeyTuple{tuple}}, &res))
			if ev := res.StateEvents[tuple]; ev != nil {
				return gjson.GetBytes(ev.Content(), "topic").String()
			}
			return ""
		}
		assert.True(t, waitFor(func() bool { return topic() == "after a failure" }), "the event was sent by the second try")
		assert.GreaterOrEqual(t, int(flaky.calls.Load()), 2)
		assert.True(t, waitFor(func() bool {
			e, err := h.userAPI.DelayedEvent(ctx, "retry-me")
			return err == nil && e == nil
		}), "and is removed once it is sent")
	})
}

func TestDelayedEventsAdminView(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, enableDelayed, admin, bob)
		defer closeFn()
		room := test.NewRoom(t, bob)
		h.sendRoom(room)
		code, res := h.call(h.routers.Client, bob, http.MethodPut,
			"/_matrix/client/v3/rooms/"+room.ID+"/state/m.room.topic/?org.matrix.msc4140.delay=30000", map[string]any{"topic": "x"})
		require.Equal(t, http.StatusOK, code, res.Raw)
		id := res.Get("delay_id").String()

		code, _ = h.call(h.routers.SynapseAdmin, bob, http.MethodGet, "/_synapse/admin/v1/delayed_events", nil)
		assert.Equal(t, http.StatusForbidden, code)
		code, res = h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/delayed_events", nil)
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, int64(1), res.Get("total").Int())
		assert.Equal(t, bob.ID, res.Get("delayed_events.0.user_id").String())
		code, res = h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/delayed_events?user_id="+admin.ID, nil)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(0), res.Get("total").Int())

		code, _ = h.call(h.routers.SynapseAdmin, admin, http.MethodDelete, "/_synapse/admin/v1/delayed_events/"+id, nil)
		assert.Equal(t, http.StatusOK, code)
		code, _ = h.call(h.routers.SynapseAdmin, admin, http.MethodDelete, "/_synapse/admin/v1/delayed_events/"+id, nil)
		assert.Equal(t, http.StatusNotFound, code)
		_, res = h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/delayed_events", nil)
		assert.Equal(t, int64(0), res.Get("total").Int())
	})
}
