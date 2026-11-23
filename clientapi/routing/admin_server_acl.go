// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/internal/httputil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
)

// AdminServerACL implements GET /_dendrite/admin/serverACL/{roomID}: the server ACL that the room has now (the
// content of its m.room.server_acl state event, null if there is none) and, with ?server=name, whether the roomserver
// would refuse that server because of it.
func AdminServerACL(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		return util.ErrorResponse(err)
	}
	roomID := vars["roomID"]
	if _, err = spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid room ID")}
	}
	ctx := req.Context()
	tuple := gomatrixserverlib.StateKeyTuple{EventType: "m.room.server_acl", StateKey: ""}
	var state roomserverAPI.QueryCurrentStateResponse
	if err = rsAPI.QueryCurrentState(ctx, &roomserverAPI.QueryCurrentStateRequest{
		RoomID: roomID, StateTuples: []gomatrixserverlib.StateKeyTuple{tuple},
	}, &state); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryCurrentState failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	body := map[string]any{"room_id": roomID, "acl": nil}
	if ev := state.StateEvents[tuple]; ev != nil {
		body["acl"] = json.RawMessage(ev.Content())
		body["event_id"] = ev.EventID()
	}
	if server := req.URL.Query().Get("server"); server != "" {
		var res roomserverAPI.QueryServerBannedFromRoomResponse
		if err = rsAPI.QueryServerBannedFromRoom(ctx, &roomserverAPI.QueryServerBannedFromRoomRequest{
			ServerName: spec.ServerName(server), RoomID: roomID,
		}, &res); err != nil {
			util.GetLogger(ctx).WithError(err).Error("QueryServerBannedFromRoom failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		body["server"] = map[string]any{"name": server, "banned": res.Banned}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}
