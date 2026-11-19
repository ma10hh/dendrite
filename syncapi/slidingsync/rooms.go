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

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// update is what building a response decided to send, and what the connection must remember having sent.
// Nothing is remembered until the response is known to be on its way, so a failed or cancelled request has
// no effect on the connection.
type update struct {
	rooms map[string]RoomResponse
	sent  map[string]*roomSent
}

// buildUpdate decides what to tell the client about each room its lists and subscriptions match.
// A room is only returned if the connection has not received it yet or it changed since.
func (h *Handler) buildUpdate(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	cands *candidates, p plan, sent map[string]*roomSent,
) (*update, error) {
	u := &update{rooms: map[string]RoomResponse{}, sent: map[string]*roomSent{}}
	entries := make(map[string]roomEntry, len(cands.entries))
	for _, e := range cands.entries {
		entries[e.roomID] = e
	}

	var joinedIDs []string
	for roomID, cfg := range p.rooms {
		entry := entries[roomID]
		if entry.kind() == kindJoined {
			joinedIDs = append(joinedIDs, roomID)
			continue
		}
		if prev := sent[roomID]; prev != nil && prev.kind == entry.kind() && prev.token == entry.token {
			continue
		}
		room, err := h.buildOtherRoom(ctx, snapshot, device, entry, cfg, cands)
		if err != nil {
			return nil, err
		}
		u.rooms[roomID] = room
		u.sent[roomID] = &roomSent{kind: entry.kind(), token: entry.token}
	}

	sort.Strings(joinedIDs)
	if len(joinedIDs) > 0 {
		rooms, states, err := h.buildJoinedRooms(ctx, snapshot, device, entries, joinedIDs, p.rooms, sent)
		if err != nil {
			return nil, err
		}
		for roomID, room := range rooms {
			u.rooms[roomID] = room
		}
		for roomID, state := range states {
			u.sent[roomID] = state
		}
	}
	return u, nil
}

// buildOtherRoom builds the response for a room the user is not joined to: an invite, a knock, or a room
// they were removed from. These are sent once, in full.
func (h *Handler) buildOtherRoom(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	entry roomEntry, cfg RoomConfig, cands *candidates,
) (RoomResponse, error) {
	switch entry.kind() {
	case kindInvited:
		return RoomResponse{Initial: true, InviteState: cands.inviteState[entry.roomID]}, nil
	case kindKnocked:
		// Clients have no field for knocks, so they are shown like invites, as Synapse does.
		return RoomResponse{Initial: true, InviteState: cands.knockState[entry.roomID]}, nil
	default:
		return h.buildLeftRoom(ctx, snapshot, device, entry, cfg)
	}
}

// joinedData is everything known about a joined room in this response, and what the connection has of it.
type joinedData struct {
	entry roomEntry
	cfg   RoomConfig
	prev  *roomSent

	// fresh rooms have not been sent as joined rooms before and are sent in full.
	fresh bool
	// dirty rooms have events the connection has not seen.
	dirty bool
	// expanded rooms are asked for more timeline than they were last sent with.
	expanded bool

	notifications, highlights int

	// The rest is only loaded for rooms that are fresh, dirty or expanded.
	name            string
	avatar          string
	heroes          []Hero
	joined, invited int
	timeline        roomTimeline
	state           []*rstypes.HeaderedEvent
}

func (d *joinedData) heavy() bool { return d.fresh || d.dirty || d.expanded }

// buildJoinedRooms builds the responses of joined rooms and the state to remember for each of them.
//
// Rooms without new events are cheap: only notification counts and the direct-message flag, which do
// not come from room events, can have changed. Everything else is only read for rooms that need it.
func (h *Handler) buildJoinedRooms(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	entries map[string]roomEntry, roomIDs []string, configs map[string]RoomConfig, sent map[string]*roomSent,
) (map[string]RoomResponse, map[string]*roomSent, error) {
	counts, err := h.unreadCounts(ctx, snapshot, device.UserID, roomIDs)
	if err != nil {
		return nil, nil, err
	}

	data := make(map[string]*joinedData, len(roomIDs))
	var heavyIDs []string
	for _, roomID := range roomIDs {
		d := &joinedData{entry: entries[roomID], cfg: configs[roomID], prev: sent[roomID]}
		d.fresh = d.prev == nil || d.prev.kind != kindJoined
		if !d.fresh {
			d.dirty = d.entry.activity.LastEvent > d.prev.pos
			d.expanded = d.cfg.TimelineLimit > d.prev.timelineLimit
		}
		if c := counts[roomID]; c != nil {
			d.notifications, d.highlights = c.UnreadNotificationCount, c.UnreadHighlightCount
		}
		data[roomID] = d
		if d.heavy() {
			heavyIDs = append(heavyIDs, roomID)
		}
	}

	if err := h.loadJoinedData(ctx, snapshot, device, data, heavyIDs, configs); err != nil {
		return nil, nil, err
	}

	rooms := make(map[string]RoomResponse, len(roomIDs))
	states := make(map[string]*roomSent, len(roomIDs))
	for _, roomID := range roomIDs {
		room, next, changed := h.renderJoined(ctx, data[roomID])
		states[roomID] = next
		if changed {
			rooms[roomID] = room
		}
	}
	return rooms, states, nil
}

// loadJoinedData reads what is needed to describe the rooms that are fresh, dirty or expanded.
func (h *Handler) loadJoinedData(
	ctx context.Context, snapshot storage.DatabaseTransaction, device *userapi.Device,
	data map[string]*joinedData, heavyIDs []string, configs map[string]RoomConfig,
) error {
	if len(heavyIDs) == 0 {
		return nil
	}
	named, err := snapshot.StateEventsForRooms(ctx, heavyIDs, spec.MRoomName, "")
	if err != nil {
		return fmt.Errorf("loading room names: %w", err)
	}
	avatars, err := snapshot.StateEventsForRooms(ctx, heavyIDs, "m.room.avatar", "")
	if err != nil {
		return fmt.Errorf("loading avatars: %w", err)
	}

	// Fresh and expanded rooms get the newest events; rooms that only gained events get those events.
	var newestIDs []string
	liveSince := map[string]types.StreamPosition{}
	timelines := map[string]roomTimeline{}
	for _, roomID := range heavyIDs {
		d := data[roomID]
		switch {
		case d.fresh:
			newestIDs = append(newestIDs, roomID)
		case d.expanded:
			newestIDs = append(newestIDs, roomID)
			liveSince[roomID] = d.prev.pos
		case d.cfg.TimelineLimit > 0:
			timeline, err := h.timelineSince(ctx, snapshot, device, roomID, d.prev.pos, d.entry.activity.LastEvent, d.cfg.TimelineLimit)
			if err != nil {
				return err
			}
			timelines[roomID] = timeline
		}
	}
	newest, err := h.timelines(ctx, snapshot, device, newestIDs, configs, liveSince)
	if err != nil {
		return err
	}
	for roomID, timeline := range newest {
		timelines[roomID] = timeline
	}

	state, err := h.requiredState(ctx, snapshot, device.UserID, heavyIDs, configs, timelines)
	if err != nil {
		return err
	}

	for _, roomID := range heavyIDs {
		d := data[roomID]
		d.name = roomName(named[roomID])
		d.avatar = roomAvatar(avatars[roomID])
		d.timeline = timelines[roomID]
		d.state = state[roomID]
		d.joined, d.invited, d.heroes, err = h.membershipInfo(ctx, snapshot, device.UserID, roomID, d.name == "")
		if err != nil {
			return err
		}
	}
	return nil
}

// renderJoined compares a room with what the connection was last sent, and returns the response carrying
// the differences, the state to remember, and whether there is anything to send at all.
func (h *Handler) renderJoined(ctx context.Context, d *joinedData) (RoomResponse, *roomSent, bool) {
	next := d.prev.cloneAs(kindJoined)
	room := RoomResponse{Initial: d.fresh}
	changed := d.fresh

	if d.fresh || next.notifications != d.notifications || next.highlights != d.highlights {
		notifications, highlights := d.notifications, d.highlights
		room.NotificationCount, room.HighlightCount = &notifications, &highlights
		changed = true
	}
	next.notifications, next.highlights = d.notifications, d.highlights

	if d.fresh && d.entry.isDM || !d.fresh && next.isDM != d.entry.isDM {
		isDM := d.entry.isDM
		room.IsDM = &isDM
		changed = true
	}
	next.isDM = d.entry.isDM

	if d.heavy() {
		if h.renderHeavy(ctx, d, next, &room) {
			changed = true
		}
		next.pos = d.entry.activity.LastEvent
	}
	next.timelineLimit = d.cfg.TimelineLimit
	return room, next, changed
}

// renderHeavy fills in the parts of a room that come from its events. It reports whether it added anything.
func (h *Handler) renderHeavy(ctx context.Context, d *joinedData, next *roomSent, room *RoomResponse) bool {
	changed := false

	if d.fresh && d.name != "" || !d.fresh && d.name != next.name {
		name := d.name
		room.Name = &name
		changed = true
	}
	next.name = d.name

	if d.fresh && d.avatar != "" || !d.fresh && d.avatar != next.avatar {
		room.Avatar = avatarJSON(d.avatar)
		if d.avatar == "" {
			room.Avatar = json.RawMessage("null")
		}
		changed = true
	}
	next.avatar = d.avatar

	if bump := int64(d.entry.activity.LastBump); bump > 0 && (d.fresh || bump != next.bump) {
		room.BumpStamp = &bump
		changed = true
		next.bump = bump
	}

	if d.fresh || d.joined != next.joined || d.invited != next.invited {
		joined, invited := d.joined, d.invited
		room.JoinedCount, room.InvitedCount = &joined, &invited
		changed = true
	}
	next.joined, next.invited = d.joined, d.invited

	heroIDs := make([]string, 0, len(d.heroes))
	for _, hero := range d.heroes {
		heroIDs = append(heroIDs, hero.UserID)
	}
	if len(d.heroes) > 0 && (d.fresh || !equalStrings(heroIDs, next.heroes)) {
		room.Heroes = d.heroes
		changed = true
	}
	next.heroes = heroIDs

	if len(d.timeline.events) > 0 || d.timeline.limited {
		room.Timeline, room.PrevBatch, room.Limited = d.timeline.events, d.timeline.prevBatch, d.timeline.limited
		if !d.fresh {
			live := d.timeline.live
			room.NumLive = &live
		}
		room.ExpandedTimeline = d.expanded
		changed = true
	}

	if state := next.unsentState(d.state); len(state) > 0 {
		room.RequiredState = h.clientEvents(ctx, state)
		changed = true
	}
	return changed
}

// cloneAs returns a copy of the sent state for editing, or an empty one of the given kind.
func (s *roomSent) cloneAs(kind roomKind) *roomSent {
	if s == nil || s.kind != kind {
		return &roomSent{kind: kind, state: map[stateID]string{}}
	}
	c := *s
	c.state = make(map[stateID]string, len(s.state))
	for id, eventID := range s.state {
		c.state[id] = eventID
	}
	return &c
}

// unsentState returns the events the connection does not have yet, and records them as sent.
func (s *roomSent) unsentState(events []*rstypes.HeaderedEvent) []*rstypes.HeaderedEvent {
	var unsent []*rstypes.HeaderedEvent
	for _, ev := range events {
		if ev.StateKey() == nil {
			continue
		}
		id := stateID{ev.Type(), *ev.StateKey()}
		if s.state[id] != ev.EventID() {
			s.state[id] = ev.EventID()
			unsent = append(unsent, ev)
		}
	}
	return unsent
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
