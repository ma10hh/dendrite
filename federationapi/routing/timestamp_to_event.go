// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"errors"
	"net/http"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/roomserver/api"
)

// TimestampToEvent handles GET /timestamp_to_event/{roomID}: the event of the room that is closest to a time, for a
// server that is in the room.
func TimestampToEvent(
	httpReq *http.Request,
	request *fclient.FederationRequest,
	rsAPI api.FederationRoomserverAPI,
	roomID string,
) util.JSONResponse {
	ctx := httpReq.Context()
	if _, err := spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Bad room ID: " + err.Error())}
	}
	ts, forward, errRes := httputil.ParseTimestampToEventQuery(httpReq)
	if errRes != nil {
		return *errRes
	}
	if errRes = ErrorIfLocalServerNotInRoom(ctx, rsAPI, roomID); errRes != nil {
		return *errRes
	}

	var joined api.QueryServerJoinedToRoomResponse
	if err := rsAPI.QueryServerJoinedToRoom(ctx, &api.QueryServerJoinedToRoomRequest{
		ServerName: request.Origin(), RoomID: roomID,
	}, &joined); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !joined.IsInRoom {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("The server is not in the room")}
	}

	eventID, eventTS, err := rsAPI.QueryEventAtTimestamp(ctx, roomID, ts, forward)
	switch {
	case errors.Is(err, api.ErrNoEventAtTimestamp):
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unable to find event from the given timestamp")}
	case err != nil:
		util.GetLogger(ctx).WithError(err).Error("QueryEventAtTimestamp failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: httputil.TimestampToEventResponse{EventID: eventID, OriginServerTS: eventTS}}
}
