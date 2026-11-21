// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"sort"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/roomserver/types"
)

// maxHeroes is how many memberships besides the required ones stay in the state, for the name of the room.
const maxHeroes = 5

// heroMemberships picks the memberships of the heroes of the room from its state: users that are joined or invited,
// other than the one who joins, ordered by the time of their membership and then by user ID.
func heroMemberships(state []*types.HeaderedEvent, joining string) map[string]bool {
	var members []*types.HeaderedEvent
	for _, ev := range state {
		if ev.Type() != spec.MRoomMember || ev.StateKey() == nil || *ev.StateKey() == joining {
			continue
		}
		if m := gjson.GetBytes(ev.Content(), "membership").String(); m == spec.Join || m == spec.Invite {
			members = append(members, ev)
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		if a, b := members[i].OriginServerTS(), members[j].OriginServerTS(); a != b {
			return a < b
		}
		return *members[i].StateKey() < *members[j].StateKey()
	})
	heroes := map[string]bool{}
	for i := 0; i < len(members) && i < maxHeroes; i++ {
		heroes[members[i].EventID()] = true
	}
	return heroes
}

// omitMembers leaves the membership events out of the state of a send_join response, for a server that asked for it
// (MSC3706): joining a big room is slow mostly because of the thousands of membership events. The servers that are in
// the room are listed instead, to ask for the rest.
//
// What stays in the state: the memberships of the users of the joining server, whose own state they are, the ones
// that are auth events of the join event, and the memberships of the room's heroes (at most maxHeroes joined or invited
// users other than the one joining, the longest members first), with which the joining server can name the room before
// it has the rest. The auth chain stays complete for what is left: a membership that some other
// event in the response has as an auth event, directly or through other auth events, moves to the auth chain, as the
// events cannot be checked without it.
func omitMembers(
	state, authChain []*types.HeaderedEvent, joinEvent gomatrixserverlib.PDU, origin spec.ServerName,
) (outState, outAuthChain []*types.HeaderedEvent, serversInRoom []string) {
	needed := make(map[string]bool, len(joinEvent.AuthEventIDs()))
	for _, id := range joinEvent.AuthEventIDs() {
		needed[id] = true
	}
	isMember := func(ev *types.HeaderedEvent) bool { return ev.Type() == spec.MRoomMember && ev.StateKey() != nil }
	heroes := heroMemberships(state, func() string {
		if key := joinEvent.StateKey(); key != nil {
			return *key
		}
		return ""
	}())
	keepInState := func(ev *types.HeaderedEvent) bool {
		if !isMember(ev) || needed[ev.EventID()] || heroes[ev.EventID()] {
			return true
		}
		user, err := spec.NewUserID(*ev.StateKey(), true)
		return err == nil && user.Domain() == origin
	}

	servers := map[string]struct{}{}
	kept := map[string]bool{}
	var dropped []*types.HeaderedEvent
	for _, ev := range state {
		if isMember(ev) && gjson.GetBytes(ev.Content(), "membership").String() == spec.Join {
			if user, err := spec.NewUserID(*ev.StateKey(), true); err == nil {
				servers[string(user.Domain())] = struct{}{}
			}
		}
		if keepInState(ev) {
			outState = append(outState, ev)
			kept[ev.EventID()] = true
		} else {
			dropped = append(dropped, ev)
		}
	}
	var candidates []*types.HeaderedEvent
	for _, ev := range authChain {
		if keepInState(ev) {
			outAuthChain = append(outAuthChain, ev)
			kept[ev.EventID()] = true
		} else {
			candidates = append(candidates, ev)
		}
	}
	candidates = append(candidates, dropped...)

	// Memberships that the events that stay have as auth events, and so on for their own auth events.
	byID := make(map[string]*types.HeaderedEvent, len(candidates))
	for _, ev := range candidates {
		byID[ev.EventID()] = ev
	}
	queue := make([]*types.HeaderedEvent, 0, len(kept))
	queue = append(queue, outState...)
	queue = append(queue, outAuthChain...)
	for len(queue) > 0 {
		ev := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, id := range ev.AuthEventIDs() {
			if auth, ok := byID[id]; ok && !kept[id] {
				kept[id] = true
				outAuthChain = append(outAuthChain, auth)
				queue = append(queue, auth)
			}
		}
	}

	// An event that is in the state is not repeated in the auth chain, which is only for what the state lacks.
	inState := make(map[string]bool, len(outState))
	for _, ev := range outState {
		inState[ev.EventID()] = true
	}
	chain := outAuthChain[:0]
	for _, ev := range outAuthChain {
		if !inState[ev.EventID()] {
			chain = append(chain, ev)
		}
	}
	outAuthChain = chain

	serversInRoom = make([]string, 0, len(servers))
	for server := range servers {
		serversInRoom = append(serversInRoom, server)
	}
	sort.Strings(serversInRoom)
	return outState, outAuthChain, serversInRoom
}
