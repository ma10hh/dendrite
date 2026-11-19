// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// This is the request shape produced by Ruma's `sync_events::msc4186` (Element X).
const elementXRequest = `{
	"conn_id": "room-list",
	"txn_id": "7",
	"lists": {
		"all_rooms": {
			"ranges": [[0, 19], [40, 59]],
			"required_state": [["m.room.name", ""], ["m.room.member", "$LAZY"], ["*", "*"]],
			"timeline_limit": 1,
			"filters": {"is_dm": false, "is_invite": true, "room_types": [null, "m.space"], "not_room_types": ["m.other"]}
		}
	},
	"room_subscriptions": {"!r:x": {"required_state": [["m.room.create", ""]], "timeline_limit": 20}},
	"extensions": {"to_device": {"enabled": true, "since": "5", "limit": 100}, "e2ee": {"enabled": true}}
}`

func TestDecodeElementXRequest(t *testing.T) {
	var req Request
	if err := json.Unmarshal([]byte(elementXRequest), &req); err != nil {
		t.Fatal(err)
	}
	list := req.Lists["all_rooms"]
	if len(list.Ranges) != 2 || list.Ranges[1] != [2]int64{40, 59} {
		t.Errorf("ranges = %v", list.Ranges)
	}
	if list.TimelineLimit != 1 || len(list.RequiredState) != 3 || list.RequiredState[1] != (StateKeyPair{"m.room.member", "$LAZY"}) {
		t.Errorf("room config = %+v", list.RoomConfig)
	}
	f := list.Filters
	if f == nil || f.IsDM == nil || *f.IsDM || f.IsInvite == nil || !*f.IsInvite || f.IsEncrypted != nil {
		t.Errorf("filters = %+v", f)
	}
	if len(f.RoomTypes) != 2 || f.RoomTypes[0] != nil || *f.RoomTypes[1] != "m.space" {
		t.Errorf("room_types must keep null as 'rooms without a type', got %v", f.RoomTypes)
	}
	if sub := req.RoomSubscriptions["!r:x"]; sub.TimelineLimit != 20 || len(sub.RequiredState) != 1 {
		t.Errorf("subscription = %+v", sub)
	}
	if req.TxnID != "7" || req.ConnID != "room-list" || len(req.Extensions) != 2 {
		t.Errorf("top-level fields = %+v", req)
	}
}

func TestEncodeResponseUsesClientFieldNames(t *testing.T) {
	name, joined, notif, num, bump := "Room", 3, 2, 1, int64(42)
	resp := Response{
		TxnID: "7",
		Pos:   "s/1",
		Lists: map[string]ListResponse{"all_rooms": {Count: 5}},
		Rooms: map[string]RoomResponse{"!r:x": {
			Name:              &name,
			Avatar:            json.RawMessage(`null`),
			Initial:           true,
			Heroes:            []Hero{{UserID: "@b:x", DisplayName: "Bob", AvatarURL: "mxc://x/y"}},
			NotificationCount: &notif,
			HighlightCount:    &notif,
			JoinedCount:       &joined,
			NumLive:           &num,
			BumpStamp:         &bump,
		}},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	out := gjson.ParseBytes(b)

	for path, want := range map[string]string{
		"txn_id":                          "7",
		"pos":                             "s/1",
		"lists.all_rooms.count":           "5",
		`rooms.!r:x.name`:                 "Room",
		`rooms.!r:x.initial`:              "true",
		`rooms.!r:x.heroes.0.user_id`:     "@b:x",
		`rooms.!r:x.heroes.0.displayname`: "Bob",
		`rooms.!r:x.heroes.0.avatar_url`:  "mxc://x/y",
		`rooms.!r:x.notification_count`:   "2",
		`rooms.!r:x.highlight_count`:      "2",
		`rooms.!r:x.joined_count`:         "3",
		`rooms.!r:x.num_live`:             "1",
		`rooms.!r:x.bump_stamp`:           "42",
	} {
		if got := out.Get(path); got.String() != want {
			t.Errorf("%s = %q, want %q (body %s)", path, got.String(), want, b)
		}
	}
	// A removed avatar is the literal null, which is different from "not mentioned".
	if av := out.Get(`rooms.!r:x.avatar`); !av.Exists() || av.Type != gjson.Null {
		t.Errorf("avatar must be an explicit null, got %s", av.Raw)
	}
	for _, absent := range []string{"timeline", "required_state", "invite_state", "limited", "prev_batch", "invited_count", "is_dm"} {
		if out.Get(`rooms.!r:x.` + absent).Exists() {
			t.Errorf("%s must be omitted when empty", absent)
		}
	}
}
