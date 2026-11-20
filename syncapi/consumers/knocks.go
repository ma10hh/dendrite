// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"fmt"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/sjson"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/types"
)

// onNewKnockEvent stores the knock of a local user on a room that this server is not in as the membership of the user
// in that room, where the sync API finds it like the knock on a room of this server. The roomserver has none of the
// state of the room, so this is all the room has here, and the stripped state the other server sent is in the event.
func (s *OutputRoomEventConsumer) onNewKnockEvent(ctx context.Context, msg api.OutputNewKnockEvent) error {
	ev := msg.Event
	userID, err := s.rsAPI.QueryUserIDForSender(ctx, ev.RoomID(), ev.SenderID())
	if err != nil {
		return err
	}
	if userID == nil || !s.cfg.Matrix.IsLocalServerName(userID.Domain()) {
		return nil
	}
	if ev, err = s.updateStateEvent(ev); err != nil {
		return err
	}
	ev.UserID = *userID

	pduPos, err := s.db.WriteEvent(ctx, ev, []*rstypes.HeaderedEvent{ev}, []string{ev.EventID()}, nil, nil, true, gomatrixserverlib.HistoryVisibilityShared)
	if err != nil {
		return fmt.Errorf("writing the knock: %w", err)
	}
	s.pduStream.Advance(pduPos)
	s.notifier.OnNewEvent(ev, ev.RoomID().String(), nil, types.StreamingToken{PDUPosition: pduPos})
	return nil
}

// onRetireKnockEvent ends such a knock. If the user withdrew it, the user is shown as having left the room, so that
// clients drop it. If an invite answered it, the invite takes its place.
func (s *OutputRoomEventConsumer) onRetireKnockEvent(ctx context.Context, msg api.OutputRetireKnockEvent) error {
	roomID, err := spec.NewRoomID(msg.RoomID)
	if err != nil {
		return err
	}
	userID, err := s.rsAPI.QueryUserIDForSender(ctx, *roomID, msg.TargetSenderID)
	if err != nil || userID == nil {
		return err
	}
	if msg.Membership != spec.Leave {
		// The room only has the knock in the sync API, so it goes with all of its state.
		return s.db.PurgeRoomState(ctx, msg.RoomID)
	}

	knock, err := s.currentMembership(ctx, msg.RoomID, userID.String())
	if err != nil || knock == nil {
		return err
	}
	raw, err := sjson.SetBytes(knock.JSON(), "content", map[string]string{"membership": spec.Leave})
	if err != nil {
		return err
	}
	// Not an event of the room, which the sync API has no use of beyond its membership: a later event than the knock.
	for path, value := range map[string]any{
		"origin_server_ts": time.Now().UnixMilli(),
		"depth":            knock.Depth() + 1,
	} {
		if raw, err = sjson.SetBytes(raw, path, value); err != nil {
			return err
		}
	}
	if raw, err = sjson.DeleteBytes(raw, "unsigned"); err != nil {
		return err
	}
	pdu, err := gomatrixserverlib.MustGetRoomVersion(knock.Version()).NewEventFromTrustedJSON(raw, false)
	if err != nil {
		return err
	}
	leave, err := s.updateStateEvent(&rstypes.HeaderedEvent{PDU: pdu})
	if err != nil {
		return err
	}
	leave.UserID = *userID

	pduPos, err := s.db.WriteEvent(ctx, leave, []*rstypes.HeaderedEvent{leave}, []string{leave.EventID()}, []string{knock.EventID()}, nil, true, gomatrixserverlib.HistoryVisibilityShared)
	if err != nil {
		return fmt.Errorf("writing the end of the knock: %w", err)
	}
	s.pduStream.Advance(pduPos)
	s.notifier.OnNewEvent(leave, leave.RoomID().String(), nil, types.StreamingToken{PDUPosition: pduPos})
	return nil
}

func (s *OutputRoomEventConsumer) currentMembership(ctx context.Context, roomID, userID string) (*rstypes.HeaderedEvent, error) {
	snapshot, err := s.db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var succeeded bool
	defer sqlutil.EndTransactionWithCheck(snapshot, &succeeded, &err)
	ev, err := snapshot.GetStateEvent(ctx, roomID, spec.MRoomMember, userID)
	succeeded = err == nil
	return ev, err
}

// onMembersAdded stores the memberships that a room joined with partial state was missing. They are as old as the
// join, so they are stored at its position: clients are not told about a change in the room, and the members show in
// the state that is read afterwards.
func (s *OutputRoomEventConsumer) onMembersAdded(ctx context.Context, msg api.OutputMembersAdded) error {
	if len(msg.Members) == 0 {
		return nil
	}
	snapshot, err := s.db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return err
	}
	topo, pos, err := snapshot.PositionInTopology(ctx, msg.JoinEventID)
	_ = snapshot.Rollback()
	if err != nil {
		return fmt.Errorf("the position of the join %s: %w", msg.JoinEventID, err)
	}

	members := make([]*rstypes.HeaderedEvent, 0, len(msg.Members))
	for _, ev := range msg.Members {
		if ev, err = s.updateStateEvent(ev); err != nil {
			return err
		}
		userID, uerr := s.rsAPI.QueryUserIDForSender(ctx, ev.RoomID(), ev.SenderID())
		if uerr != nil || userID == nil {
			continue
		}
		ev.UserID = *userID
		members = append(members, ev)
	}
	return s.db.AddResyncedMembers(ctx, members, pos, topo)
}
