// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"fmt"
	"sort"

	"github.com/matrix-org/gomatrixserverlib/spec"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/synctypes"
)

type stateID struct{ evType, stateKey string }

// roomState collects the state events found for each room, without duplicates.
type roomState map[string]map[stateID]*rstypes.HeaderedEvent

func (s roomState) add(roomID string, events ...*rstypes.HeaderedEvent) {
	for _, ev := range events {
		if ev == nil || ev.StateKey() == nil {
			continue
		}
		if s[roomID] == nil {
			s[roomID] = map[stateID]*rstypes.HeaderedEvent{}
		}
		s[roomID][stateID{ev.Type(), *ev.StateKey()}] = ev
	}
}

// sorted returns each room's events ordered by type and state key, so responses are stable.
func (s roomState) sorted() map[string][]*rstypes.HeaderedEvent {
	out := make(map[string][]*rstypes.HeaderedEvent, len(s))
	for roomID, events := range s {
		ids := make([]stateID, 0, len(events))
		for id := range events {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if ids[i].evType != ids[j].evType {
				return ids[i].evType < ids[j].evType
			}
			return ids[i].stateKey < ids[j].stateKey
		})
		for _, id := range ids {
			out[roomID] = append(out[roomID], events[id])
		}
	}
	return out
}

// requiredState returns the current state events that each room's required_state asks for.
//
// Exact `(type, state_key)` pairs are looked up for all rooms at once, one query per distinct pair.
// Only rooms that use wildcards need their state read per room. Lazy-loaded memberships come from the
// room's timeline: its senders and the targets of its membership events.
func (h *Handler) requiredState(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID string,
	roomIDs []string, configs map[string]RoomConfig, timelines map[string]roomTimeline,
) (map[string][]*rstypes.HeaderedEvent, error) {
	found := roomState{}
	matchers := make(map[string]*stateMatcher, len(roomIDs))
	roomsByPair := map[StateKeyPair][]string{}

	for _, roomID := range roomIDs {
		if len(configs[roomID].RequiredState) == 0 {
			continue
		}
		m := newStateMatcher(configs[roomID].RequiredState, userID)
		matchers[roomID] = m
		if m.needsAllState() {
			continue
		}
		for _, pair := range m.exactPairs() {
			roomsByPair[pair] = append(roomsByPair[pair], roomID)
		}
	}

	pairs := make([]StateKeyPair, 0, len(roomsByPair))
	for pair := range roomsByPair {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	for _, pair := range pairs {
		events, err := snapshot.StateEventsForRooms(ctx, roomsByPair[pair], pair[0], pair[1])
		if err != nil {
			return nil, fmt.Errorf("loading %s state: %w", pair[0], err)
		}
		for roomID, ev := range events {
			found.add(roomID, ev)
		}
	}

	for _, roomID := range roomIDs {
		m := matchers[roomID]
		if m == nil {
			continue
		}
		if err := h.addScannedState(ctx, snapshot, roomID, m, found); err != nil {
			return nil, err
		}
		if m.lazy && !m.coversAllMembers() {
			if err := h.addLazyMembers(ctx, snapshot, roomID, timelines[roomID], found); err != nil {
				return nil, err
			}
		}
	}
	return found.sorted(), nil
}

// addScannedState adds the state that cannot be looked up by exact key: everything for rooms that request
// all state or use a wildcard event type, and every event of the types requested with a wildcard state key.
func (h *Handler) addScannedState(
	ctx context.Context, snapshot storage.DatabaseTransaction, roomID string, m *stateMatcher, found roomState,
) error {
	filter := synctypes.DefaultStateFilter()
	if !m.needsAllState() {
		wildcardTypes := m.wildcardKeyTypes()
		if len(wildcardTypes) == 0 {
			return nil
		}
		filter.Types = &wildcardTypes
	}
	events, err := snapshot.CurrentState(ctx, roomID, &filter, nil)
	if err != nil {
		return fmt.Errorf("scanning state of %s: %w", roomID, err)
	}
	for _, ev := range events {
		if ev.StateKey() != nil && m.matches(ev.Type(), *ev.StateKey()) {
			found.add(roomID, ev)
		}
	}
	return nil
}

// addLazyMembers adds the membership events of everyone the timeline needs to be rendered.
func (h *Handler) addLazyMembers(
	ctx context.Context, snapshot storage.DatabaseTransaction, roomID string, timeline roomTimeline, found roomState,
) error {
	room, err := spec.NewRoomID(roomID)
	if err != nil {
		return fmt.Errorf("room ID %q: %w", roomID, err)
	}
	var senders, targets []string
	for _, ev := range timeline.raw {
		if id, err := h.rsAPI.QueryUserIDForSender(ctx, *room, ev.SenderID()); err == nil && id != nil {
			senders = append(senders, id.String())
		}
		if ev.Type() == spec.MRoomMember && ev.StateKey() != nil {
			if id, err := h.rsAPI.QueryUserIDForSender(ctx, *room, spec.SenderID(*ev.StateKey())); err == nil && id != nil {
				targets = append(targets, id.String())
			}
		}
	}
	users := lazyMembers(senders, targets)
	if len(users) == 0 {
		return nil
	}
	members, err := snapshot.StateEventsWithKeys(ctx, roomID, spec.MRoomMember, users)
	if err != nil {
		return fmt.Errorf("loading lazy members of %s: %w", roomID, err)
	}
	found.add(roomID, members...)
	return nil
}
