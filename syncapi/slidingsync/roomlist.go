// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"sort"

	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/syncapi/types"
)

// maxTimelineLimit caps how many timeline events a client can ask for per room.
const maxTimelineLimit = 100

// roomEntry is a room that can show up in a list, with the attributes lists filter on.
type roomEntry struct {
	roomID string

	invited bool
	// knocked is set for rooms the user has asked to join. They sort by the position of the knock.
	knocked bool
	// left is set for rooms the user was kicked or banned from. They sort by the position of that event.
	left bool
	// token is the ID of the membership event behind an invite, knock, kick or ban.
	token string
	// inviteTS orders invites among themselves.
	inviteTS spec.Timestamp
	// activity is only set for rooms the user has joined.
	activity types.RoomActivity

	isDM        bool
	isEncrypted bool
	// roomType is the `type` of the room's m.room.create event, nil if it has none.
	roomType *string
}

// sortEntries orders rooms for lists. Invites come first, newest first, so that they are
// never hidden below a window. The remaining rooms are ordered by their latest activity.
// Rooms are deliberately not ordered by bump_stamp (see MSC4186).
func sortEntries(entries []roomEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch {
		case a.invited != b.invited:
			return a.invited
		case a.invited && a.inviteTS != b.inviteTS:
			return a.inviteTS > b.inviteTS
		case !a.invited && a.activity.LastEvent != b.activity.LastEvent:
			return a.activity.LastEvent > b.activity.LastEvent
		}
		return a.roomID < b.roomID
	})
}

// matches reports whether the room passes all filters that are set. A nil receiver matches everything.
func (f *ListFilters) matches(e roomEntry) bool {
	if f == nil {
		return true
	}
	if f.IsDM != nil && *f.IsDM != e.isDM {
		return false
	}
	if f.IsEncrypted != nil && *f.IsEncrypted != e.isEncrypted {
		return false
	}
	if f.IsInvite != nil && *f.IsInvite != e.invited {
		return false
	}
	if len(f.RoomTypes) > 0 && !roomTypeIn(e.roomType, f.RoomTypes) {
		return false
	}
	// not_room_types wins over room_types.
	return !roomTypeIn(e.roomType, f.NotRoomTypes)
}

// roomTypeIn reports whether roomType is in the list; a nil entry stands for "no type".
func roomTypeIn(roomType *string, list []*string) bool {
	for _, t := range list {
		if t == nil && roomType == nil {
			return true
		}
		if t != nil && roomType != nil && *t == *roomType {
			return true
		}
	}
	return false
}

// attributeNeeds says which expensive room attributes the request's filters depend on.
type attributeNeeds struct {
	encrypted bool
	roomType  bool
}

func needsFor(req *Request) attributeNeeds {
	var n attributeNeeds
	for _, l := range req.Lists {
		if l.Filters == nil {
			continue
		}
		n.encrypted = n.encrypted || l.Filters.IsEncrypted != nil
		n.roomType = n.roomType || len(l.Filters.RoomTypes) > 0 || len(l.Filters.NotRoomTypes) > 0
	}
	return n
}

// window returns the entries covered by the inclusive ranges, in list order and without duplicates.
func window(entries []roomEntry, ranges [][2]int64) []roomEntry {
	picked := make([]bool, len(entries))
	for _, r := range ranges {
		start, end := r[0], r[1]
		if start < 0 {
			start = 0
		}
		if end >= int64(len(entries)) {
			end = int64(len(entries)) - 1
		}
		for i := start; i <= end; i++ {
			picked[i] = true
		}
	}
	var out []roomEntry
	for i, p := range picked {
		if p {
			out = append(out, entries[i])
		}
	}
	return out
}

// combine merges two room configs into the superset of both.
func (c RoomConfig) combine(o RoomConfig) RoomConfig {
	out := RoomConfig{TimelineLimit: max(c.TimelineLimit, o.TimelineLimit)}
	seen := make(map[StateKeyPair]struct{}, len(c.RequiredState)+len(o.RequiredState))
	for _, pairs := range [][]StateKeyPair{c.RequiredState, o.RequiredState} {
		for _, p := range pairs {
			if _, dup := seen[p]; !dup {
				seen[p] = struct{}{}
				out.RequiredState = append(out.RequiredState, p)
			}
		}
	}
	return out
}

func (c RoomConfig) clamped() RoomConfig {
	c.TimelineLimit = min(max(c.TimelineLimit, 0), maxTimelineLimit)
	return c
}

// plan is the outcome of evaluating a request's lists and subscriptions.
type plan struct {
	// counts is the number of rooms matching each list's filters, ignoring its ranges.
	counts map[string]int
	// rooms are the rooms to return, with the combined config of every rule matching them.
	rooms map[string]RoomConfig
	// byList are the rooms in the windows of each list, in list order.
	byList map[string][]string
}

// planRooms evaluates the request against the sorted entries. Subscriptions for rooms
// that are not in entries are ignored, as the user may not see them.
func planRooms(sorted []roomEntry, req *Request) plan {
	p := plan{
		counts: make(map[string]int, len(req.Lists)),
		rooms:  make(map[string]RoomConfig),
		byList: make(map[string][]string, len(req.Lists)),
	}
	add := func(roomID string, cfg RoomConfig) {
		cfg = cfg.clamped()
		if existing, ok := p.rooms[roomID]; ok {
			cfg = existing.combine(cfg)
		}
		p.rooms[roomID] = cfg
	}

	for name, list := range req.Lists {
		filtered := make([]roomEntry, 0, len(sorted))
		for _, e := range sorted {
			if list.Filters.matches(e) {
				filtered = append(filtered, e)
			}
		}
		p.counts[name] = len(filtered)
		for _, e := range window(filtered, list.Ranges) {
			add(e.roomID, list.RoomConfig)
			p.byList[name] = append(p.byList[name], e.roomID)
		}
	}

	known := make(map[string]struct{}, len(sorted))
	for _, e := range sorted {
		known[e.roomID] = struct{}{}
	}
	for roomID, cfg := range req.RoomSubscriptions {
		if _, ok := known[roomID]; ok {
			add(roomID, cfg)
		}
	}
	return p
}
