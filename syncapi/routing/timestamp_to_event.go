// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/syncapi/internal"
	"github.com/element-hq/dendrite/syncapi/storage"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// maxTimestampHops is how many events that the user may not see are skipped to find the one the user may see.
const maxTimestampHops = 20

// TimestampToEvent handles GET /rooms/{roomId}/timestamp_to_event: the event of the room that is closest to a time,
// after it (dir=f) or before it (dir=b). It looks at what this server has of the room, and only at events that the
// user may see.
func TimestampToEvent(
	req *http.Request, device *userapi.Device,
	syncDB storage.Database, rsAPI api.SyncRoomserverAPI,
	rawRoomID string,
) util.JSONResponse {
	ctx := req.Context()
	roomID, err := spec.NewRoomID(rawRoomID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid room ID")}
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("device.UserID invalid")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	ts, forward, errRes := httputil.ParseTimestampToEventQuery(req)
	if errRes != nil {
		return *errRes
	}

	membership, err := getMembershipForUser(ctx, roomID.String(), device.UserID, rsAPI)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("getMembershipForUser failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !membership.RoomExists {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("room does not exist")}
	}
	if membership.IsRoomForgotten {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("user already forgot about this room")}
	}

	snapshot, err := syncDB.NewDatabaseSnapshot(ctx)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("failed to get a snapshot for timestamp_to_event")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	var succeeded bool
	defer sqlutil.EndTransactionWithCheck(snapshot, &succeeded, &err)

	if !membership.IsInRoom && !membership.HasBeenInRoom {
		visibility, vErr := snapshot.GetStateEvent(ctx, roomID.String(), spec.MRoomHistoryVisibility, "")
		if vErr != nil && !errors.Is(vErr, sql.ErrNoRows) {
			err = vErr
			return util.ErrorResponse(err)
		}
		if visibility == nil || gjson.GetBytes(visibility.Content(), "history_visibility").String() != "world_readable" {
			return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("you cannot see the history of this room")}
		}
	}

	notFound := util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unable to find event from the given timestamp")}
	for hop := 0; hop < maxTimestampHops; hop++ {
		eventID, eventTS, lerr := snapshot.EventAtTimestamp(ctx, roomID.String(), ts, forward)
		if errors.Is(lerr, sql.ErrNoRows) {
			return notFound
		}
		if lerr != nil {
			err = lerr
			return util.ErrorResponse(err)
		}
		events, lerr := snapshot.Events(ctx, []string{eventID})
		if lerr != nil {
			err = lerr
			return util.ErrorResponse(err)
		}
		visible, lerr := internal.ApplyHistoryVisibilityFilter(ctx, snapshot, rsAPI, events, nil, *userID, "timestamp_to_event")
		if lerr != nil {
			err = lerr
			return util.ErrorResponse(err)
		}
		if len(visible) > 0 {
			succeeded = true
			return util.JSONResponse{Code: http.StatusOK, JSON: httputil.TimestampToEventResponse{EventID: eventID, OriginServerTS: eventTS}}
		}
		// The event is one the user may not see: look past it.
		if forward {
			ts = eventTS + 1
		} else {
			ts = eventTS - 1
		}
		if ts < 0 {
			return notFound
		}
	}
	return notFound
}
