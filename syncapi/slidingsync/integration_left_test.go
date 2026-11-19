// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"reflect"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/test"
)

func membership(t *testing.T, room *test.Room, sender, target *test.User, value string) *rstypes.HeaderedEvent {
	t.Helper()
	return room.CreateAndInsert(t, sender, spec.MRoomMember, map[string]string{"membership": value}, test.WithStateKey(target.ID))
}

func TestKickedRoomShowsHistoryUpToTheKickAndNothingAfter(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, bob)
		membership(t, room, bob, hn.alice, spec.Invite)
		join := room.CreateAndInsert(t, hn.alice, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(hn.alice.ID))
		during := message(t, room, bob, "while alice was in the room")
		kick := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Leave}, test.WithStateKey(hn.alice.ID))
		after := message(t, room, bob, "written after the kick")
		state(t, room, bob, spec.MRoomName, "", map[string]string{"name": "Renamed after the kick"})
		state(t, room, bob, spec.MRoomTopic, "", map[string]string{"topic": "secret"})
		hn.write(room.Events()...)
		hn.rs.membershipAt[join.EventID()] = join
		hn.rs.membershipAt[during.EventID()] = join

		res := hn.mustPost("", listReq("[[0,9]]", 20, `,"required_state":[["*","*"]]`))
		assertRooms(t, res, room.ID)
		got := res.Get("rooms").Map()[room.ID]

		ids := eventIDs(got, "timeline")
		if len(ids) == 0 || ids[len(ids)-1] != kick.EventID() {
			t.Fatalf("the timeline must end with the kick, got %v", ids)
		}
		if !contains(ids, during.EventID()) {
			t.Errorf("events from while the user was in the room are visible: %v", ids)
		}
		if contains(ids, after.EventID()) {
			t.Errorf("an event after the kick was returned: %v", ids)
		}
		if got.Get("required_state").Exists() || got.Get("name").Exists() || got.Get("heroes").Exists() || got.Get("joined_count").Exists() {
			t.Errorf("state written after the kick must not be exposed: %s", got.Raw)
		}
		if got.Get("bump_stamp").Int() <= 0 || !got.Get("initial").Bool() {
			t.Errorf("a kicked room is an initial room with a bump_stamp: %s", got.Raw)
		}
		if res.Get("lists.all.count").Int() != 1 {
			t.Errorf("count = %d, want 1", res.Get("lists.all.count").Int())
		}

		// It can be asked for by subscription too, and with no timeline it has no events.
		res = hn.mustPost("", subscribe(room.ID, 0, `[]`))
		if got := res.Get("rooms").Map()[room.ID]; !got.Exists() || got.Get("timeline").Exists() {
			t.Errorf("subscription to a kicked room: %s", got.Raw)
		}
	})
}

func TestBannedRooms(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)

		joinedThenBanned := test.NewRoom(t, bob)
		membership(t, joinedThenBanned, bob, hn.alice, spec.Invite)
		join(t, joinedThenBanned, hn.alice, "")
		membership(t, joinedThenBanned, bob, hn.alice, spec.Ban)

		invitedThenBanned := test.NewRoom(t, bob)
		membership(t, invitedThenBanned, bob, hn.alice, spec.Invite)
		membership(t, invitedThenBanned, bob, hn.alice, spec.Ban)

		neverInvolved := test.NewRoom(t, bob)
		membership(t, neverInvolved, bob, hn.alice, spec.Ban)

		for _, r := range []*test.Room{joinedThenBanned, invitedThenBanned, neverInvolved} {
			hn.write(r.Events()...)
		}

		res := hn.mustPost("", listReq("[[0,9]]", 5, ""))
		assertRooms(t, res, joinedThenBanned.ID)
		ids := eventIDs(res.Get("rooms").Map()[joinedThenBanned.ID], "timeline")
		last := joinedThenBanned.Events()[len(joinedThenBanned.Events())-1]
		if len(ids) == 0 || ids[len(ids)-1] != last.EventID() {
			t.Errorf("the timeline of a banned room must end with the ban, got %v", ids)
		}
	})
}

func TestSelfLeaveIsNotAKick(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, bob)
		membership(t, room, bob, hn.alice, spec.Invite)
		join(t, room, hn.alice, "")
		membership(t, room, hn.alice, hn.alice, spec.Leave)
		hn.write(room.Events()...)

		assertRooms(t, hn.mustPost("", listReq("[[0,9]]", 5, "")))
	})
}

func TestKnockedRooms(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)

		// A room this server is in: the stripped state is built from its current state.
		local := test.NewRoom(t, bob)
		state(t, local, bob, spec.MRoomJoinRules, "", map[string]string{"join_rule": "knock"})
		state(t, local, bob, spec.MRoomName, "", map[string]string{"name": "Knock here"})
		state(t, local, bob, spec.MRoomTopic, "", map[string]string{"topic": "A topic"})
		state(t, local, bob, "m.room.power_levels_unrelated", "", map[string]string{"secret": "not stripped state"})
		membership(t, local, hn.alice, hn.alice, spec.Knock)

		// A room the knock came with its own stripped state for, which must be used as given.
		remote := test.NewRoom(t, bob)
		state(t, remote, bob, spec.MRoomJoinRules, "", map[string]string{"join_rule": "knock"})
		remote.CreateAndInsert(t, hn.alice, spec.MRoomMember, map[string]string{"membership": spec.Knock}, test.WithStateKey(hn.alice.ID),
			test.WithUnsigned(map[string]any{"knock_room_state": []map[string]any{
				{"type": spec.MRoomName, "state_key": "", "sender": bob.ID, "content": map[string]any{"name": "Attached"}},
			}}))

		hn.write(local.Events()...)
		hn.write(remote.Events()...)

		res := hn.mustPost("", listReq("[[0,9]]", 5, ""))
		assertRooms(t, res, local.ID, remote.ID)

		kinds := func(room gjson.Result) []string {
			var out []string
			for _, ev := range room.Get("invite_state").Array() {
				out = append(out, ev.Get("type").String())
			}
			return out
		}
		localRoom := res.Get("rooms").Map()[local.ID]
		want := []string{spec.MRoomCreate, spec.MRoomName, spec.MRoomTopic, spec.MRoomJoinRules, spec.MRoomMember}
		if got := kinds(localRoom); !reflect.DeepEqual(got, want) {
			t.Errorf("stripped state of a local room = %v, want %v", got, want)
		}
		knock := localRoom.Get("invite_state.#(type==\"m.room.member\")")
		if knock.Get("content.membership").String() != spec.Knock || knock.Get("state_key").String() != hn.alice.ID || knock.Get("sender").String() != hn.alice.ID {
			t.Errorf("the knock itself must be last in the stripped state: %s", knock.Raw)
		}
		for _, field := range []string{"event_id", "origin_server_ts", "unsigned"} {
			if knock.Get(field).Exists() {
				t.Errorf("stripped events must not carry %s: %s", field, knock.Raw)
			}
		}
		if localRoom.Get("timeline").Exists() || localRoom.Get("required_state").Exists() {
			t.Errorf("a knocked room has no timeline or state: %s", localRoom.Raw)
		}

		remoteRoom := res.Get("rooms").Map()[remote.ID]
		if got, want := kinds(remoteRoom), []string{spec.MRoomName, spec.MRoomMember}; !reflect.DeepEqual(got, want) {
			t.Errorf("stripped state of a remote knock = %v, want the attached state then the knock %v", got, want)
		}
		if remoteRoom.Get("invite_state.0.content.name").String() != "Attached" {
			t.Errorf("attached state must be used as given: %s", remoteRoom.Raw)
		}

		// Knocks are not invites.
		assertRooms(t, hn.mustPost("", listReq("[[0,9]]", 0, `,"filters":{"is_invite":true}`)))
		assertRooms(t, hn.mustPost("", listReq("[[0,9]]", 0, `,"filters":{"is_invite":false}`)), local.ID, remote.ID)
	})
}
