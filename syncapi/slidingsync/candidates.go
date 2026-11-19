// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// bumpEventTypes are the event types that advance a room's bump_stamp (MSC4186).
var bumpEventTypes = []string{
	spec.MRoomCreate, "m.room.message", "m.room.encrypted", "m.sticker", "m.call.invite", "m.poll.start", "m.beacon_info",
}

// candidates is every room the user can see in a list, together with data needed to render invites.
type candidates struct {
	entries     []roomEntry
	inviteState map[string][]json.RawMessage
	// knockState is the stripped state shown for rooms the user has knocked on.
	knockState map[string][]json.RawMessage
}

// loadCandidates gathers the user's joined and invited rooms with the attributes that lists filter on.
// Only attributes the request actually filters on are looked up, except is_dm which is always returned.
func (h *Handler) loadCandidates(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID string, needs attributeNeeds,
	sent map[string]*roomSent,
) (*candidates, error) {
	memberships, err := snapshot.RoomIDsWithAnyMembership(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("loading memberships: %w", err)
	}
	var joinedIDs []string
	for roomID, membership := range memberships {
		if membership == spec.Join {
			joinedIDs = append(joinedIDs, roomID)
		}
	}

	directRooms, err := h.directRooms(ctx, userID)
	if err != nil {
		return nil, err
	}

	c := &candidates{inviteState: map[string][]json.RawMessage{}, knockState: map[string][]json.RawMessage{}}
	if err = h.addInvites(ctx, snapshot, userID, memberships, directRooms, c); err != nil {
		return nil, err
	}

	activity, err := snapshot.RoomActivity(ctx, joinedIDs, bumpEventTypes)
	if err != nil {
		return nil, fmt.Errorf("loading room activity: %w", err)
	}
	for _, roomID := range joinedIDs {
		c.entries = append(c.entries, roomEntry{roomID: roomID, activity: activity[roomID], isDM: directRooms[roomID]})
	}

	if err = h.addLeftAndKnocked(ctx, snapshot, userID, memberships, directRooms, sent, c); err != nil {
		return nil, err
	}
	if err = applyFilterState(ctx, snapshot, c, needs); err != nil {
		return nil, err
	}
	return c, nil
}

// directRooms returns the set of rooms listed in the user's m.direct account data.
func (h *Handler) directRooms(ctx context.Context, userID string) (map[string]bool, error) {
	var res userapi.QueryAccountDataResponse
	if err := h.userAPI.QueryAccountData(ctx, &userapi.QueryAccountDataRequest{UserID: userID, DataType: "m.direct"}, &res); err != nil {
		return nil, fmt.Errorf("loading m.direct: %w", err)
	}
	rooms := map[string]bool{}
	raw, ok := res.GlobalAccountData["m.direct"]
	if !ok {
		return rooms, nil
	}
	var byUser map[string][]string
	if err := json.Unmarshal(raw, &byUser); err != nil {
		// A malformed m.direct must not break sync; treat it as "no DMs".
		logrus.WithError(err).WithField("user_id", userID).Warn("ignoring malformed m.direct")
		return rooms, nil
	}
	for _, roomIDs := range byUser {
		for _, roomID := range roomIDs {
			rooms[roomID] = true
		}
	}
	return rooms, nil
}

// addInvites adds the user's pending invites, skipping those from ignored users and those for rooms already joined.
func (h *Handler) addInvites(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID string,
	memberships map[string]string, directRooms map[string]bool, c *candidates,
) error {
	maxInvite, err := snapshot.MaxStreamPositionForInvites(ctx)
	if err != nil {
		return fmt.Errorf("loading invite position: %w", err)
	}
	invites, _, _, err := snapshot.InviteEventsInRange(ctx, userID, types.Range{From: 0, To: maxInvite})
	if err != nil {
		return fmt.Errorf("loading invites: %w", err)
	}
	ignored, err := ignoredUsers(ctx, snapshot, userID)
	if err != nil {
		return err
	}

	for roomID, invite := range invites {
		if memberships[roomID] == spec.Join {
			continue
		}
		if sender, err := h.rsAPI.QueryUserIDForSender(ctx, invite.RoomID(), invite.SenderID()); err == nil && sender != nil {
			if _, isIgnored := ignored[sender.String()]; isIgnored {
				continue
			}
		}
		res, err := types.NewInviteResponse(ctx, h.rsAPI, invite, synctypes.FormatSync)
		if err != nil {
			logrus.WithError(err).WithField("room_id", roomID).Error("failed to build invite state")
			continue
		}
		entry := roomEntry{roomID: roomID, invited: true, token: invite.EventID(), inviteTS: invite.OriginServerTS(), isDM: directRooms[roomID]}
		for _, raw := range res.InviteState.Events {
			switch gjson.GetBytes(raw, "type").String() {
			case spec.MRoomCreate:
				entry.roomType = roomTypeOf(spec.RawJSON(gjson.GetBytes(raw, "content").Raw))
			case "m.room.encryption":
				entry.isEncrypted = true
			}
		}
		c.entries = append(c.entries, entry)
		c.inviteState[roomID] = res.InviteState.Events
	}
	return nil
}

// loadFilterState batch-loads the state that room filters depend on.
func loadFilterState(
	ctx context.Context, snapshot storage.DatabaseTransaction, roomIDs []string, needs attributeNeeds,
) (encrypted map[string]*rstypes.HeaderedEvent, create map[string]*rstypes.HeaderedEvent, err error) {
	if needs.encrypted {
		if encrypted, err = snapshot.StateEventsForRooms(ctx, roomIDs, "m.room.encryption", ""); err != nil {
			return nil, nil, fmt.Errorf("loading encryption state: %w", err)
		}
	}
	if needs.roomType {
		if create, err = snapshot.StateEventsForRooms(ctx, roomIDs, spec.MRoomCreate, ""); err != nil {
			return nil, nil, fmt.Errorf("loading create events: %w", err)
		}
	}
	return encrypted, create, nil
}

// roomTypeOf extracts `type` from the content of an m.room.create event, nil if it has none.
func roomTypeOf(content spec.RawJSON) *string {
	t := gjson.GetBytes(content, "type")
	if !t.Exists() || t.Type != gjson.String {
		return nil
	}
	s := t.String()
	return &s
}

// ignoredUsers returns the set of users the user ignores. Having no ignore list is not an error.
func ignoredUsers(ctx context.Context, snapshot storage.DatabaseTransaction, userID string) (map[string]interface{}, error) {
	ignores, err := snapshot.IgnoresForUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading ignored users: %w", err)
	}
	return ignores.List, nil
}

// applyFilterState sets the filter attributes of rooms whose state this server holds, that is, every room
// except invites, whose attributes come from their stripped state, and knocks.
func applyFilterState(ctx context.Context, snapshot storage.DatabaseTransaction, c *candidates, needs attributeNeeds) error {
	var roomIDs []string
	for _, e := range c.entries {
		if !e.invited && !e.knocked {
			roomIDs = append(roomIDs, e.roomID)
		}
	}
	encrypted, create, err := loadFilterState(ctx, snapshot, roomIDs, needs)
	if err != nil {
		return err
	}
	for i := range c.entries {
		e := &c.entries[i]
		if e.invited || e.knocked {
			continue
		}
		_, e.isEncrypted = encrypted[e.roomID]
		if ev := create[e.roomID]; ev != nil {
			e.roomType = roomTypeOf(ev.Content())
		}
	}
	return nil
}
