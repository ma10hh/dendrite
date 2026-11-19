// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/eventutil"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/internal"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/streams"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// maxHeroes is how many users a room response names.
const maxHeroes = 5

// membershipInfo returns the member counts of a room and, if it has no name, the users to name it after.
func (h *Handler) membershipInfo(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID, roomID string, unnamed bool,
) (joined, invited int, heroes []Hero, err error) {
	joined, invited, err = snapshot.RoomMemberCounts(ctx, roomID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("loading member counts of %s: %w", roomID, err)
	}
	if !unnamed {
		return joined, invited, nil, nil
	}

	heroIDs, err := snapshot.RoomHeroes(ctx, roomID, userID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("loading heroes of %s: %w", roomID, err)
	}
	if len(heroIDs) > maxHeroes {
		heroIDs = heroIDs[:maxHeroes]
	}
	if len(heroIDs) == 0 {
		return joined, invited, nil, nil
	}
	memberEvents, err := snapshot.StateEventsWithKeys(ctx, roomID, spec.MRoomMember, heroIDs)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("loading hero profiles of %s: %w", roomID, err)
	}
	profiles := make(map[string]*rstypes.HeaderedEvent, len(memberEvents))
	for _, ev := range memberEvents {
		profiles[*ev.StateKey()] = ev
	}
	for _, id := range heroIDs {
		hero := Hero{UserID: id}
		if ev := profiles[id]; ev != nil {
			hero.DisplayName = gjson.GetBytes(ev.Content(), "displayname").String()
			hero.AvatarURL = gjson.GetBytes(ev.Content(), "avatar_url").String()
		}
		heroes = append(heroes, hero)
	}
	return joined, invited, heroes, nil
}

// unreadCounts returns the notification and highlight counts of the rooms that have any.
func (h *Handler) unreadCounts(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID string, roomIDs []string,
) (map[string]*eventutil.NotificationData, error) {
	memberships := make(map[string]string, len(roomIDs))
	for _, roomID := range roomIDs {
		memberships[roomID] = spec.Join
	}
	counts, err := snapshot.GetUserUnreadNotificationCountsForRooms(ctx, userID, memberships)
	if err != nil {
		return nil, fmt.Errorf("loading unread counts: %w", err)
	}
	return counts, nil
}

// roomName returns the m.room.name of a room, which is empty if it has none or an empty one.
func roomName(ev *rstypes.HeaderedEvent) string {
	if ev == nil {
		return ""
	}
	return gjson.GetBytes(ev.Content(), "name").String()
}

// roomAvatar returns the JSON string of the room's avatar URL, or "" if it has none.
func roomAvatar(ev *rstypes.HeaderedEvent) string {
	if ev == nil {
		return ""
	}
	if url := gjson.GetBytes(ev.Content(), "url"); url.Type == gjson.String {
		return url.Raw
	}
	return ""
}

func (h *Handler) clientEvents(ctx context.Context, events []*rstypes.HeaderedEvent) []synctypes.ClientEvent {
	if len(events) == 0 {
		return nil
	}
	return synctypes.ToClientEvents(gomatrixserverlib.ToPDUs(events), synctypes.FormatSync, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return h.rsAPI.QueryUserIDForSender(ctx, roomID, senderID)
	})
}

type roomTimeline struct {
	// events are the timeline events after history visibility was applied.
	events    []synctypes.ClientEvent
	prevBatch string
	limited   bool
	// live is how many of the events are newer than the position the room was last sent at.
	live int
	// raw are the same events as events, which required_state needs to find lazily loaded members.
	raw []*rstypes.HeaderedEvent
}

// timelines fetches the newest events of each room, honouring each room's timeline limit.
// Rooms are fetched in groups of equal limit because the storage layer takes one limit per call.
// liveSince, if it has an entry for a room, makes the timeline count the events newer than that position.
func (h *Handler) timelines(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	roomIDs []string, configs map[string]RoomConfig, liveSince map[string]types.StreamPosition,
) (map[string]roomTimeline, error) {
	result := make(map[string]roomTimeline, len(roomIDs))
	byLimit := map[int][]string{}
	for _, roomID := range roomIDs {
		if limit := configs[roomID].TimelineLimit; limit > 0 {
			byLimit[limit] = append(byLimit[limit], roomID)
		}
	}
	if len(byLimit) == 0 {
		return result, nil
	}

	filter, err := h.timelineFilter(ctx, snapshot, device.UserID)
	if err != nil {
		return nil, err
	}

	// The snapshot is consistent, so there is no need to look up the newest position to bound the range.
	newest := types.Range{From: types.StreamPosition(math.MaxInt64), To: 0, Backwards: true}
	for limit, ids := range byLimit {
		filter.Limit = limit
		recent, err := snapshot.RecentEvents(ctx, ids, newest, &filter, true, true)
		if err != nil {
			return nil, fmt.Errorf("loading recent events: %w", err)
		}
		for _, roomID := range ids {
			var since *types.StreamPosition
			if pos, ok := liveSince[roomID]; ok {
				since = &pos
			}
			timeline, err := h.timeline(ctx, snapshot, device, roomID, recent[roomID], since)
			if err != nil {
				return nil, err
			}
			result[roomID] = timeline
		}
	}
	return result, nil
}

// timelineSince fetches the events of a room newer than since, up to and including upTo, newest ones first
// if there are more than the limit.
func (h *Handler) timelineSince(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	roomID string, since, upTo types.StreamPosition, limit int,
) (roomTimeline, error) {
	filter, err := h.timelineFilter(ctx, snapshot, device.UserID)
	if err != nil {
		return roomTimeline{}, err
	}
	filter.Limit = limit
	recent, err := snapshot.RecentEvents(ctx, []string{roomID}, types.Range{From: upTo, To: since, Backwards: true}, &filter, true, true)
	if err != nil {
		return roomTimeline{}, fmt.Errorf("loading events of %s since %d: %w", roomID, since, err)
	}
	return h.timeline(ctx, snapshot, device, roomID, recent[roomID], &since)
}

// timelineFilter is the event filter shared by all rooms: it hides events from ignored users.
func (h *Handler) timelineFilter(ctx context.Context, snapshot storage.DatabaseTransaction, userID string) (synctypes.RoomEventFilter, error) {
	filter := synctypes.DefaultRoomEventFilter()
	ignored, err := ignoredUsers(ctx, snapshot, userID)
	if err != nil {
		return filter, err
	}
	if len(ignored) > 0 {
		notSenders := make([]string, 0, len(ignored))
		for sender := range ignored {
			notSenders = append(notSenders, sender)
		}
		filter.NotSenders = &notSenders
	}
	return filter, nil
}

// timeline applies history visibility to the events of a room. If liveSince is set, it also counts the
// events that are newer than that position.
func (h *Handler) timeline(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	roomID string, recent types.RecentEvents, liveSince *types.StreamPosition,
) (roomTimeline, error) {
	streamEvents := snapshot.StreamEventsToEvents(ctx, device, recent.Events, h.rsAPI)
	events, err := streams.ApplyHistoryVisibilityFilter(ctx, snapshot, h.rsAPI, roomID, device.UserID, streamEvents)
	if err != nil {
		// Fail closed: never return events that history visibility could not be checked for.
		logrus.WithError(err).WithField("room_id", roomID).Error("history visibility filter failed for sliding sync")
		return roomTimeline{limited: true}, nil
	}

	// Only report a gap if history visibility did not itself remove events.
	limited := recent.Limited && len(events) == len(streamEvents)
	// The aggregations of relations are for timelines the client has nothing to aggregate into: the first one of a
	// room, and the ones with a gap.
	if liveSince == nil || limited {
		if uid, uerr := spec.NewUserID(device.UserID, true); uerr == nil {
			events = internal.BundleRelations(ctx, snapshot, h.rsAPI, *uid, events)
		}
	}

	out := roomTimeline{
		limited: limited,
		events:  h.clientEvents(ctx, events),
		raw:     events,
	}
	if liveSince != nil {
		positions := make(map[string]types.StreamPosition, len(recent.Events))
		for _, ev := range recent.Events {
			positions[ev.EventID()] = ev.StreamPosition
		}
		for _, ev := range events {
			if positions[ev.EventID()] > *liveSince {
				out.live++
			}
		}
	}
	if len(events) > 0 {
		out.prevBatch, err = h.prevBatch(ctx, snapshot, events)
		if err != nil {
			return roomTimeline{}, err
		}
	}
	return out, nil
}

// prevBatch returns a token for paginating backwards from the oldest returned event.
func (h *Handler) prevBatch(ctx context.Context, snapshot storage.DatabaseTransaction, events []*rstypes.HeaderedEvent) (string, error) {
	event := events[0]
	// At the start of the room there is nothing earlier to paginate to, so use the last event instead.
	if event.Type() == spec.MRoomCreate && event.StateKeyEquals("") {
		event = events[len(events)-1]
	}
	depth, streamPos, err := snapshot.PositionInTopology(ctx, event.EventID())
	if err != nil {
		return "", fmt.Errorf("loading topology position: %w", err)
	}
	token := types.TopologyToken{Depth: depth, PDUPosition: streamPos}
	token.Decrement()
	return token.String(), nil
}

// buildLeftRoom builds the response of a room the user was kicked or banned from, or left after receiving it.
// It holds the timeline up to and including that event and nothing else: state is not sent, because the
// room's current state includes changes made after the user lost access to it.
func (h *Handler) buildLeftRoom(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device, entry roomEntry, cfg RoomConfig,
) (RoomResponse, error) {
	room := RoomResponse{Initial: true}
	if entry.isDM {
		isDM := true
		room.IsDM = &isDM
	}
	if entry.activity.LastBump > 0 {
		bump := int64(entry.activity.LastBump)
		room.BumpStamp = &bump
	}
	if cfg.TimelineLimit <= 0 {
		return room, nil
	}

	filter, err := h.timelineFilter(ctx, snapshot, device.UserID)
	if err != nil {
		return room, err
	}
	filter.Limit = cfg.TimelineLimit
	untilLeave := types.Range{From: entry.activity.LastEvent, To: 0, Backwards: true}
	recent, err := snapshot.RecentEvents(ctx, []string{entry.roomID}, untilLeave, &filter, true, true)
	if err != nil {
		return room, fmt.Errorf("loading events before leaving %s: %w", entry.roomID, err)
	}
	timeline, err := h.timeline(ctx, snapshot, device, entry.roomID, recent[entry.roomID], nil)
	if err != nil {
		return room, err
	}
	room.Timeline, room.PrevBatch, room.Limited = timeline.events, timeline.prevBatch, timeline.limited
	return room, nil
}

func avatarJSON(avatar string) json.RawMessage {
	if avatar == "" {
		return nil
	}
	return json.RawMessage(avatar)
}
