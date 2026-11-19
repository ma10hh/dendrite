// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	fsAPI "github.com/element-hq/dendrite/federationapi/api"
	"github.com/element-hq/dendrite/internal/eventutil"
	rsAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/internal/input"
	"github.com/element-hq/dendrite/roomserver/internal/query"
	"github.com/element-hq/dendrite/roomserver/storage"
	"github.com/element-hq/dendrite/roomserver/storage/tables"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
)

// Knocker lets local users knock on rooms, to ask for an invite.
type Knocker struct {
	Cfg   *config.RoomServer
	FSAPI fsAPI.RoomserverFederationAPI
	RSAPI rsAPI.RoomserverInternalAPI
	DB    storage.Database

	Inputer *input.Inputer
	Queryer *query.Queryer
}

// Knock makes the user knock on a room, over federation if this server is not in the room. It returns the output
// events for the consumers of the roomserver that the caller has to produce.
func (r *Knocker) Knock(ctx context.Context, req *rsAPI.PerformKnockRequest) (string, []rsAPI.OutputEvent, error) {
	logger := logrus.WithContext(ctx).WithFields(logrus.Fields{
		"room_id": req.RoomIDOrAlias,
		"user_id": req.UserID,
		"servers": req.ServerNames,
	})
	logger.Info("User requested to knock on a room")
	roomID, outputs, err := r.performKnock(context.Background(), req)
	if err != nil {
		var noRoom eventutil.ErrRoomNoExists
		if errors.As(err, &rsAPI.ErrNotAllowed{}) || errors.As(err, &noRoom) {
			logger.WithError(err).Warn("The knock was refused")
		} else {
			logger.WithError(err).Error("Failed to knock on the room")
		}
		return "", nil, err
	}
	logger.Info("User knocked on the room")
	return roomID, outputs, nil
}
func (r *Knocker) performKnock(ctx context.Context, req *rsAPI.PerformKnockRequest) (string, []rsAPI.OutputEvent, error) {
	userID, err := spec.NewUserID(req.UserID, true)
	if err != nil {
		return "", nil, rsAPI.ErrInvalidID{Err: fmt.Errorf("supplied user ID %q in incorrect format", req.UserID)}
	}
	if !r.Cfg.Matrix.IsLocalServerName(userID.Domain()) {
		return "", nil, rsAPI.ErrInvalidID{Err: fmt.Errorf("user %q does not belong to this homeserver", req.UserID)}
	}

	switch {
	case strings.HasPrefix(req.RoomIDOrAlias, "#"):
		roomID, servers, err := resolveRoomAlias(ctx, r.Cfg, r.FSAPI, r.RSAPI, req.RoomIDOrAlias)
		if err != nil {
			return "", nil, err
		}
		req.ServerNames = append(req.ServerNames, servers...)
		req.RoomIDOrAlias = roomID
	case strings.HasPrefix(req.RoomIDOrAlias, "!"):
	default:
		return "", nil, rsAPI.ErrInvalidID{Err: fmt.Errorf("room ID or alias %q is invalid", req.RoomIDOrAlias)}
	}
	roomID, err := spec.NewRoomID(req.RoomIDOrAlias)
	if err != nil {
		return "", nil, rsAPI.ErrInvalidID{Err: fmt.Errorf("room ID %q is invalid: %w", req.RoomIDOrAlias, err)}
	}

	// The servers to ask for the room do not include this one. The server of the room is always one to ask.
	servers := make([]spec.ServerName, 0, len(req.ServerNames)+1)
	for _, server := range append(req.ServerNames, roomID.Domain()) {
		if !r.Cfg.Matrix.IsLocalServerName(server) && !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	}
	req.ServerNames = servers

	inRoomRes := &rsAPI.QueryServerJoinedToRoomResponse{}
	if err = r.Queryer.QueryServerJoinedToRoom(ctx, &rsAPI.QueryServerJoinedToRoomRequest{RoomID: roomID.String()}, inRoomRes); err != nil {
		return "", nil, fmt.Errorf("r.Queryer.QueryServerJoinedToRoom: %w", err)
	}
	if !inRoomRes.IsInRoom {
		if len(req.ServerNames) == 0 {
			return "", nil, eventutil.ErrRoomNoExists{}
		}
		return r.performFederatedKnock(ctx, req, *userID, *roomID)
	}
	if inRoomRes.RoomVersion == gomatrixserverlib.RoomVersionPseudoIDs {
		return "", nil, rsAPI.ErrNotAllowed{Err: fmt.Errorf("knocking is not supported in rooms with pseudo IDs")}
	}

	sender := userID.String()
	proto := gomatrixserverlib.ProtoEvent{
		Type:     spec.MRoomMember,
		SenderID: sender,
		StateKey: &sender,
		RoomID:   roomID.String(),
	}
	if err = proto.SetUnsigned(struct{}{}); err != nil {
		return "", nil, fmt.Errorf("proto.SetUnsigned: %w", err)
	}
	content := map[string]interface{}{}
	for key, value := range req.Content {
		content[key] = value
	}
	content["membership"] = spec.Knock
	if err = proto.SetContent(content); err != nil {
		return "", nil, fmt.Errorf("proto.SetContent: %w", err)
	}

	identity := r.Cfg.Matrix.SigningIdentity
	var buildRes rsAPI.QueryLatestEventsAndStateResponse
	event, err := eventutil.QueryAndBuildEvent(ctx, &proto, &identity, time.Now(), r.RSAPI, &buildRes)
	if err != nil {
		return "", nil, err
	}

	// The roomserver checks the knock against the rules of the room.
	inputRes := rsAPI.InputRoomEventsResponse{}
	r.Inputer.InputRoomEvents(ctx, &rsAPI.InputRoomEventsRequest{
		InputRoomEvents: []rsAPI.InputRoomEvent{{
			Kind:         rsAPI.KindNew,
			Event:        event,
			SendAsServer: string(userID.Domain()),
		}},
	}, &inputRes)
	if err = inputRes.Err(); err != nil {
		return "", nil, rsAPI.ErrNotAllowed{Err: err}
	}
	return roomID.String(), nil, nil
}

// performFederatedKnock knocks on a room that this server is not in. The servers that are, answer with the state
// of the room that a knocking user may see. The knock goes into the membership table like an invite does, with that
// state, because the roomserver has none of the room: the event cannot be checked against it.
func (r *Knocker) performFederatedKnock(
	ctx context.Context, req *rsAPI.PerformKnockRequest, userID spec.UserID, roomID spec.RoomID,
) (string, []rsAPI.OutputEvent, error) {
	fedReq := fsAPI.PerformKnockRequest{
		RoomID:      roomID.String(),
		UserID:      userID.String(),
		Content:     req.Content,
		ServerNames: req.ServerNames,
	}
	fedRes := fsAPI.PerformKnockResponse{}
	if err := r.FSAPI.PerformKnock(ctx, &fedReq, &fedRes); err != nil {
		var httpErr gomatrix.HTTPError
		if errors.As(err, &httpErr) {
			switch httpErr.Code {
			case http.StatusForbidden:
				return "", nil, rsAPI.ErrNotAllowed{Err: fmt.Errorf("the room does not allow knocking: %s", httpErr.Message)}
			case http.StatusNotFound:
				return "", nil, eventutil.ErrRoomNoExists{}
			}
		}
		return "", nil, err
	}
	if fedRes.RoomVersion == gomatrixserverlib.RoomVersionPseudoIDs {
		return "", nil, rsAPI.ErrNotAllowed{Err: fmt.Errorf("knocking is not supported in rooms with pseudo IDs")}
	}

	// The state goes along with the event for the clients, in the unsigned data, which no signature covers.
	event := fedRes.Event
	if err := event.SetUnsignedField("knock_room_state", fedRes.KnockRoomState); err != nil {
		return "", nil, fmt.Errorf("event.SetUnsignedField: %w", err)
	}

	updater, err := r.DB.MembershipUpdater(ctx, roomID.String(), *event.StateKey(), true, fedRes.RoomVersion)
	if err != nil {
		return "", nil, fmt.Errorf("r.DB.MembershipUpdater: %w", err)
	}
	if _, _, err = updater.Update(tables.MembershipStateKnock, &types.Event{EventNID: 0, PDU: event}); err != nil {
		_ = updater.Rollback()
		return "", nil, fmt.Errorf("updater.Update: %w", err)
	}
	if err = updater.Commit(); err != nil {
		return "", nil, fmt.Errorf("updater.Commit: %w", err)
	}
	return roomID.String(), []rsAPI.OutputEvent{{
		Type: rsAPI.OutputTypeNewKnockEvent,
		NewKnockEvent: &rsAPI.OutputNewKnockEvent{
			RoomVersion: fedRes.RoomVersion,
			Event:       &types.HeaderedEvent{PDU: event},
		},
	}}, nil
}
