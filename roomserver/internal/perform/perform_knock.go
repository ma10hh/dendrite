// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	fsAPI "github.com/element-hq/dendrite/federationapi/api"
	"github.com/element-hq/dendrite/internal/eventutil"
	rsAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/internal/input"
	"github.com/element-hq/dendrite/roomserver/internal/query"
	"github.com/element-hq/dendrite/roomserver/storage"
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

// PerformKnock makes the user knock on a room, over federation if this server is not in the room.
func (r *Knocker) PerformKnock(ctx context.Context, req *rsAPI.PerformKnockRequest) (string, error) {
	logger := logrus.WithContext(ctx).WithFields(logrus.Fields{
		"room_id": req.RoomIDOrAlias,
		"user_id": req.UserID,
		"servers": req.ServerNames,
	})
	logger.Info("User requested to knock on a room")
	roomID, err := r.performKnock(context.Background(), req)
	if err != nil {
		var noRoom eventutil.ErrRoomNoExists
		if errors.As(err, &rsAPI.ErrNotAllowed{}) || errors.As(err, &noRoom) {
			logger.WithError(err).Warn("The knock was refused")
		} else {
			logger.WithError(err).Error("Failed to knock on the room")
		}
		return "", err
	}
	logger.Info("User knocked on the room")
	return roomID, nil
}

func (r *Knocker) performKnock(ctx context.Context, req *rsAPI.PerformKnockRequest) (string, error) {
	userID, err := spec.NewUserID(req.UserID, true)
	if err != nil {
		return "", rsAPI.ErrInvalidID{Err: fmt.Errorf("supplied user ID %q in incorrect format", req.UserID)}
	}
	if !r.Cfg.Matrix.IsLocalServerName(userID.Domain()) {
		return "", rsAPI.ErrInvalidID{Err: fmt.Errorf("user %q does not belong to this homeserver", req.UserID)}
	}

	switch {
	case strings.HasPrefix(req.RoomIDOrAlias, "#"):
		roomID, servers, err := resolveRoomAlias(ctx, r.Cfg, r.FSAPI, r.RSAPI, req.RoomIDOrAlias)
		if err != nil {
			return "", err
		}
		req.ServerNames = append(req.ServerNames, servers...)
		req.RoomIDOrAlias = roomID
	case strings.HasPrefix(req.RoomIDOrAlias, "!"):
	default:
		return "", rsAPI.ErrInvalidID{Err: fmt.Errorf("room ID or alias %q is invalid", req.RoomIDOrAlias)}
	}
	roomID, err := spec.NewRoomID(req.RoomIDOrAlias)
	if err != nil {
		return "", rsAPI.ErrInvalidID{Err: fmt.Errorf("room ID %q is invalid: %w", req.RoomIDOrAlias, err)}
	}

	// The servers to ask for the room do not include this one.
	servers := make([]spec.ServerName, 0, len(req.ServerNames))
	for _, server := range req.ServerNames {
		if !r.Cfg.Matrix.IsLocalServerName(server) {
			servers = append(servers, server)
		}
	}
	req.ServerNames = servers

	inRoomRes := &rsAPI.QueryServerJoinedToRoomResponse{}
	if err = r.Queryer.QueryServerJoinedToRoom(ctx, &rsAPI.QueryServerJoinedToRoomRequest{RoomID: roomID.String()}, inRoomRes); err != nil {
		return "", fmt.Errorf("r.Queryer.QueryServerJoinedToRoom: %w", err)
	}
	if !inRoomRes.IsInRoom {
		if len(req.ServerNames) == 0 {
			return "", eventutil.ErrRoomNoExists{}
		}
		return "", rsAPI.ErrNotAllowed{Err: fmt.Errorf("knocking on rooms of other servers is not supported by this server")}
	}
	if inRoomRes.RoomVersion == gomatrixserverlib.RoomVersionPseudoIDs {
		return "", rsAPI.ErrNotAllowed{Err: fmt.Errorf("knocking is not supported in rooms with pseudo IDs")}
	}

	sender := userID.String()
	proto := gomatrixserverlib.ProtoEvent{
		Type:     spec.MRoomMember,
		SenderID: sender,
		StateKey: &sender,
		RoomID:   roomID.String(),
	}
	if err = proto.SetUnsigned(struct{}{}); err != nil {
		return "", fmt.Errorf("proto.SetUnsigned: %w", err)
	}
	content := map[string]interface{}{}
	for key, value := range req.Content {
		content[key] = value
	}
	content["membership"] = spec.Knock
	if err = proto.SetContent(content); err != nil {
		return "", fmt.Errorf("proto.SetContent: %w", err)
	}

	identity := r.Cfg.Matrix.SigningIdentity
	var buildRes rsAPI.QueryLatestEventsAndStateResponse
	event, err := eventutil.QueryAndBuildEvent(ctx, &proto, &identity, time.Now(), r.RSAPI, &buildRes)
	if err != nil {
		return "", err
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
		return "", rsAPI.ErrNotAllowed{Err: err}
	}
	return roomID.String(), nil
}
