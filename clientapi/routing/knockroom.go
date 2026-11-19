// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"io"
	"net/http"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	appserviceAPI "github.com/element-hq/dendrite/appservice/api"
	"github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/internal/eventutil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/userapi/api"
)

// KnockRoomByIDOrAlias handles POST /knock/{roomIdOrAlias}: the user asks to be invited to a room.
func KnockRoomByIDOrAlias(
	req *http.Request,
	device *api.Device,
	rsAPI roomserverAPI.ClientRoomserverAPI,
	profileAPI api.ClientUserAPI,
	roomIDOrAlias string,
) util.JSONResponse {
	var body struct {
		Reason string `json:"reason"`
	}
	// The body is optional.
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be read")}
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if resErr := httputil.UnmarshalJSON(raw, &body); resErr != nil {
			return *resErr
		}
	}

	knockReq := roomserverAPI.PerformKnockRequest{
		RoomIDOrAlias: roomIDOrAlias,
		UserID:        device.UserID,
		Content:       map[string]interface{}{},
	}
	if body.Reason != "" {
		knockReq.Content["reason"] = body.Reason
	}
	for _, server := range req.URL.Query()["via"] {
		knockReq.ServerNames = append(knockReq.ServerNames, spec.ServerName(server))
	}

	// Like for a join, the profile of the user goes into the event.
	profile, err := profileAPI.QueryProfile(req.Context(), device.UserID)
	switch err {
	case nil:
		knockReq.Content["displayname"] = profile.DisplayName
		knockReq.Content["avatar_url"] = profile.AvatarURL
	case appserviceAPI.ErrProfileNotExists:
		util.GetLogger(req.Context()).Error("Unable to query user profile, no profile found.")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.Unknown("Unable to query user profile, no profile found."),
		}
	default:
	}

	roomID, err := rsAPI.PerformKnock(req.Context(), &knockReq)
	switch e := err.(type) {
	case nil:
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: struct {
				RoomID string `json:"room_id"`
			}{roomID},
		}
	case roomserverAPI.ErrInvalidID:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.Unknown(e.Error())}
	case roomserverAPI.ErrNotAllowed:
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(e.Error())}
	case eventutil.ErrRoomNoExists:
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound(e.Error())}
	default:
		util.GetLogger(req.Context()).WithError(err).Error("PerformKnock failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
}
