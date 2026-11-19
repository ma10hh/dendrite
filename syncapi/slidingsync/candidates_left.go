// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/types"
)

// strippedStateTypes are the room state types a user who is not in a room may be shown (the invite_room_state set).
var strippedStateTypes = []string{
	spec.MRoomCreate, spec.MRoomName, "m.room.avatar", spec.MRoomTopic, spec.MRoomJoinRules, spec.MRoomCanonicalAlias, "m.room.encryption",
}

// addLeftAndKnocked adds the rooms the user was kicked or banned from, and the rooms they have knocked on.
//
// A room the user left on their own is only listed if the connection had already received it, so the client
// learns that it is gone. A kick is a leave sent by somebody else, and is always listed.
// A ban is only listed if the user was joined just before it, so rooms they were banned from without ever
// having been in them stay hidden.
func (h *Handler) addLeftAndKnocked(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID string,
	memberships map[string]string, directRooms map[string]bool, sent map[string]*roomSent, c *candidates,
) error {
	var roomIDs []string
	for roomID, membership := range memberships {
		if membership == spec.Leave || membership == spec.Ban || membership == spec.Knock {
			roomIDs = append(roomIDs, roomID)
		}
	}
	if len(roomIDs) == 0 {
		return nil
	}
	sort.Strings(roomIDs)
	memberEvents, err := snapshot.StateEventsForRooms(ctx, roomIDs, spec.MRoomMember, userID)
	if err != nil {
		return fmt.Errorf("loading own memberships: %w", err)
	}

	knocks := map[string]*rstypes.HeaderedEvent{}
	for _, roomID := range roomIDs {
		ev := memberEvents[roomID]
		if ev == nil || ev.StateKey() == nil {
			continue
		}
		pos := eventPosition(ctx, snapshot, ev)
		entry := roomEntry{roomID: roomID, token: ev.EventID(), isDM: directRooms[roomID], activity: types.RoomActivity{LastEvent: pos}}

		switch memberships[roomID] {
		case spec.Knock:
			entry.knocked = true
			knocks[roomID] = ev
		case spec.Leave:
			selfLeft := ev.SenderID() == spec.SenderID(*ev.StateKey())
			if _, wasSent := sent[roomID]; selfLeft && !wasSent {
				continue
			}
			entry.left, entry.activity.LastBump = true, pos
		case spec.Ban:
			before, _, err := snapshot.SelectMembershipForUser(ctx, roomID, userID, int64(pos)-1)
			if err != nil {
				return fmt.Errorf("loading membership before ban in %s: %w", roomID, err)
			}
			if before != spec.Join {
				continue
			}
			entry.left, entry.activity.LastBump = true, pos
		}
		c.entries = append(c.entries, entry)
	}
	return h.addKnockState(ctx, snapshot, knocks, c)
}

// eventPosition returns the stream position of an event, or 0 if this server does not have it in its timeline.
func eventPosition(ctx context.Context, snapshot storage.DatabaseTransaction, ev *rstypes.HeaderedEvent) types.StreamPosition {
	_, pos, err := snapshot.PositionInTopology(ctx, ev.EventID())
	if err != nil {
		logrus.WithError(err).WithField("event_id", ev.EventID()).Debug("no stream position for event")
		return 0
	}
	return pos
}

// addKnockState builds the stripped state shown for each room the user has knocked on: the room's public state
// as seen by this server, or the state the remote server attached to the knock, followed by the knock itself.
func (h *Handler) addKnockState(
	ctx context.Context, snapshot storage.DatabaseTransaction, knocks map[string]*rstypes.HeaderedEvent, c *candidates,
) error {
	if len(knocks) == 0 {
		return nil
	}
	roomIDs := make([]string, 0, len(knocks))
	for roomID := range knocks {
		roomIDs = append(roomIDs, roomID)
	}
	sort.Strings(roomIDs)

	public := make(map[string]map[string]*rstypes.HeaderedEvent, len(strippedStateTypes))
	for _, evType := range strippedStateTypes {
		events, err := snapshot.StateEventsForRooms(ctx, roomIDs, evType, "")
		if err != nil {
			return fmt.Errorf("loading %s of knocked rooms: %w", evType, err)
		}
		public[evType] = events
	}

	for _, roomID := range roomIDs {
		knock := knocks[roomID]
		var state []json.RawMessage
		if attached := gjson.GetBytes(knock.Unsigned(), "knock_room_state"); attached.IsArray() {
			for _, ev := range attached.Array() {
				state = append(state, json.RawMessage(ev.Raw))
			}
		} else {
			for _, evType := range strippedStateTypes {
				if ev := public[evType][roomID]; ev != nil {
					state = append(state, h.strippedEvent(ctx, ev))
				}
			}
		}
		c.knockState[roomID] = append(state, h.strippedEvent(ctx, knock))
	}
	return nil
}

// strippedEvent returns the event reduced to the fields shown to users who are not in the room.
func (h *Handler) strippedEvent(ctx context.Context, ev *rstypes.HeaderedEvent) json.RawMessage {
	client := h.clientEvents(ctx, []*rstypes.HeaderedEvent{ev})
	if len(client) == 0 {
		return nil
	}
	stateKey := ""
	if client[0].StateKey != nil {
		stateKey = *client[0].StateKey
	}
	raw, err := json.Marshal(struct {
		Type     string          `json:"type"`
		StateKey string          `json:"state_key"`
		Sender   string          `json:"sender"`
		Content  json.RawMessage `json:"content"`
	}{client[0].Type, stateKey, client[0].Sender, json.RawMessage(client[0].Content)})
	if err != nil {
		return nil
	}
	return raw
}
