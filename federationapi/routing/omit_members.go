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

// omitMembers leaves the membership events out of the state and the auth chain of a send_join response, for a server
// that asked for it (MSC3706): joining a big room is slow mostly because of the thousands of membership events. The
// servers that are in the room are listed instead, to ask for the rest.
//
// What stays: the memberships of the users of the joining server, whose own state they are, and the ones that are
// auth events of the join event, which it cannot be checked without.
func omitMembers(
	state, authChain []*types.HeaderedEvent, joinEvent gomatrixserverlib.PDU, origin spec.ServerName,
) (outState, outAuthChain []*types.HeaderedEvent, serversInRoom []string) {
	needed := make(map[string]bool, len(joinEvent.AuthEventIDs()))
	for _, id := range joinEvent.AuthEventIDs() {
		needed[id] = true
	}
	keep := func(ev *types.HeaderedEvent) bool {
		if ev.Type() != spec.MRoomMember || ev.StateKey() == nil || needed[ev.EventID()] {
			return true
		}
		user, err := spec.NewUserID(*ev.StateKey(), true)
		return err == nil && user.Domain() == origin
	}

	servers := map[string]struct{}{}
	for _, ev := range state {
		if ev.Type() == spec.MRoomMember && ev.StateKey() != nil && gjson.GetBytes(ev.Content(), "membership").String() == spec.Join {
			if user, err := spec.NewUserID(*ev.StateKey(), true); err == nil {
				servers[string(user.Domain())] = struct{}{}
			}
		}
		if keep(ev) {
			outState = append(outState, ev)
		}
	}
	for _, ev := range authChain {
		if keep(ev) {
			outAuthChain = append(outAuthChain, ev)
		}
	}
	serversInRoom = make([]string, 0, len(servers))
	for server := range servers {
		serversInRoom = append(serversInRoom, server)
	}
	sort.Strings(serversInRoom)
	return outState, outAuthChain, serversInRoom
}
