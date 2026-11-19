// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"strings"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	roomserver "github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/synctypes"
)

// stateAtEvent returns the room state after the event, limited to the given state keys if there are any.
// ok is false if the roomserver does not have the event, so the caller can fall back to the current state.
func stateAtEvent(
	ctx context.Context, rsAPI roomserver.SyncRoomserverAPI, roomID, eventID string, tuples []gomatrixserverlib.StateKeyTuple,
) (events []*rstypes.HeaderedEvent, ok bool, err error) {
	res := roomserver.QueryStateAfterEventsResponse{}
	err = rsAPI.QueryStateAfterEvents(ctx, &roomserver.QueryStateAfterEventsRequest{
		RoomID:       roomID,
		PrevEventIDs: []string{eventID},
		StateToFetch: tuples,
	}, &res)
	if err != nil {
		return nil, false, err
	}
	if !res.RoomExists || !res.PrevEventsExist {
		return nil, false, nil
	}
	return res.StateEvents, true, nil
}

// memberTuples returns the state keys of the membership events of the senders of the events.
func memberTuples(events []*rstypes.HeaderedEvent) []gomatrixserverlib.StateKeyTuple {
	seen := make(map[spec.SenderID]struct{}, len(events))
	tuples := make([]gomatrixserverlib.StateKeyTuple, 0, len(events))
	for _, ev := range events {
		if _, done := seen[ev.SenderID()]; done {
			continue
		}
		seen[ev.SenderID()] = struct{}{}
		tuples = append(tuples, gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: string(ev.SenderID())})
	}
	return tuples
}

// filterState applies the types and senders of a state filter, which the database does for the current state.
// A type may end in * to match every type with that prefix.
func filterState(events []*rstypes.HeaderedEvent, filter *synctypes.StateFilter) []*rstypes.HeaderedEvent {
	matches := func(patterns *[]string, value string) bool {
		if patterns == nil {
			return false
		}
		for _, p := range *patterns {
			if p == value || (strings.HasSuffix(p, "*") && strings.HasPrefix(value, strings.TrimSuffix(p, "*"))) {
				return true
			}
		}
		return false
	}
	out := make([]*rstypes.HeaderedEvent, 0, len(events))
	for _, ev := range events {
		sender := string(ev.SenderID())
		switch {
		case filter.Types != nil && !matches(filter.Types, ev.Type()):
		case matches(filter.NotTypes, ev.Type()):
		case filter.Senders != nil && !matches(filter.Senders, sender):
		case matches(filter.NotSenders, sender):
		default:
			out = append(out, ev)
		}
	}
	return out
}

// contextState is the state of the room after the event, filtered. It falls back to the current state if the roomserver
// does not know the event.
func contextState(
	ctx context.Context, snapshot storage.DatabaseTransaction, rsAPI roomserver.SyncRoomserverAPI,
	roomID, eventID string, filter *synctypes.StateFilter,
) ([]*rstypes.HeaderedEvent, error) {
	state, ok, err := stateAtEvent(ctx, rsAPI, roomID, eventID, nil)
	if err != nil {
		return nil, err
	}
	if !ok {
		return snapshot.CurrentState(ctx, roomID, filter, nil)
	}
	return filterState(state, filter), nil
}
