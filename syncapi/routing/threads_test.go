package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

type threadsRS struct {
	api.SyncRoomserverAPI
	// membershipAt tells what the membership of the requesting user was at an event. Nil means: joined.
	membershipAt func(eventID string) string
	joinEvent    *rstypes.HeaderedEvent
	leaveEvent   *rstypes.HeaderedEvent
	forgotten    bool
	roomExists   bool
	inRoom       bool
	beenInRoom   bool
}

func (r *threadsRS) QueryUserIDForSender(_ context.Context, _ spec.RoomID, s spec.SenderID) (*spec.UserID, error) {
	return spec.NewUserID(string(s), true)
}

func (r *threadsRS) QuerySenderIDForUser(_ context.Context, _ spec.RoomID, u spec.UserID) (*spec.SenderID, error) {
	s := spec.SenderID(u.String())
	return &s, nil
}

func (r *threadsRS) QueryMembershipForUser(_ context.Context, _ *api.QueryMembershipForUserRequest, res *api.QueryMembershipForUserResponse) error {
	res.RoomExists, res.IsRoomForgotten, res.IsInRoom, res.HasBeenInRoom = r.roomExists, r.forgotten, r.inRoom, r.beenInRoom
	return nil
}

func (r *threadsRS) QueryMembershipAtEvent(_ context.Context, _ spec.RoomID, eventIDs []string, _ spec.SenderID) (map[string]*rstypes.HeaderedEvent, error) {
	out := map[string]*rstypes.HeaderedEvent{}
	for _, id := range eventIDs {
		if r.membershipAt != nil && r.membershipAt(id) == spec.Leave {
			out[id] = r.leaveEvent
		} else {
			out[id] = r.joinEvent
		}
	}
	return out, nil
}

type threadsHarness struct {
	t      *testing.T
	db     storage.Database
	rs     *threadsRS
	alice  *test.User
	bob    *test.User
	carol  *test.User
	room   *test.Room
	device *userapi.Device
}

func newThreadsHarness(t *testing.T, dbType test.DBType) *threadsHarness {
	t.Helper()
	connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
	t.Cleanup(closeDB)
	cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
	db, err := storage.NewSyncServerDatasource(context.Background(), cm, &config.DatabaseOptions{ConnectionString: config.DataSource(connStr)})
	if err != nil {
		t.Fatalf("NewSyncServerDatasource: %s", err)
	}
	h := &threadsHarness{t: t, db: db, alice: test.NewUser(t), bob: test.NewUser(t), carol: test.NewUser(t)}
	h.room = test.NewRoom(t, h.alice)
	for _, u := range []*test.User{h.bob, h.carol} {
		h.room.CreateAndInsert(t, u, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(u.ID))
	}
	h.device = &userapi.Device{UserID: h.alice.ID, ID: "A"}
	h.rs = &threadsRS{roomExists: true, inRoom: true, beenInRoom: true}
	for _, ev := range h.room.Events() {
		h.write(ev, gomatrixserverlib.HistoryVisibilityShared)
		if ev.Type() == spec.MRoomMember && ev.StateKeyEquals(h.alice.ID) {
			h.rs.joinEvent = ev
		}
	}
	// The membership event that the roomserver would give for the times alice was not in the room. It comes from
	// another room so that alice stays in this one.
	h.rs.leaveEvent = test.NewRoom(t, h.alice).CreateAndInsert(t, h.alice, spec.MRoomMember, map[string]string{"membership": "leave"}, test.WithStateKey(h.alice.ID))
	return h
}

func (h *threadsHarness) write(ev *rstypes.HeaderedEvent, vis gomatrixserverlib.HistoryVisibility) {
	h.t.Helper()
	sender, err := spec.NewUserID(string(ev.SenderID()), true)
	if err != nil {
		h.t.Fatal(err)
	}
	ev.UserID = *sender
	var add []*rstypes.HeaderedEvent
	var addIDs []string
	if ev.StateKey() != nil {
		ev.StateKeyResolved = ev.StateKey()
		add, addIDs = []*rstypes.HeaderedEvent{ev}, []string{ev.EventID()}
	}
	if _, err = h.db.WriteEvent(context.Background(), ev, add, addIDs, nil, nil, false, vis); err != nil {
		h.t.Fatalf("WriteEvent: %s", err)
	}
	if err = h.db.UpdateRelations(context.Background(), ev); err != nil {
		h.t.Fatalf("UpdateRelations: %s", err)
	}
}

func (h *threadsHarness) message(sender *test.User, body string, relatesTo *rstypes.HeaderedEvent) *rstypes.HeaderedEvent {
	h.t.Helper()
	return h.messageWithVisibility(sender, body, relatesTo, gomatrixserverlib.HistoryVisibilityShared)
}

func (h *threadsHarness) messageWithVisibility(sender *test.User, body string, relatesTo *rstypes.HeaderedEvent, vis gomatrixserverlib.HistoryVisibility) *rstypes.HeaderedEvent {
	h.t.Helper()
	content := map[string]any{"msgtype": "m.text", "body": body}
	if relatesTo != nil {
		content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": relatesTo.EventID()}
	}
	ev := h.room.CreateAndInsert(h.t, sender, "m.room.message", content)
	h.write(ev, vis)
	return ev
}

func (h *threadsHarness) get(query string) (int, gjson.Result) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+h.room.ID+"/threads"+query, nil)
	res := Threads(req, h.device, h.db, h.rs, h.room.ID)
	b, err := json.Marshal(res.JSON)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Code, gjson.ParseBytes(b)
}

func rootIDs(res gjson.Result) []string {
	var ids []string
	for _, ev := range res.Get("chunk").Array() {
		ids = append(ids, ev.Get("event_id").String())
	}
	return ids
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestThreadsList(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newThreadsHarness(t, dbType)
		root1, root2, root3 := h.message(h.alice, "root 1", nil), h.message(h.bob, "root 2", nil), h.message(h.bob, "root 3", nil)
		h.message(h.bob, "plain message", nil)
		h.message(h.bob, "reply 1a", root1)
		h.message(h.carol, "reply 3a", root3)
		reply2a := h.message(h.carol, "reply 2a", root2)
		reply1b := h.message(h.bob, "reply 1b", root1)
		reply2b := h.message(h.alice, "reply 2b", root2)

		code, res := h.get("")
		if code != http.StatusOK {
			t.Fatalf("got %d %s", code, res.Raw)
		}
		if got, want := rootIDs(res), []string{root2.EventID(), root1.EventID(), root3.EventID()}; !equalIDs(got, want) {
			t.Errorf("order: got %v, want %v (by the latest reply, newest first)", got, want)
		}
		thread := func(root *rstypes.HeaderedEvent) gjson.Result {
			for _, ev := range res.Get("chunk").Array() {
				if ev.Get("event_id").String() == root.EventID() {
					return ev.Get("unsigned").Get(`m\.relations`).Get(`m\.thread`)
				}
			}
			return gjson.Result{}
		}
		for root, want := range map[*rstypes.HeaderedEvent]struct {
			count        int64
			participated bool
			latest       string
		}{
			root1: {2, true, reply1b.EventID()}, root2: {2, true, reply2b.EventID()}, root3: {1, false, ""},
		} {
			agg := thread(root)
			if !agg.Exists() {
				t.Errorf("%s: no bundled thread aggregation", root.EventID())
				continue
			}
			if agg.Get("count").Int() != want.count || agg.Get("current_user_participated").Bool() != want.participated {
				t.Errorf("%s: %s, want count %d participated %v", root.EventID(), agg.Raw, want.count, want.participated)
			}
			if want.latest != "" && agg.Get("latest_event.event_id").String() != want.latest {
				t.Errorf("%s: latest event %s, want %s", root.EventID(), agg.Get("latest_event.event_id").String(), want.latest)
			}
		}
		_ = reply2a
		if got := res.Get("chunk.0.content.body").String(); got != "root 2" {
			t.Errorf("the chunk holds the roots, got %q", got)
		}
		if res.Get("next_batch").Exists() {
			t.Error("there is no next page")
		}

		// Only the threads the user took part in.
		_, res = h.get("?include=participated")
		if got, want := rootIDs(res), []string{root2.EventID(), root1.EventID()}; !equalIDs(got, want) {
			t.Errorf("participated: got %v, want %v", got, want)
		}

		// Paging.
		_, first := h.get("?limit=2")
		if got, want := rootIDs(first), []string{root2.EventID(), root1.EventID()}; !equalIDs(got, want) || !first.Get("next_batch").Exists() {
			t.Fatalf("first page: %v next_batch %v", got, first.Get("next_batch").Raw)
		}
		_, second := h.get("?limit=2&from=" + first.Get("next_batch").String())
		if got, want := rootIDs(second), []string{root3.EventID()}; !equalIDs(got, want) || second.Get("next_batch").Exists() {
			t.Errorf("second page: %v, next_batch %v", got, second.Get("next_batch").Raw)
		}

		for _, bad := range []string{"?include=nope", "?limit=0", "?limit=x", "?from=abc", "?from=-1"} {
			if code, _ := h.get(bad); code != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400", bad, code)
			}
		}
		if code, res := h.get("?limit=1000"); code != http.StatusOK || len(res.Get("chunk").Array()) != 3 {
			t.Errorf("a large limit is capped, not refused: %d %s", code, res.Raw)
		}
	})
}

func TestThreadsAccess(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newThreadsHarness(t, dbType)
		root := h.message(h.bob, "root", nil)
		h.message(h.carol, "reply", root)

		h.rs.roomExists = false
		if code, _ := h.get(""); code != http.StatusForbidden {
			t.Errorf("unknown room: got %d, want 403", code)
		}
		h.rs.roomExists, h.rs.forgotten = true, true
		if code, _ := h.get(""); code != http.StatusForbidden {
			t.Errorf("forgotten room: got %d, want 403", code)
		}
		h.rs.forgotten, h.rs.inRoom, h.rs.beenInRoom = false, false, false
		if code, res := h.get(""); code != http.StatusForbidden {
			t.Errorf("stranger, room not world readable: got %d %s, want 403", code, res.Raw)
		}
		// A room that anyone may read.
		readable := h.room.CreateAndInsert(t, h.alice, spec.MRoomHistoryVisibility, map[string]string{"history_visibility": "world_readable"}, test.WithStateKey(""))
		h.write(readable, gomatrixserverlib.HistoryVisibilityWorldReadable)
		if code, res := h.get(""); code != http.StatusOK || len(res.Get("chunk").Array()) != 1 {
			t.Errorf("stranger, world readable room: got %d %s", code, res.Raw)
		}
	})
}

func TestThreadsHistoryVisibilityAndIgnoredUsers(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newThreadsHarness(t, dbType)
		root := h.message(h.bob, "root by bob", nil)
		early := h.message(h.carol, "early reply", root)
		late := h.message(h.bob, "late reply by bob", root)
		other := h.message(h.carol, "other root", nil)
		h.message(h.carol, "other reply", other)

		// Alice has the room with the most recent reply by an ignored user bob: the thread keeps its place, but shows carol's reply.
		if err := h.db.UpdateIgnoresForUser(context.Background(), h.alice.ID, &types.IgnoredUsers{List: map[string]interface{}{h.bob.ID: struct{}{}}}); err != nil {
			t.Fatal(err)
		}
		_, res := h.get("")
		if got, want := rootIDs(res), []string{other.EventID(), root.EventID()}; !equalIDs(got, want) {
			t.Fatalf("order %v, want %v", got, want)
		}
		var ignoredThread gjson.Result
		for _, ev := range res.Get("chunk").Array() {
			if ev.Get("event_id").String() == root.EventID() {
				ignoredThread = ev
			}
		}
		if got := ignoredThread.Get("content.body"); got.Exists() {
			t.Errorf("the root of an ignored user is redacted, got content %s", ignoredThread.Get("content").Raw)
		}
		agg := ignoredThread.Get("unsigned").Get(`m\.relations`).Get(`m\.thread`)
		if got := agg.Get("latest_event.event_id").String(); got != early.EventID() {
			t.Errorf("latest event %s, want carol's reply %s, not %s by the ignored user", got, early.EventID(), late.EventID())
		}
		if agg.Get("count").Int() != 2 {
			t.Errorf("the count stays as it is: %s", agg.Raw)
		}
		if err := h.db.UpdateIgnoresForUser(context.Background(), h.alice.ID, &types.IgnoredUsers{List: map[string]interface{}{}}); err != nil {
			t.Fatal(err)
		}

		// Alice left after the early reply and the room only shows later events to those who are joined.
		later := h.messageWithVisibility(h.carol, "reply after alice left", root, gomatrixserverlib.HistoryVisibilityJoined)
		lonely := h.message(h.carol, "root with only hidden replies", nil)
		secret := h.messageWithVisibility(h.carol, "reply that is hidden", lonely, gomatrixserverlib.HistoryVisibilityJoined)
		h.rs.membershipAt = func(id string) string {
			if id == later.EventID() || id == secret.EventID() {
				return spec.Leave
			}
			return spec.Join
		}
		_, res = h.get("")
		if got, want := rootIDs(res), []string{root.EventID(), other.EventID()}; !equalIDs(got, want) {
			t.Fatalf("visibility: got %v, want %v: the thread whose only reply alice cannot see is left out", got, want)
		}
		agg = res.Get("chunk.0.unsigned").Get(`m\.relations`).Get(`m\.thread`)
		if got := agg.Get("latest_event.event_id").String(); got != late.EventID() {
			t.Errorf("latest visible reply %s, want %s, the newest one before alice left (not %s)", got, late.EventID(), later.EventID())
		}
	})
}
