// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"math"
	"reflect"
	"testing"

	"github.com/element-hq/dendrite/syncapi/types"
)

func ptr[T any](v T) *T { return &v }

func joined(id string, lastEvent int64) roomEntry {
	return roomEntry{roomID: id, activity: types.RoomActivity{LastEvent: types.StreamPosition(lastEvent)}}
}

func ids(entries []roomEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.roomID)
	}
	return out
}

func TestSortEntries(t *testing.T) {
	entries := []roomEntry{
		joined("!old", 1),
		{roomID: "!invite-old", invited: true, inviteTS: 100},
		joined("!new", 9),
		{roomID: "!invite-new", invited: true, inviteTS: 200},
		joined("!tie-b", 5),
		joined("!tie-a", 5),
		joined("!silent", 0),
	}
	sortEntries(entries)
	want := []string{"!invite-new", "!invite-old", "!new", "!tie-a", "!tie-b", "!old", "!silent"}
	if got := ids(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSortIgnoresBumpStamp(t *testing.T) {
	// "!quiet" has newer activity than "!chatty" but an older bump: activity decides.
	quiet := roomEntry{roomID: "!quiet", activity: types.RoomActivity{LastEvent: 10, LastBump: 1}}
	chatty := roomEntry{roomID: "!chatty", activity: types.RoomActivity{LastEvent: 5, LastBump: 5}}
	entries := []roomEntry{chatty, quiet}
	sortEntries(entries)
	if got := ids(entries); !reflect.DeepEqual(got, []string{"!quiet", "!chatty"}) {
		t.Errorf("order = %v", got)
	}
}

func TestFilters(t *testing.T) {
	space, other := ptr("m.space"), ptr("m.other")
	dm := roomEntry{roomID: "!dm", isDM: true, isEncrypted: true}
	plain := roomEntry{roomID: "!plain"}
	spaceRoom := roomEntry{roomID: "!space", roomType: space}
	invite := roomEntry{roomID: "!invite", invited: true}

	for name, tc := range map[string]struct {
		filters *ListFilters
		want    []roomEntry
	}{
		"nil filters match everything":  {nil, []roomEntry{dm, plain, spaceRoom, invite}},
		"is_dm true":                    {&ListFilters{IsDM: ptr(true)}, []roomEntry{dm}},
		"is_dm false":                   {&ListFilters{IsDM: ptr(false)}, []roomEntry{plain, spaceRoom, invite}},
		"is_encrypted true":             {&ListFilters{IsEncrypted: ptr(true)}, []roomEntry{dm}},
		"is_invite true":                {&ListFilters{IsInvite: ptr(true)}, []roomEntry{invite}},
		"is_invite false":               {&ListFilters{IsInvite: ptr(false)}, []roomEntry{dm, plain, spaceRoom}},
		"room_types space":              {&ListFilters{RoomTypes: []*string{space}}, []roomEntry{spaceRoom}},
		"room_types null means no type": {&ListFilters{RoomTypes: []*string{nil}}, []roomEntry{dm, plain, invite}},
		"room_types null or space":      {&ListFilters{RoomTypes: []*string{nil, space}}, []roomEntry{dm, plain, spaceRoom, invite}},
		"room_types unknown":            {&ListFilters{RoomTypes: []*string{other}}, nil},
		"not_room_types space":          {&ListFilters{NotRoomTypes: []*string{space}}, []roomEntry{dm, plain, invite}},
		"not_room_types null":           {&ListFilters{NotRoomTypes: []*string{nil}}, []roomEntry{spaceRoom}},
		"not_room_types wins":           {&ListFilters{RoomTypes: []*string{space}, NotRoomTypes: []*string{space}}, nil},
		"filters are ANDed":             {&ListFilters{IsDM: ptr(true), IsInvite: ptr(true)}, nil},
	} {
		var got []roomEntry
		for _, e := range []roomEntry{dm, plain, spaceRoom, invite} {
			if tc.filters.matches(e) {
				got = append(got, e)
			}
		}
		if !reflect.DeepEqual(ids(got), ids(tc.want)) {
			t.Errorf("%s: got %v, want %v", name, ids(got), ids(tc.want))
		}
	}
}

func TestNeedsFor(t *testing.T) {
	req := &Request{Lists: map[string]ListRequest{
		"a": {},
		"b": {Filters: &ListFilters{IsDM: ptr(true)}},
	}}
	if n := needsFor(req); !reflect.DeepEqual(n, attributeNeeds{}) {
		t.Errorf("is_dm and an absent filter need nothing extra, got %+v", n)
	}
	req.Lists["c"] = ListRequest{Filters: &ListFilters{IsEncrypted: ptr(false)}}
	req.Lists["d"] = ListRequest{Filters: &ListFilters{NotRoomTypes: []*string{ptr("m.space")}}}
	if n := needsFor(req); !n.encrypted || !n.roomType {
		t.Errorf("needs = %+v, want encrypted and roomType", n)
	}
}

func TestWindow(t *testing.T) {
	entries := []roomEntry{joined("!0", 0), joined("!1", 0), joined("!2", 0), joined("!3", 0), joined("!4", 0)}
	for name, tc := range map[string]struct {
		ranges [][2]int64
		want   []string
	}{
		"single range is inclusive": {[][2]int64{{0, 1}}, []string{"!0", "!1"}},
		"end beyond the list":       {[][2]int64{{3, 100}}, []string{"!3", "!4"}},
		"huge end is clamped":       {[][2]int64{{0, math.MaxInt64}}, []string{"!0", "!1", "!2", "!3", "!4"}},
		"overlap is deduplicated":   {[][2]int64{{0, 2}, {1, 3}}, []string{"!0", "!1", "!2", "!3"}},
		"disjoint ranges":           {[][2]int64{{0, 0}, {4, 4}}, []string{"!0", "!4"}},
		"negative start is clamped": {[][2]int64{{-5, 0}}, []string{"!0"}},
		"start past the end":        {[][2]int64{{10, 20}}, nil},
		"inverted range is empty":   {[][2]int64{{3, 1}}, nil},
		"no ranges":                 {nil, nil},
	} {
		got := ids(window(entries, tc.ranges))
		if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	if got := window(nil, [][2]int64{{0, 9}}); len(got) != 0 {
		t.Errorf("an empty list has an empty window, got %v", got)
	}
}

func TestCombineConfigs(t *testing.T) {
	a := RoomConfig{TimelineLimit: 1, RequiredState: []StateKeyPair{{"m.room.name", ""}, {"m.room.create", ""}}}
	b := RoomConfig{TimelineLimit: 20, RequiredState: []StateKeyPair{{"m.room.create", ""}, {"m.room.topic", ""}}}
	got := a.combine(b)
	if got.TimelineLimit != 20 {
		t.Errorf("timeline_limit = %d, want the maximum", got.TimelineLimit)
	}
	want := []StateKeyPair{{"m.room.name", ""}, {"m.room.create", ""}, {"m.room.topic", ""}}
	if !reflect.DeepEqual(got.RequiredState, want) {
		t.Errorf("required_state = %v, want the deduplicated union %v", got.RequiredState, want)
	}
}

func TestClampedTimelineLimit(t *testing.T) {
	if got := (RoomConfig{TimelineLimit: -3}).clamped().TimelineLimit; got != 0 {
		t.Errorf("negative limit = %d, want 0", got)
	}
	if got := (RoomConfig{TimelineLimit: 100000}).clamped().TimelineLimit; got != maxTimelineLimit {
		t.Errorf("huge limit = %d, want %d", got, maxTimelineLimit)
	}
}

func TestPlanRooms(t *testing.T) {
	sorted := []roomEntry{
		{roomID: "!invite", invited: true},
		{roomID: "!dm1", isDM: true}, {roomID: "!a"}, {roomID: "!dm2", isDM: true}, {roomID: "!b"}, {roomID: "!c"},
	}
	req := &Request{
		Lists: map[string]ListRequest{
			"top":  {RoomConfig: RoomConfig{TimelineLimit: 2}, Ranges: [][2]int64{{0, 2}}},
			"dms":  {RoomConfig: RoomConfig{TimelineLimit: 10}, Ranges: [][2]int64{{0, 0}}, Filters: &ListFilters{IsDM: ptr(true)}},
			"none": {RoomConfig: RoomConfig{TimelineLimit: 1}, Filters: &ListFilters{IsDM: ptr(true), IsInvite: ptr(true)}},
		},
		RoomSubscriptions: map[string]RoomConfig{
			"!c":       {TimelineLimit: 50},
			"!unknown": {TimelineLimit: 50},
		},
	}
	p := planRooms(sorted, req)

	if want := map[string]int{"top": 6, "dms": 2, "none": 0}; !reflect.DeepEqual(p.counts, want) {
		t.Errorf("counts = %v, want %v (counts ignore ranges)", p.counts, want)
	}
	got := map[string]int{}
	for id, cfg := range p.rooms {
		got[id] = cfg.TimelineLimit
	}
	// !dm1 is in both lists: the larger timeline limit wins. !unknown is not visible to the user.
	want := map[string]int{"!invite": 2, "!dm1": 10, "!a": 2, "!c": 50}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rooms = %v, want %v", got, want)
	}
}

func TestPlanRoomsClampsTimelineLimit(t *testing.T) {
	p := planRooms([]roomEntry{{roomID: "!a"}}, &Request{Lists: map[string]ListRequest{
		"x": {RoomConfig: RoomConfig{TimelineLimit: 1 << 20}, Ranges: [][2]int64{{0, 0}}},
	}})
	if got := p.rooms["!a"].TimelineLimit; got != maxTimelineLimit {
		t.Errorf("timeline_limit = %d, want %d", got, maxTimelineLimit)
	}
}

func TestTagAndSpaceFilters(t *testing.T) {
	fav := roomEntry{roomID: "!fav", tags: []string{"m.favourite"}}
	lowFav := roomEntry{roomID: "!both", tags: []string{"m.favourite", "m.lowpriority"}}
	low := roomEntry{roomID: "!low", tags: []string{"m.lowpriority"}}
	untagged := roomEntry{roomID: "!none"}
	inA := roomEntry{roomID: "!inA", inSpaces: []string{"!spaceA"}}
	inB := roomEntry{roomID: "!inB", inSpaces: []string{"!spaceB"}}
	inBoth := roomEntry{roomID: "!inBoth", inSpaces: []string{"!spaceA", "!spaceB"}}
	all := []roomEntry{fav, lowFav, low, untagged, inA, inB, inBoth}

	for name, tc := range map[string]struct {
		filters *ListFilters
		want    []string
	}{
		"tags":                      {&ListFilters{Tags: []string{"m.favourite"}}, []string{"!fav", "!both"}},
		"any of several tags":       {&ListFilters{Tags: []string{"m.favourite", "m.lowpriority"}}, []string{"!fav", "!both", "!low"}},
		"not_tags":                  {&ListFilters{NotTags: []string{"m.lowpriority"}}, []string{"!fav", "!none", "!inA", "!inB", "!inBoth"}},
		"not_tags wins":             {&ListFilters{Tags: []string{"m.favourite"}, NotTags: []string{"m.lowpriority"}}, []string{"!fav"}},
		"unknown tag":               {&ListFilters{Tags: []string{"u.nothing"}}, nil},
		"one space":                 {&ListFilters{Spaces: []string{"!spaceA"}}, []string{"!inA", "!inBoth"}},
		"any of several spaces":     {&ListFilters{Spaces: []string{"!spaceA", "!spaceB"}}, []string{"!inA", "!inB", "!inBoth"}},
		"space the user is not in":  {&ListFilters{Spaces: []string{"!elsewhere"}}, nil},
		"spaces and tags are ANDed": {&ListFilters{Spaces: []string{"!spaceA"}, NotTags: []string{"m.favourite"}}, []string{"!inA", "!inBoth"}},
	} {
		var got []roomEntry
		for _, e := range all {
			if tc.filters.matches(e) {
				got = append(got, e)
			}
		}
		if !reflect.DeepEqual(ids(got), ids(entriesWithIDs(tc.want))) {
			t.Errorf("%s: got %v, want %v", name, ids(got), tc.want)
		}
	}
}

func entriesWithIDs(ids []string) []roomEntry {
	var out []roomEntry
	for _, id := range ids {
		out = append(out, roomEntry{roomID: id})
	}
	return out
}

func TestNeedsForTagsAndSpaces(t *testing.T) {
	req := &Request{Lists: map[string]ListRequest{
		"a": {Filters: &ListFilters{Tags: []string{"m.favourite"}}},
		"b": {Filters: &ListFilters{Spaces: []string{"!s1", "!s2"}}},
		"c": {Filters: &ListFilters{NotTags: []string{"m.lowpriority"}, Spaces: []string{"!s2"}}},
	}}
	n := needsFor(req)
	if !n.tags {
		t.Error("tag filters need the room tags")
	}
	if len(n.spaces) != 2 {
		t.Errorf("spaces = %v, want the two named spaces once each", n.spaces)
	}
}
