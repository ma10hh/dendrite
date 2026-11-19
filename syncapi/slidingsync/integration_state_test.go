// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/test"
)

// subscribe requests a single room by subscription.
func subscribe(roomID string, limit int, requiredState string) string {
	return fmt.Sprintf(`{"room_subscriptions":{%q:{"timeline_limit":%d,"required_state":%s}}}`, roomID, limit, requiredState)
}

// stateIDs returns the "type|state_key" of each required_state event of a room, sorted.
func stateIDs(room gjson.Result) []string {
	var out []string
	for _, ev := range room.Get("required_state").Array() {
		out = append(out, ev.Get("type").String()+"|"+ev.Get("state_key").String())
	}
	sort.Strings(out)
	return out
}

func join(t *testing.T, room *test.Room, user *test.User, displayName string) *rstypes.HeaderedEvent {
	t.Helper()
	content := map[string]string{"membership": spec.Join}
	if displayName != "" {
		content["displayname"] = displayName
	}
	return room.CreateAndInsert(t, user, spec.MRoomMember, content, test.WithStateKey(user.ID))
}

func state(t *testing.T, room *test.Room, user *test.User, evType, stateKey string, content map[string]string) *rstypes.HeaderedEvent {
	t.Helper()
	return room.CreateAndInsert(t, user, evType, content, test.WithStateKey(stateKey))
}

func TestRequiredStateExactPairs(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		state(t, room, hn.alice, spec.MRoomName, "", map[string]string{"name": "Named"})
		state(t, room, hn.alice, spec.MRoomTopic, "", map[string]string{"topic": "Topic"})
		state(t, room, hn.alice, "m.room.encryption", "", map[string]string{"algorithm": "m.megolm.v1.aes-sha2"})
		hn.write(room.Events()...)

		res := hn.mustPost("", subscribe(room.ID, 0, `[["m.room.name",""],["m.room.topic",""],["m.room.member","$ME"],["m.room.missing",""]]`))
		got := stateIDs(res.Get("rooms").Map()[room.ID])
		want := []string{"m.room.member|" + hn.alice.ID, "m.room.name|", "m.room.topic|"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("required_state = %v, want exactly the requested events %v", got, want)
		}

		// Without required_state nothing is returned, and nothing is implied.
		res = hn.mustPost("", subscribe(room.ID, 0, `[]`))
		if got := stateIDs(res.Get("rooms").Map()[room.ID]); len(got) != 0 {
			t.Errorf("an empty required_state returned %v", got)
		}
	})
}

func TestRequiredStateLazyMembers(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob, carol, dave := test.NewUser(t), test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, hn.alice)
		join(t, room, bob, "Bob")
		join(t, room, carol, "Carol")
		message(t, room, carol, "from carol")
		message(t, room, bob, "from bob")
		hn.write(room.Events()...)
		members := func(users ...*test.User) []string {
			var ids []string
			for _, u := range users {
				ids = append(ids, "m.room.member|"+u.ID)
			}
			sort.Strings(ids)
			return ids
		}

		// Only the senders of the timeline are loaded. The user's own membership is not implied.
		res := hn.mustPost("", subscribe(room.ID, 2, `[["m.room.member","$LAZY"]]`))
		if got, want := stateIDs(res.Get("rooms").Map()[room.ID]), members(bob, carol); !reflect.DeepEqual(got, want) {
			t.Errorf("lazy members = %v, want the timeline senders %v", got, want)
		}

		// Asking for oneself explicitly adds it.
		res = hn.mustPost("", subscribe(room.ID, 2, `[["m.room.member","$LAZY"],["m.room.member","$ME"]]`))
		if got, want := stateIDs(res.Get("rooms").Map()[room.ID]), members(hn.alice, bob, carol); !reflect.DeepEqual(got, want) {
			t.Errorf("lazy members with $ME = %v, want %v", got, want)
		}

		// Without a timeline there is nobody to load.
		res = hn.mustPost("", subscribe(room.ID, 0, `[["m.room.member","$LAZY"]]`))
		if got := stateIDs(res.Get("rooms").Map()[room.ID]); len(got) != 0 {
			t.Errorf("no timeline, but lazy members = %v", got)
		}

		// The target of a membership event in the timeline is loaded as well as its sender.
		room.CreateAndInsert(t, hn.alice, spec.MRoomMember, map[string]string{"membership": spec.Invite}, test.WithStateKey(dave.ID))
		invite := room.Events()[len(room.Events())-1]
		hn.write(invite)
		res = hn.mustPost("", subscribe(room.ID, 1, `[["m.room.member","$LAZY"]]`))
		if got, want := stateIDs(res.Get("rooms").Map()[room.ID]), members(hn.alice, dave); !reflect.DeepEqual(got, want) {
			t.Errorf("lazy members for an invite = %v, want sender and target %v", got, want)
		}
	})
}

func TestRequiredStateWildcards(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, hn.alice)
		join(t, room, bob, "")
		state(t, room, hn.alice, "m.space.child", "!a:test", map[string]string{"via": "x"})
		state(t, room, hn.alice, "m.space.child", "!b:test", map[string]string{"via": "x"})
		state(t, room, hn.alice, spec.MRoomName, "", map[string]string{"name": "Named"})
		hn.write(room.Events()...)
		post := func(required string) []string {
			return stateIDs(hn.mustPost("", subscribe(room.ID, 0, required)).Get("rooms").Map()[room.ID])
		}

		if got, want := post(`[["m.space.child","*"]]`), []string{"m.space.child|!a:test", "m.space.child|!b:test"}; !reflect.DeepEqual(got, want) {
			t.Errorf("wildcard state key = %v, want %v", got, want)
		}

		all := post(`[["*","*"]]`)
		for _, want := range []string{"m.room.create|", "m.room.name|", "m.space.child|!a:test", "m.room.member|" + bob.ID, "m.room.member|" + hn.alice.ID} {
			if !contains(all, want) {
				t.Errorf("all state is missing %s: %v", want, all)
			}
		}
		// As in Synapse, asking for everything also returns every member, whatever else was asked for.
		if got := post(`[["m.room.member","$LAZY"],["*","*"]]`); !reflect.DeepEqual(got, all) {
			t.Errorf("all state combined with $LAZY = %v, want all state %v", got, all)
		}

		emptyKeys := post(`[["*",""]]`)
		if !contains(emptyKeys, "m.room.name|") || contains(emptyKeys, "m.space.child|!a:test") || contains(emptyKeys, "m.room.member|"+bob.ID) {
			t.Errorf("wildcard type with an empty key = %v, want only events with an empty state key", emptyKeys)
		}

		if got := post(`[["m.room.member","*"]]`); len(got) != 2 {
			t.Errorf("every member = %v, want alice and bob", got)
		}
	})
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestRequiredStateCombinesRules(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		state(t, room, hn.alice, spec.MRoomName, "", map[string]string{"name": "Named"})
		state(t, room, hn.alice, spec.MRoomTopic, "", map[string]string{"topic": "Topic"})
		hn.write(room.Events()...)

		res := hn.mustPost("", fmt.Sprintf(
			`{"lists":{"a":{"ranges":[[0,0]],"timeline_limit":0,"required_state":[["m.room.name",""]]}},"room_subscriptions":{%q:{"timeline_limit":0,"required_state":[["m.room.topic",""],["m.room.name",""]]}}}`,
			room.ID))
		if got, want := stateIDs(res.Get("rooms").Map()[room.ID]), []string{"m.room.name|", "m.room.topic|"}; !reflect.DeepEqual(got, want) {
			t.Errorf("combined required_state = %v, want the union %v", got, want)
		}
	})
}

func TestHeroes(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob, carol := test.NewUser(t), test.NewUser(t)

		unnamed := test.NewRoom(t, hn.alice)
		join(t, unnamed, bob, "Bob")
		join(t, unnamed, carol, "")
		named := test.NewRoom(t, hn.alice)
		state(t, named, hn.alice, spec.MRoomName, "", map[string]string{"name": "Named"})
		join(t, named, bob, "Bob")
		aliased := test.NewRoom(t, hn.alice)
		state(t, aliased, hn.alice, spec.MRoomCanonicalAlias, "", map[string]string{"alias": "#alias:test"})
		join(t, aliased, bob, "Bob")
		alone := test.NewRoom(t, hn.alice)
		emptied := test.NewRoom(t, hn.alice)
		join(t, emptied, bob, "Bob")
		emptied.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Leave}, test.WithStateKey(bob.ID))
		for _, r := range []*test.Room{unnamed, named, aliased, alone, emptied} {
			hn.write(r.Events()...)
		}

		res := hn.mustPost("", listReq("[[0,9]]", 0, ""))
		rooms := res.Get("rooms").Map()

		heroes := func(room gjson.Result) []string {
			var out []string
			for _, h := range room.Get("heroes").Array() {
				out = append(out, h.Get("user_id").String())
			}
			return out
		}
		if got, want := heroes(rooms[unnamed.ID]), []string{bob.ID, carol.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("unnamed room heroes = %v, want the other members %v", got, want)
		}
		first := rooms[unnamed.ID].Get("heroes.0")
		if first.Get("displayname").String() != "Bob" {
			t.Errorf("hero profile missing: %s", first.Raw)
		}
		if rooms[unnamed.ID].Get("heroes.1.displayname").Exists() {
			t.Error("a hero without a display name must not get an empty one")
		}
		if got := heroes(rooms[named.ID]); len(got) != 0 {
			t.Errorf("a named room has no heroes, got %v", got)
		}
		if got := heroes(rooms[aliased.ID]); len(got) != 1 {
			t.Errorf("a canonical alias does not name the room, so it still has heroes, got %v", got)
		}
		if rooms[aliased.ID].Get("name").Exists() {
			t.Error("the canonical alias must not become the name")
		}
		if got := heroes(rooms[alone.ID]); len(got) != 0 {
			t.Errorf("the requester is never a hero, got %v", got)
		}
		if got, want := heroes(rooms[emptied.ID]), []string{bob.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("with nobody else joined the users who left are used, got %v want %v", got, want)
		}
		if got := rooms[unnamed.ID].Get("joined_count").Int(); got != 3 {
			t.Errorf("joined_count = %d, want 3", got)
		}
	})
}

func TestUnreadCounts(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		busy, quiet := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		hn.write(busy.Events()...)
		hn.write(quiet.Events()...)
		if _, err := hn.db.UpsertRoomUnreadNotificationCounts(bg, hn.alice.ID, busy.ID, 7, 2); err != nil {
			t.Fatal(err)
		}

		rooms := hn.mustPost("", listReq("[[0,9]]", 0, "")).Get("rooms").Map()
		if got := rooms[busy.ID]; got.Get("notification_count").Int() != 7 || got.Get("highlight_count").Int() != 2 {
			t.Errorf("busy room counts: %s", strings.TrimSpace(got.Raw))
		}
		// Rooms without notifications report zero explicitly rather than leaving the counts out.
		if got := rooms[quiet.ID]; !got.Get("notification_count").Exists() || got.Get("notification_count").Int() != 0 || got.Get("highlight_count").Int() != 0 {
			t.Errorf("quiet room counts: %s", strings.TrimSpace(got.Raw))
		}
	})
}
