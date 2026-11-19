// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/internal/eventutil"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
)

// knockStrippedStateTypes are the state events a server gets to see about a room it has knocked on.
var knockStrippedStateTypes = []string{
	spec.MRoomCreate,
	spec.MRoomJoinRules,
	spec.MRoomCanonicalAlias,
	spec.MRoomName,
	spec.MRoomAvatar,
	spec.MRoomTopic,
	spec.MRoomEncryption,
}

// knockAllowed checks the knock against the state of the room, so that the knocking server is told about a knock the
// room does not allow before anything is stored.
func knockAllowed(
	httpReq *http.Request, rsAPI api.FederationRoomserverAPI, event gomatrixserverlib.PDU,
) error {
	needed := gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.PDU{event})
	res := api.QueryLatestEventsAndStateResponse{}
	err := rsAPI.QueryLatestEventsAndState(httpReq.Context(), &api.QueryLatestEventsAndStateRequest{
		RoomID:       event.RoomID().String(),
		StateToFetch: needed.Tuples(),
	}, &res)
	if err != nil {
		return err
	}
	authEvents, err := gomatrixserverlib.NewAuthEvents(nil)
	if err != nil {
		return err
	}
	for _, stateEvent := range res.StateEvents {
		if err = authEvents.AddEvent(stateEvent.PDU); err != nil {
			return err
		}
	}
	return gomatrixserverlib.Allowed(event, authEvents, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return rsAPI.QueryUserIDForSender(httpReq.Context(), roomID, senderID)
	})
}

// MakeKnock implements the /make_knock API: a template of the knock for the other server to sign.
func MakeKnock(
	httpReq *http.Request,
	request *fclient.FederationRequest,
	cfg *config.FederationAPI,
	rsAPI api.FederationRoomserverAPI,
	roomID spec.RoomID, userID spec.UserID,
	remoteVersions []gomatrixserverlib.RoomVersion,
) util.JSONResponse {
	ctx := httpReq.Context()
	inRoom := api.QueryServerJoinedToRoomResponse{}
	if err := rsAPI.QueryServerJoinedToRoom(ctx, &api.QueryServerJoinedToRoomRequest{
		ServerName: request.Destination(),
		RoomID:     roomID.String(),
	}, &inRoom); err != nil {
		util.GetLogger(ctx).WithError(err).Error("rsAPI.QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !inRoom.RoomExists || !inRoom.IsInRoom {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound(fmt.Sprintf("Local server not currently joined to room: %s", roomID.String()))}
	}
	roomVersion := inRoom.RoomVersion

	if !slices.Contains(remoteVersions, roomVersion) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.IncompatibleRoomVersion(string(roomVersion))}
	}
	if userID.Domain() != request.Origin() {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(fmt.Sprintf(
			"The knock must be sent by the server of the user. Origin %s != %s", request.Origin(), userID.Domain(),
		))}
	}
	if roomVersion == gomatrixserverlib.RoomVersionPseudoIDs {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("Knocking is not supported in this room version")}
	}

	identity, err := cfg.Matrix.SigningIdentityFor(request.Destination())
	if err != nil {
		util.GetLogger(ctx).WithError(err).Errorf("obtaining signing identity for %s failed", request.Destination())
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound(fmt.Sprintf("Server name %q does not exist", request.Destination()))}
	}

	sender := userID.String()
	proto := gomatrixserverlib.ProtoEvent{
		SenderID: sender,
		RoomID:   roomID.String(),
		Type:     spec.MRoomMember,
		StateKey: &sender,
	}
	if err = proto.SetContent(gomatrixserverlib.MemberContent{Membership: spec.Knock}); err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	queryRes := api.QueryLatestEventsAndStateResponse{RoomVersion: roomVersion}
	event, err := eventutil.QueryAndBuildEvent(ctx, &proto, identity, time.Now(), rsAPI, &queryRes)
	switch err.(type) {
	case nil:
	case eventutil.ErrRoomNoExists:
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room does not exist")}
	default:
		util.GetLogger(ctx).WithError(err).Error("eventutil.QueryAndBuildEvent failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}

	if err = knockAllowed(httpReq, rsAPI, event); err != nil {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(err.Error())}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: map[string]any{
			"event":        proto,
			"room_version": roomVersion,
		},
	}
}

// SendKnock implements the /send_knock API: a signed knock to store, answered with the state of the room that the
// knocking user may see.
// nolint:gocyclo
func SendKnock(
	httpReq *http.Request,
	request *fclient.FederationRequest,
	cfg *config.FederationAPI,
	rsAPI api.FederationRoomserverAPI,
	keys gomatrixserverlib.JSONVerifier,
	roomID, eventID string,
) util.JSONResponse {
	ctx := httpReq.Context()
	roomVersion, err := rsAPI.QueryRoomVersionForRoom(ctx, roomID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room does not exist")}
	}
	verImpl, err := gomatrixserverlib.GetRoomVersion(roomVersion)
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.UnsupportedRoomVersion(
			fmt.Sprintf("QueryRoomVersionForRoom returned unknown version: %s", roomVersion),
		)}
	}

	event, err := verImpl.NewEventFromUntrustedJSON(request.Content())
	switch err.(type) {
	case gomatrixserverlib.BadJSONError:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON(err.Error())}
	case nil:
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded into valid JSON. " + err.Error())}
	}

	if event.RoomID().String() != roomID {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The room ID in the request path must match the room ID in the knock event JSON")}
	}
	if event.EventID() != eventID {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The event ID in the request path must match the event ID in the knock event JSON")}
	}
	if event.Type() != spec.MRoomMember || event.StateKey() == nil || !event.StateKeyEquals(string(event.SenderID())) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The event must be a membership event of its sender")}
	}
	if mem, merr := event.Membership(); merr != nil || mem != spec.Knock {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The membership in the event content must be set to knock")}
	}

	sender, err := rsAPI.QueryUserIDForSender(ctx, event.RoomID(), event.SenderID())
	if err != nil || sender == nil {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("The sender of the knock is invalid")}
	}
	if sender.Domain() != request.Origin() {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("The sender does not match the server that originated the request")}
	}

	inRoom := api.QueryServerJoinedToRoomResponse{}
	if err = rsAPI.QueryServerJoinedToRoom(ctx, &api.QueryServerJoinedToRoomRequest{
		ServerName: request.Destination(),
		RoomID:     roomID,
	}, &inRoom); err != nil {
		util.GetLogger(ctx).WithError(err).Error("rsAPI.QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !inRoom.RoomExists || !inRoom.IsInRoom {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Local server not currently joined to room")}
	}

	redacted, err := verImpl.RedactEventJSON(event.JSON())
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The event JSON could not be redacted")}
	}
	verifyResults, err := keys.VerifyJSONs(ctx, []gomatrixserverlib.VerifyJSONRequest{{
		ServerName:           sender.Domain(),
		Message:              redacted,
		AtTS:                 event.OriginServerTS(),
		ValidityCheckingFunc: gomatrixserverlib.StrictValiditySignatureCheck,
	}})
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("keys.VerifyJSONs failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if verifyResults[0].Error != nil {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("The knock must be signed by the server it originated on")}
	}

	if err = knockAllowed(httpReq, rsAPI, event); err != nil {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(err.Error())}
	}

	// The room is told about the knock, and so are the other servers in it.
	var inputRes api.InputRoomEventsResponse
	rsAPI.InputRoomEvents(ctx, &api.InputRoomEventsRequest{
		InputRoomEvents: []api.InputRoomEvent{{
			Kind:         api.KindNew,
			Event:        &types.HeaderedEvent{PDU: event},
			Origin:       request.Origin(),
			SendAsServer: string(cfg.Matrix.ServerName),
		}},
	}, &inputRes)
	if inputRes.ErrMsg != "" {
		util.GetLogger(ctx).WithField("error", inputRes.ErrMsg).WithField("not_allowed", inputRes.NotAllowed).Error("InputRoomEvents failed for a knock")
		if inputRes.NotAllowed {
			return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(inputRes.ErrMsg)}
		}
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}

	stateRes := api.QueryLatestEventsAndStateResponse{}
	tuples := make([]gomatrixserverlib.StateKeyTuple, 0, len(knockStrippedStateTypes))
	for _, eventType := range knockStrippedStateTypes {
		tuples = append(tuples, gomatrixserverlib.StateKeyTuple{EventType: eventType, StateKey: ""})
	}
	if err = rsAPI.QueryLatestEventsAndState(ctx, &api.QueryLatestEventsAndStateRequest{
		RoomID:       roomID,
		StateToFetch: tuples,
	}, &stateRes); err != nil {
		util.GetLogger(ctx).WithError(err).Error("rsAPI.QueryLatestEventsAndState failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	stripped := make([]gomatrixserverlib.InviteStrippedState, 0, len(stateRes.StateEvents))
	for _, stateEvent := range stateRes.StateEvents {
		stripped = append(stripped, gomatrixserverlib.NewInviteStrippedState(stateEvent.PDU))
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: fclient.RespSendKnock{KnockRoomState: stripped}}
}
