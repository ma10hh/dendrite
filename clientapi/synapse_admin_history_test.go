package clientapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	rsapi "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/test"
	uapi "github.com/element-hq/dendrite/userapi/api"
)

func TestSynapseAdminPurgeHistory(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, nil, admin, bob)
		defer closeFn()
		h.startSyncAPI()

		now := time.Now()
		room := test.NewRoom(t, bob)
		old1 := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "old 1"}, test.WithTimestamp(now.Add(-48*time.Hour)))
		old2 := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "old 2"}, test.WithTimestamp(now.Add(-47*time.Hour)))
		recent := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "recent"}, test.WithTimestamp(now))
		h.sendRoom(room)

		inTimeline := func(eventID string) bool {
			sync := h.rsAPI.SyncAdmin()
			if sync == nil {
				return false
			}
			_, err := sync.EventTimestamp(h.ctx, room.ID, eventID)
			return err == nil
		}
		require.True(t, waitFor(func() bool { return inTimeline(recent.EventID()) && inTimeline(old1.EventID()) }), "the sync API has the room")

		purge := func(path string, body any) (int, gjson.Result) {
			return h.call(h.routers.SynapseAdmin, admin, http.MethodPost, "/_synapse/admin/v1/purge_history/"+path, body)
		}
		awaitDone := func(id string) string {
			var status string
			waitFor(func() bool {
				_, res := h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/purge_history_status/"+id, nil)
				status = res.Get("status").String()
				return status != "active"
			})
			return status
		}
		cutoff := now.Add(-time.Hour).UnixMilli()

		code, _ := h.call(h.routers.SynapseAdmin, bob, http.MethodPost, "/_synapse/admin/v1/purge_history/"+room.ID, map[string]any{"purge_up_to_ts": cutoff})
		assert.Equal(t, http.StatusForbidden, code)
		code, _ = purge(room.ID, map[string]any{})
		assert.Equal(t, http.StatusBadRequest, code, "neither a time nor an event")
		code, _ = purge(room.ID, map[string]any{"purge_up_to_ts": cutoff, "purge_up_to_event_id": recent.EventID()})
		assert.Equal(t, http.StatusBadRequest, code, "both")
		code, _ = purge("!nobody:test", map[string]any{"purge_up_to_ts": cutoff})
		assert.Equal(t, http.StatusNotFound, code, "unknown room")
		code, _ = purge(room.ID+"/$unknown", nil)
		assert.Equal(t, http.StatusNotFound, code, "unknown event")
		code, _ = h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/purge_history_status/nope", nil)
		assert.Equal(t, http.StatusNotFound, code)

		// As in Synapse the events of local users stay, unless asked.
		code, res := purge(room.ID, map[string]any{"purge_up_to_ts": cutoff})
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "complete", awaitDone(res.Get("purge_id").String()))
		assert.True(t, inTimeline(old1.EventID()), "a local event is kept by default")

		code, res = purge(room.ID, map[string]any{"purge_up_to_ts": cutoff, "delete_local_events": true})
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "complete", awaitDone(res.Get("purge_id").String()))
		assert.False(t, inTimeline(old1.EventID()))
		assert.False(t, inTimeline(old2.EventID()))
		assert.True(t, inTimeline(recent.EventID()), "newer than the cutoff")
		createEvent := room.Events()[0]
		assert.True(t, inTimeline(createEvent.EventID()), "state events stay")

		// Up to an event: what is before it, by time.
		code, res = purge(room.ID+"/"+recent.EventID(), map[string]any{"delete_local_events": true})
		require.Equal(t, http.StatusOK, code, res.Raw)
		assert.Equal(t, "complete", awaitDone(res.Get("purge_id").String()))
		assert.True(t, inTimeline(recent.EventID()), "the event itself stays")
	})
}

func TestSynapseAdminRedactUser(t *testing.T) {
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	bob := test.NewUser(t, test.WithAccountType(uapi.AccountTypeUser))
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h, closeFn := newAdminHarness(t, dbType, nil, admin, bob)
		defer closeFn()
		h.startSyncAPI()

		room1, room2 := test.NewRoom(t, bob), test.NewRoom(t, bob)
		m1 := room1.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam 1"})
		m2 := room1.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam 2"})
		m3 := room2.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam 3"})
		h.sendRoom(room1)
		h.sendRoom(room2)

		redacted := func(roomID, eventID string) bool {
			var res rsapi.QueryEventsByIDResponse
			require.NoError(t, h.rsAPI.QueryEventsByID(h.ctx, &rsapi.QueryEventsByIDRequest{RoomID: roomID, EventIDs: []string{eventID}}, &res))
			return len(res.Events) == 1 && gjson.GetBytes(res.Events[0].Unsigned(), "redacted_because").Exists()
		}
		listed := func() bool {
			sync := h.rsAPI.SyncAdmin()
			if sync == nil {
				return false
			}
			evs, err := sync.EventsBySender(h.ctx, bob.ID, 0, 100)
			return err == nil && len(evs) >= 3
		}
		require.True(t, waitFor(listed), "the sync API has the events")
		path := "/_synapse/admin/v1/user/" + bob.ID + "/redact"

		code, _ := h.call(h.routers.SynapseAdmin, bob, http.MethodPost, path, map[string]any{})
		assert.Equal(t, http.StatusForbidden, code)
		code, _ = h.call(h.routers.SynapseAdmin, admin, http.MethodPost, "/_synapse/admin/v1/user/@someone:elsewhere.example/redact", map[string]any{})
		assert.Equal(t, http.StatusBadRequest, code, "local users only")
		code, _ = h.call(h.routers.SynapseAdmin, admin, http.MethodPost, path, map[string]any{"limit": 0})
		assert.Equal(t, http.StatusBadRequest, code)
		code, _ = h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/user/redact_status/nope", nil)
		assert.Equal(t, http.StatusNotFound, code)

		status := func(id string) gjson.Result {
			var res gjson.Result
			waitFor(func() bool {
				_, res = h.call(h.routers.SynapseAdmin, admin, http.MethodGet, "/_synapse/admin/v1/user/redact_status/"+id, nil)
				s := res.Get("status").String()
				return s == "completed" || s == "failed"
			})
			return res
		}

		// Only the rooms asked for.
		code, res := h.call(h.routers.SynapseAdmin, admin, http.MethodPost, path, map[string]any{"rooms": []string{room1.ID}, "reason": "spam"})
		require.Equal(t, http.StatusOK, code, res.Raw)
		st := status(res.Get("redact_id").String())
		assert.Equal(t, "completed", st.Get("status").String(), st.Raw)
		assert.Empty(t, st.Get("failed_redactions").Map(), st.Raw)
		assert.True(t, waitFor(func() bool { return redacted(room1.ID, m1.EventID()) && redacted(room1.ID, m2.EventID()) }))
		assert.False(t, redacted(room2.ID, m3.EventID()), "the other room is not touched")

		// Everything, with a limit.
		code, res = h.call(h.routers.SynapseAdmin, admin, http.MethodPost, path, map[string]any{})
		require.Equal(t, http.StatusOK, code, res.Raw)
		st = status(res.Get("redact_id").String())
		assert.Equal(t, "completed", st.Get("status").String(), st.Raw)
		assert.True(t, waitFor(func() bool { return redacted(room2.ID, m3.EventID()) }))
	})
}
