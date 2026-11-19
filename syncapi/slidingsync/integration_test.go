// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	rsapi "github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/notifier"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

var bg = context.Background()

type fakeRoomserver struct {
	rsapi.SyncRoomserverAPI
	// membershipAt maps an event ID to the requesting user's membership event at that event.
	// Events that are missing are treated as "not a member", as the real roomserver does.
	membershipAt map[string]*rstypes.HeaderedEvent
	// members lists the joined users of the rooms the user is in, which TrackChangedUsers asks about.
	members map[string][]string
}

func (f *fakeRoomserver) QueryUserIDForSender(_ context.Context, _ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	return spec.NewUserID(string(senderID), true)
}

func (f *fakeRoomserver) QuerySenderIDForUser(_ context.Context, _ spec.RoomID, userID spec.UserID) (*spec.SenderID, error) {
	senderID := spec.SenderID(userID.String())
	return &senderID, nil
}

func (f *fakeRoomserver) QueryMembershipAtEvent(_ context.Context, _ spec.RoomID, eventIDs []string, _ spec.SenderID) (map[string]*rstypes.HeaderedEvent, error) {
	out := map[string]*rstypes.HeaderedEvent{}
	for _, id := range eventIDs {
		if ev, ok := f.membershipAt[id]; ok {
			out[id] = ev
		}
	}
	return out, nil
}

type fakeUserAPI struct {
	userapi.SyncUserAPI
	direct map[string][]string
	// global and room are account data other than m.direct.
	global map[string]json.RawMessage
	room   map[string]map[string]json.RawMessage
	// otk and fallback are the device's one-time key counts and unused fallback key types.
	otk      map[string]int
	fallback []string
	// keyChanges are the users whose keys changed, as of keyOffset.
	keyChanges []string
	keyOffset  int64
}

func (f *fakeUserAPI) PerformLastSeenUpdate(context.Context, *userapi.PerformLastSeenUpdateRequest, *userapi.PerformLastSeenUpdateResponse) error {
	return nil
}

func (f *fakeUserAPI) QueryAccountData(_ context.Context, _ *userapi.QueryAccountDataRequest, res *userapi.QueryAccountDataResponse) error {
	res.GlobalAccountData = map[string]json.RawMessage{}
	for dataType, content := range f.global {
		res.GlobalAccountData[dataType] = content
	}
	if f.direct != nil {
		b, _ := json.Marshal(f.direct)
		res.GlobalAccountData["m.direct"] = b
	}
	res.RoomAccountData = f.room
	return nil
}

func (f *fakeUserAPI) QueryOneTimeKeys(_ context.Context, _ *userapi.QueryOneTimeKeysRequest, res *userapi.QueryOneTimeKeysResponse) error {
	res.Count.KeyCount = f.otk
	res.UnusedFallbackAlgorithms = f.fallback
	return nil
}

func (f *fakeUserAPI) QueryKeyChanges(_ context.Context, req *userapi.QueryKeyChangesRequest, res *userapi.QueryKeyChangesResponse) error {
	res.Offset = f.keyOffset
	if req.Offset < f.keyOffset {
		res.UserIDs = f.keyChanges
	}
	return nil
}

type harness struct {
	t          *testing.T
	notifier   *notifier.Notifier
	typing     *caching.EDUCache
	shutdown   context.Context
	stopServer context.CancelFunc
	db         storage.Database
	rs         *fakeRoomserver
	users      *fakeUserAPI
	handle     *Handler
	alice      *test.User
	device     *userapi.Device
}

func newHarness(t *testing.T, dbType test.DBType) *harness {
	t.Helper()
	connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
	t.Cleanup(closeDB)
	cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
	db, err := storage.NewSyncServerDatasource(bg, cm, &config.DatabaseOptions{ConnectionString: config.DataSource(connStr)})
	if err != nil {
		t.Fatalf("NewSyncServerDatasource: %s", err)
	}
	hn := &harness{
		t: t, db: db,
		rs:    &fakeRoomserver{membershipAt: map[string]*rstypes.HeaderedEvent{}},
		users: &fakeUserAPI{},
		alice: test.NewUser(t),
	}
	hn.device = &userapi.Device{UserID: hn.alice.ID, ID: "DEVICE"}
	hn.notifier = notifier.NewNotifier(hn.rs)
	hn.notifier.SetCurrentPosition(types.StreamingToken{PDUPosition: 1})
	hn.typing = caching.NewTypingCache()
	hn.shutdown, hn.stopServer = context.WithCancel(context.Background())
	t.Cleanup(hn.stopServer)
	hn.handle = NewHandler(NewConnectionStore(time.Hour, 10), db, hn.users, hn.rs, hn.notifier, hn.typing, nil, hn.shutdown, false)
	return hn
}

// write stores events the way the roomserver consumer would. Events default to shared history visibility.
func (hn *harness) write(events ...*rstypes.HeaderedEvent) {
	hn.t.Helper()
	hn.writeWithVisibility(gomatrixserverlib.HistoryVisibilityShared, events...)
}

func (hn *harness) writeWithVisibility(vis gomatrixserverlib.HistoryVisibility, events ...*rstypes.HeaderedEvent) {
	hn.t.Helper()
	for _, ev := range events {
		sender, err := spec.NewUserID(string(ev.SenderID()), true)
		if err != nil {
			hn.t.Fatalf("sender %q: %s", ev.SenderID(), err)
		}
		ev.UserID = *sender // the consumer does this; the sender column is filled from it
		var addState []*rstypes.HeaderedEvent
		var addStateIDs []string
		if ev.StateKey() != nil {
			ev.StateKeyResolved = ev.StateKey()
			addState = append(addState, ev)
			addStateIDs = append(addStateIDs, ev.EventID())
		}
		pos, err := hn.db.WriteEvent(bg, ev, addState, addStateIDs, nil, nil, false, vis)
		if err != nil {
			hn.t.Fatalf("WriteEvent: %s", err)
		}
		hn.notifier.OnNewEvent(nil, "", []string{hn.alice.ID}, types.StreamingToken{PDUPosition: pos})
	}
}

func message(t *testing.T, room *test.Room, sender *test.User, body string) *rstypes.HeaderedEvent {
	t.Helper()
	return room.CreateAndInsert(t, sender, "m.room.message", map[string]string{"msgtype": "m.text", "body": body})
}

// invite stores an invite the way the consumer does: UserID is the invited user, not the sender.
func (hn *harness) invite(ev *rstypes.HeaderedEvent) {
	hn.t.Helper()
	target, err := spec.NewUserID(*ev.StateKey(), true)
	if err != nil {
		hn.t.Fatalf("invite target: %s", err)
	}
	ev.UserID = *target
	if _, err := hn.db.AddInviteEvent(bg, ev); err != nil {
		hn.t.Fatalf("AddInviteEvent: %s", err)
	}
}

// post sends a request and returns the HTTP status and the JSON body of the response.
func (hn *harness) post(pos, body string) (int, gjson.Result) {
	hn.t.Helper()
	target := "/sync"
	if pos != "" {
		target += "?pos=" + url.QueryEscape(pos)
	}
	res := hn.handle.OnIncomingRequest(httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)), hn.device)
	b, err := json.Marshal(res.JSON)
	if err != nil {
		hn.t.Fatalf("marshalling response: %s", err)
	}
	return res.Code, gjson.ParseBytes(b)
}

func (hn *harness) mustPost(pos, body string) gjson.Result {
	hn.t.Helper()
	code, res := hn.post(pos, body)
	if code != http.StatusOK {
		hn.t.Fatalf("got HTTP %d: %s", code, res.Raw)
	}
	return res
}

func roomIDs(res gjson.Result) map[string]bool {
	out := map[string]bool{}
	for id := range res.Get("rooms").Map() {
		out[id] = true
	}
	return out
}

func eventIDs(room gjson.Result, field string) []string {
	var out []string
	for _, ev := range room.Get(field).Array() {
		out = append(out, ev.Get("event_id").String())
	}
	return out
}

func assertRooms(t *testing.T, res gjson.Result, want ...string) {
	t.Helper()
	got := roomIDs(res)
	if len(got) != len(want) {
		t.Fatalf("got rooms %v, want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("got rooms %v, want %v", got, want)
		}
	}
}

func listReq(ranges string, limit int, extra string) string {
	return fmt.Sprintf(`{"lists":{"all":{"ranges":%s,"timeline_limit":%d%s}}}`, ranges, limit, extra)
}

func TestRoomListWindowsOrderAndPaging(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		oldest, middle, newest := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		message(t, oldest, hn.alice, "o1")
		middle.CreateAndInsert(t, hn.alice, spec.MRoomName, map[string]string{"name": "Second"}, test.WithStateKey(""))
		message(t, middle, hn.alice, "m1")
		m2 := message(t, middle, hn.alice, "m2")
		m3 := message(t, middle, hn.alice, "m3")
		message(t, newest, hn.alice, "n1")
		n2 := message(t, newest, hn.alice, "n2")
		n3 := message(t, newest, hn.alice, "n3")
		// Written oldest first, so the stream order is the creation order.
		hn.write(oldest.Events()...)
		hn.write(middle.Events()...)
		hn.write(newest.Events()...)

		first := hn.mustPost("", listReq("[[0,1]]", 2, ""))
		assertRooms(t, first, newest.ID, middle.ID)
		if got := first.Get("lists.all.count").Int(); got != 3 {
			t.Errorf("count = %d, want 3 (it ignores ranges)", got)
		}
		if got := first.Get("pos").String(); got == "" {
			t.Error("response must have a pos")
		}

		rooms := first.Get("rooms").Map()
		if got, want := eventIDs(rooms[newest.ID], "timeline"), []string{n2.EventID(), n3.EventID()}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("newest timeline = %v, want the last two events in chronological order %v", got, want)
		}
		if got, want := eventIDs(rooms[middle.ID], "timeline"), []string{m2.EventID(), m3.EventID()}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("middle timeline = %v, want %v", got, want)
		}
		for id, room := range rooms {
			if !room.Get("initial").Bool() {
				t.Errorf("%s: a room sent for the first time must be initial", id)
			}
			if !room.Get("limited").Bool() || room.Get("prev_batch").String() == "" {
				t.Errorf("%s: more events exist than the limit, so it must be limited with a prev_batch: %s", id, room.Raw)
			}
			if room.Get("bump_stamp").Int() <= 0 {
				t.Errorf("%s: bump_stamp must be set for a room with messages", id)
			}
			if room.Get("joined_count").Int() != 1 {
				t.Errorf("%s: joined_count = %d, want 1", id, room.Get("joined_count").Int())
			}
		}
		if got := rooms[middle.ID].Get("name").String(); got != "Second" {
			t.Errorf("name = %q, want Second", got)
		}
		if rooms[newest.ID].Get("name").Exists() {
			t.Error("a room without a name must not get one")
		}

		// Growing the window only returns the rooms that were not sent yet.
		second := hn.mustPost(first.Get("pos").String(), listReq("[[0,2]]", 2, ""))
		assertRooms(t, second, oldest.ID)

		// Nothing changed in the window any more, so nothing is returned again.
		third := hn.mustPost(second.Get("pos").String(), listReq("[[0,2]]", 2, ""))
		assertRooms(t, third)
		if got := third.Get("lists.all.count").Int(); got != 3 {
			t.Errorf("count = %d, want 3 even when no rooms are returned", got)
		}
	})
}

func TestSubscriptionsAndTimelineLimits(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		message(t, room, hn.alice, "one")
		last := message(t, room, hn.alice, "two")
		hn.write(room.Events()...)

		res := hn.mustPost("", `{"room_subscriptions":{"`+room.ID+`":{"timeline_limit":1},"!unknown:test":{"timeline_limit":5}}}`)
		assertRooms(t, res, room.ID)
		if got := eventIDs(res.Get("rooms").Map()[room.ID], "timeline"); len(got) != 1 || got[0] != last.EventID() {
			t.Errorf("timeline = %v, want only the latest event", got)
		}
		if res.Get("lists").Exists() {
			t.Error("a request without lists must not return lists")
		}

		// timeline_limit 0 returns the room without any events.
		res = hn.mustPost("", `{"room_subscriptions":{"`+room.ID+`":{"timeline_limit":0}}}`)
		got := res.Get("rooms").Map()[room.ID]
		if len(got.Get("timeline").Array()) != 0 || got.Get("prev_batch").Exists() {
			t.Errorf("no timeline was requested: %s", got.Raw)
		}

		// A huge limit is capped, not rejected.
		res = hn.mustPost("", `{"room_subscriptions":{"`+room.ID+`":{"timeline_limit":1000000}}}`)
		assertRooms(t, res, room.ID)

		// A subscription and a list for the same room use the larger timeline limit.
		res = hn.mustPost("", `{"lists":{"a":{"ranges":[[0,0]],"timeline_limit":1}},"room_subscriptions":{"`+room.ID+`":{"timeline_limit":50}}}`)
		if n := len(eventIDs(res.Get("rooms").Map()[room.ID], "timeline")); n != len(room.Events()) {
			t.Errorf("got %d events, want all %d", n, len(room.Events()))
		}
	})
}

func TestListFiltersAgainstStorage(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		dm, encrypted, plain := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		encrypted.CreateAndInsert(t, hn.alice, "m.room.encryption", map[string]string{"algorithm": "m.megolm.v1.aes-sha2"}, test.WithStateKey(""))
		hn.write(dm.Events()...)
		hn.write(encrypted.Events()...)
		hn.write(plain.Events()...)
		hn.users.direct = map[string][]string{"@bob:test": {dm.ID}}

		for name, tc := range map[string]struct {
			filters string
			want    []string
		}{
			"dm only":              {`"filters":{"is_dm":true}`, []string{dm.ID}},
			"no dms":               {`"filters":{"is_dm":false}`, []string{encrypted.ID, plain.ID}},
			"encrypted only":       {`"filters":{"is_encrypted":true}`, []string{encrypted.ID}},
			"unencrypted only":     {`"filters":{"is_encrypted":false}`, []string{dm.ID, plain.ID}},
			"filters combine":      {`"filters":{"is_dm":false,"is_encrypted":false}`, []string{plain.ID}},
			"rooms without a type": {`"filters":{"room_types":[null]}`, []string{dm.ID, encrypted.ID, plain.ID}},
			"unknown type":         {`"filters":{"room_types":["m.space"]}`, nil},
			"not a space":          {`"filters":{"not_room_types":["m.space"]}`, []string{dm.ID, encrypted.ID, plain.ID}},
			"no invites":           {`"filters":{"is_invite":false}`, []string{dm.ID, encrypted.ID, plain.ID}},
			"only invites":         {`"filters":{"is_invite":true}`, nil},
		} {
			res := hn.mustPost("", listReq("[[0,99]]", 0, ","+tc.filters))
			if got := int(res.Get("lists.all.count").Int()); got != len(tc.want) {
				t.Errorf("%s: count = %d, want %d", name, got, len(tc.want))
			}
			assertRooms(t, res, tc.want...)
		}

		// is_dm is reported on the room itself.
		res := hn.mustPost("", listReq("[[0,99]]", 0, ""))
		rooms := res.Get("rooms").Map()
		if !rooms[dm.ID].Get("is_dm").Bool() {
			t.Error("the DM room must be reported as is_dm")
		}
		if rooms[plain.ID].Get("is_dm").Exists() {
			t.Error("is_dm is absent for rooms that are not direct messages")
		}
	})
}

func TestLeftRoomsAreNotListed(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		kept, left := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		left.CreateAndInsert(t, hn.alice, spec.MRoomMember, map[string]string{"membership": spec.Leave}, test.WithStateKey(hn.alice.ID))
		hn.write(kept.Events()...)
		hn.write(left.Events()...)

		res := hn.mustPost("", listReq("[[0,99]]", 1, ""))
		assertRooms(t, res, kept.ID)
		if got := res.Get("lists.all.count").Int(); got != 1 {
			t.Errorf("count = %d, want 1", got)
		}
		// Subscribing to a room the user left does not reveal it either.
		res = hn.mustPost("", `{"room_subscriptions":{"`+left.ID+`":{"timeline_limit":5}}}`)
		assertRooms(t, res)
	})
}

func TestInvites(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		joined := test.NewRoom(t, hn.alice)
		message(t, joined, hn.alice, "hello")
		hn.write(joined.Events()...)

		// A remote-style invite: the room's events are never written, only the invite is.
		space := test.NewRoom(t, bob)
		invite := space.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Invite}, test.WithStateKey(hn.alice.ID),
			test.WithUnsigned(map[string]any{"invite_room_state": []map[string]any{
				{"type": spec.MRoomCreate, "state_key": "", "sender": bob.ID, "content": map[string]any{"type": "m.space"}},
				{"type": spec.MRoomName, "state_key": "", "sender": bob.ID, "content": map[string]any{"name": "Bob's space"}},
			}}))
		hn.invite(invite)

		res := hn.mustPost("", listReq("[[0,0]]", 1, ""))
		assertRooms(t, res, space.ID)
		if got := res.Get("lists.all.count").Int(); got != 2 {
			t.Errorf("count = %d, want 2", got)
		}
		room := res.Get("rooms").Map()[space.ID]
		if !room.Get("initial").Bool() {
			t.Error("invites are sent as initial rooms")
		}
		var kinds []string
		for _, ev := range room.Get("invite_state").Array() {
			kinds = append(kinds, ev.Get("type").String())
		}
		if fmt.Sprint(kinds) != fmt.Sprint([]string{spec.MRoomCreate, spec.MRoomName, spec.MRoomMember}) {
			t.Errorf("invite_state types = %v, want the stripped state followed by the invite", kinds)
		}
		if room.Get("timeline").Exists() {
			t.Error("a room the user has not joined must not expose a timeline")
		}

		for name, tc := range map[string]struct {
			filters string
			want    []string
		}{
			"only invites":   {`"filters":{"is_invite":true}`, []string{space.ID}},
			"no invites":     {`"filters":{"is_invite":false}`, []string{joined.ID}},
			"invited spaces": {`"filters":{"room_types":["m.space"]}`, []string{space.ID}},
			"not spaces":     {`"filters":{"not_room_types":["m.space"]}`, []string{joined.ID}},
		} {
			t.Run(name, func(t *testing.T) {
				res := hn.mustPost("", listReq("[[0,99]]", 0, ","+tc.filters))
				assertRooms(t, res, tc.want...)
			})
		}

		// Invites from ignored users are hidden.
		if err := hn.db.UpdateIgnoresForUser(bg, hn.alice.ID, &types.IgnoredUsers{List: map[string]interface{}{bob.ID: map[string]any{}}}); err != nil {
			t.Fatal(err)
		}
		res = hn.mustPost("", listReq("[[0,99]]", 0, ""))
		assertRooms(t, res, joined.ID)
	})
}

func TestIgnoredUsersAreHiddenFromTimelines(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, hn.alice)
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(bob.ID))
		spam := message(t, room, bob, "spam")
		fine := message(t, room, hn.alice, "fine")
		hn.write(room.Events()...)
		if err := hn.db.UpdateIgnoresForUser(bg, hn.alice.ID, &types.IgnoredUsers{List: map[string]interface{}{bob.ID: map[string]any{}}}); err != nil {
			t.Fatal(err)
		}

		res := hn.mustPost("", listReq("[[0,0]]", 10, ""))
		got := eventIDs(res.Get("rooms").Map()[room.ID], "timeline")
		for _, id := range got {
			if id == spam.EventID() {
				t.Fatalf("an event from an ignored user was returned: %v", got)
			}
		}
		if got[len(got)-1] != fine.EventID() {
			t.Errorf("events from other users must still be returned: %v", got)
		}
	})
}

func TestHistoryVisibilityLimitsTheTimeline(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, bob, test.RoomHistoryVisibility(gomatrixserverlib.HistoryVisibilityJoined))
		before := message(t, room, bob, "before alice joined")
		invite := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Invite}, test.WithStateKey(hn.alice.ID))
		join := room.CreateAndInsert(t, hn.alice, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(hn.alice.ID))
		after := message(t, room, bob, "after alice joined")

		events := room.Events()
		hn.writeWithVisibility(gomatrixserverlib.HistoryVisibilityJoined, events...)
		// From the roomserver's point of view alice is a member from her join onwards.
		hn.rs.membershipAt[after.EventID()] = join
		hn.rs.membershipAt[join.EventID()] = join
		_ = invite

		res := hn.mustPost("", listReq("[[0,0]]", 50, ""))
		got := eventIDs(res.Get("rooms").Map()[room.ID], "timeline")
		has := func(id string) bool {
			for _, g := range got {
				if g == id {
					return true
				}
			}
			return false
		}
		if has(before.EventID()) {
			t.Errorf("history before alice joined must be hidden with joined visibility: %v", got)
		}
		if !has(after.EventID()) {
			t.Errorf("history after alice joined must be visible: %v", got)
		}
		if !has(join.EventID()) {
			t.Errorf("alice's own membership event must be visible: %v", got)
		}
	})
}

func TestRetriesAndNewSessions(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)

		first := hn.mustPost("", listReq("[[0,0]]", 1, ""))
		assertRooms(t, first, room.ID)

		// The room was sent, so the next request does not repeat it.
		next := hn.mustPost(first.Get("pos").String(), listReq("[[0,0]]", 1, ""))
		assertRooms(t, next)

		// The client never saw `next` and repeats its request: the same response is replayed.
		replay := hn.mustPost(first.Get("pos").String(), listReq("[[0,0]]", 1, ""))
		if replay.Raw != next.Raw {
			t.Errorf("replay differs from the original response:\n%s\n%s", replay.Raw, next.Raw)
		}

		// A request without pos starts over, so every room is sent again.
		fresh := hn.mustPost("", listReq("[[0,0]]", 1, ""))
		assertRooms(t, fresh, room.ID)
		if code, _ := hn.post(first.Get("pos").String(), listReq("[[0,0]]", 1, "")); code != http.StatusBadRequest {
			t.Errorf("the replaced session must be gone, got HTTP %d", code)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %s", err)
	}
	return string(b)
}

func (f *fakeRoomserver) QuerySharedUsers(_ context.Context, req *rsapi.QuerySharedUsersRequest, res *rsapi.QuerySharedUsersResponse) error {
	res.UserIDsToCount = map[string]int{}
	excluded := map[string]bool{}
	for _, id := range req.ExcludeRoomIDs {
		excluded[id] = true
	}
	included := map[string]bool{}
	for _, id := range req.IncludeRoomIDs {
		included[id] = true
	}
	for roomID, users := range f.members {
		if excluded[roomID] || (len(included) > 0 && !included[roomID]) {
			continue
		}
		for _, user := range users {
			res.UserIDsToCount[user]++
		}
	}
	return nil
}

func (f *fakeRoomserver) QueryBulkStateContent(_ context.Context, req *rsapi.QueryBulkStateContentRequest, res *rsapi.QueryBulkStateContentResponse) error {
	res.Rooms = map[string]map[gomatrixserverlib.StateKeyTuple]string{}
	for _, roomID := range req.RoomIDs {
		res.Rooms[roomID] = map[gomatrixserverlib.StateKeyTuple]string{}
		for _, user := range f.members[roomID] {
			res.Rooms[roomID][gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: user}] = spec.Join
		}
	}
	return nil
}
