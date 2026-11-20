// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/eventutil"
	"github.com/element-hq/dendrite/internal/httputil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/synctypes"
)

// The rooms part of the Synapse admin API:
// https://element-hq.github.io/synapse/latest/admin_api/rooms.html

type synapseRoom struct {
	RoomID             string  `json:"room_id"`
	Name               *string `json:"name"`
	CanonicalAlias     *string `json:"canonical_alias"`
	JoinedMembers      int     `json:"joined_members"`
	JoinedLocalMembers int     `json:"joined_local_members"`
	Version            string  `json:"version"`
	Creator            *string `json:"creator"`
	Encryption         *string `json:"encryption"`
	Federatable        bool    `json:"federatable"`
	Public             bool    `json:"public"`
	JoinRules          *string `json:"join_rules"`
	GuestAccess        *string `json:"guest_access"`
	HistoryVisibility  *string `json:"history_visibility"`
	// StateEvents is only known when one room is looked at.
	StateEvents *int `json:"state_events,omitempty"`
}

var (
	nameTuple       = gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomName}
	aliasTuple      = gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomCanonicalAlias}
	joinRuleTuple   = gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomJoinRules}
	guestTuple      = gomatrixserverlib.StateKeyTuple{EventType: "m.room.guest_access"}
	historyTuple    = gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomHistoryVisibility}
	encryptionTuple = gomatrixserverlib.StateKeyTuple{EventType: "m.room.encryption"}
	createTuple     = gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomCreate}
)

// adminRooms reads what the room list shows of rooms, from the state of the rooms.
func adminRooms(req *http.Request, cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI, roomIDs []string) ([]synapseRoom, error) {
	ctx := req.Context()
	var published roomserverAPI.QueryPublishedRoomsResponse
	if err := rsAPI.QueryPublishedRooms(ctx, &roomserverAPI.QueryPublishedRoomsRequest{}, &published); err != nil {
		return nil, err
	}
	isPublished := make(map[string]bool, len(published.RoomIDs))
	for _, id := range published.RoomIDs {
		isPublished[id] = true
	}

	var content roomserverAPI.QueryBulkStateContentResponse
	if err := rsAPI.QueryBulkStateContent(ctx, &roomserverAPI.QueryBulkStateContentRequest{
		RoomIDs:        roomIDs,
		AllowWildcards: true,
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			nameTuple, aliasTuple, joinRuleTuple, guestTuple, historyTuple, encryptionTuple,
			{EventType: spec.MRoomMember, StateKey: "*"},
		},
	}, &content); err != nil {
		return nil, err
	}

	rooms := make([]synapseRoom, 0, len(roomIDs))
	for _, id := range roomIDs {
		r := synapseRoom{RoomID: id, Public: isPublished[id], Federatable: true}
		for tuple, value := range content.Rooms[id] {
			if tuple.EventType == spec.MRoomMember {
				if value == spec.Join {
					r.JoinedMembers++
					if _, domain, err := gomatrixserverlib.SplitID('@', tuple.StateKey); err == nil && cfg.Matrix.IsLocalServerName(domain) {
						r.JoinedLocalMembers++
					}
				}
				continue
			}
			switch tuple {
			case nameTuple:
				r.Name = optionalString(value)
			case aliasTuple:
				r.CanonicalAlias = optionalString(value)
			case joinRuleTuple:
				r.JoinRules = optionalString(value)
			case guestTuple:
				r.GuestAccess = optionalString(value)
			case historyTuple:
				r.HistoryVisibility = optionalString(value)
			case encryptionTuple:
				r.Encryption = optionalString(value)
			}
		}
		rooms = append(rooms, r)
	}
	return rooms, nil
}

// addCreateInfo fills in what only the create event has: the version, the creator and whether the room federates.
func addCreateInfo(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI, rooms []synapseRoom) error {
	for i := range rooms {
		var res roomserverAPI.QueryCurrentStateResponse
		if err := rsAPI.QueryCurrentState(req.Context(), &roomserverAPI.QueryCurrentStateRequest{
			RoomID: rooms[i].RoomID, StateTuples: []gomatrixserverlib.StateKeyTuple{createTuple},
		}, &res); err != nil {
			return err
		}
		create := res.StateEvents[createTuple]
		if create == nil {
			continue
		}
		rooms[i].Version = string(create.Version())
		if creator, err := rsAPI.QueryUserIDForSender(req.Context(), create.RoomID(), create.SenderID()); err == nil && creator != nil {
			rooms[i].Creator = optionalString(creator.String())
		}
		if federate := gjson.GetBytes(create.Content(), "m\\.federate"); federate.Exists() {
			rooms[i].Federatable = federate.Bool()
		}
	}
	return nil
}

var roomOrderFields = map[string]func(a, b *synapseRoom) int{
	"":     func(a, b *synapseRoom) int { return strings.Compare(lowerOrEmpty(a.Name), lowerOrEmpty(b.Name)) },
	"name": func(a, b *synapseRoom) int { return strings.Compare(lowerOrEmpty(a.Name), lowerOrEmpty(b.Name)) },
	"canonical_alias": func(a, b *synapseRoom) int {
		return strings.Compare(lowerOrEmpty(a.CanonicalAlias), lowerOrEmpty(b.CanonicalAlias))
	},
	"joined_members":       func(a, b *synapseRoom) int { return a.JoinedMembers - b.JoinedMembers },
	"joined_local_members": func(a, b *synapseRoom) int { return a.JoinedLocalMembers - b.JoinedLocalMembers },
	"version":              func(a, b *synapseRoom) int { return strings.Compare(a.Version, b.Version) },
	"creator":              func(a, b *synapseRoom) int { return strings.Compare(lowerOrEmpty(a.Creator), lowerOrEmpty(b.Creator)) },
	"encryption": func(a, b *synapseRoom) int {
		return strings.Compare(lowerOrEmpty(a.Encryption), lowerOrEmpty(b.Encryption))
	},
	"federatable": func(a, b *synapseRoom) int { return boolInt(a.Federatable) - boolInt(b.Federatable) },
	"public":      func(a, b *synapseRoom) int { return boolInt(a.Public) - boolInt(b.Public) },
	"join_rules": func(a, b *synapseRoom) int {
		return strings.Compare(lowerOrEmpty(a.JoinRules), lowerOrEmpty(b.JoinRules))
	},
	"guest_access": func(a, b *synapseRoom) int {
		return strings.Compare(lowerOrEmpty(a.GuestAccess), lowerOrEmpty(b.GuestAccess))
	},
	"history_visibility": func(a, b *synapseRoom) int {
		return strings.Compare(lowerOrEmpty(a.HistoryVisibility), lowerOrEmpty(b.HistoryVisibility))
	},
}

func lowerOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToLower(*s)
}

// SynapseAdminListRooms implements GET /_synapse/admin/v1/rooms. Every room is looked at to sort and search, so on a
// server with many rooms this is slow: it is for administrators, not for clients.
// nolint:gocyclo
func SynapseAdminListRooms(req *http.Request, cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	q := req.URL.Query()
	order := q.Get("order_by")
	if order == "state_events" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("order_by state_events is not supported")}
	}
	compare, ok := roomOrderFields[order]
	if !ok {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("unknown order_by")}
	}
	backwards := false
	switch q.Get("dir") {
	case "", "f":
	case "b":
		backwards = true
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("dir must be f or b")}
	}
	from, limit := 0, 100
	for name, dst := range map[string]*int{"from": &from, "limit": &limit} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(name + " must be a non-negative integer")}
			}
			*dst = n
		}
	}

	ids, err := rsAPI.AdminQueryAllRooms(req.Context())
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("AdminQueryAllRooms failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	rooms, err := adminRooms(req, cfg, rsAPI, ids)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("adminRooms failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if term := strings.ToLower(q.Get("search_term")); term != "" {
		kept := rooms[:0]
		for _, r := range rooms {
			if strings.Contains(strings.ToLower(r.RoomID), term) || strings.Contains(lowerOrEmpty(r.Name), term) || strings.Contains(lowerOrEmpty(r.CanonicalAlias), term) {
				kept = append(kept, r)
			}
		}
		rooms = kept
	}

	needsCreate := order == "version" || order == "creator" || order == "federatable"
	if needsCreate {
		if err = addCreateInfo(req, rsAPI, rooms); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("addCreateInfo failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}
	sort.SliceStable(rooms, func(i, j int) bool {
		c := compare(&rooms[i], &rooms[j])
		if c == 0 {
			return rooms[i].RoomID < rooms[j].RoomID
		}
		if backwards {
			return c > 0
		}
		return c < 0
	})

	total := len(rooms)
	from = min(from, total)
	page := rooms[from:min(from+limit, total)]
	if !needsCreate {
		if err = addCreateInfo(req, rsAPI, page); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("addCreateInfo failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}
	body := map[string]any{"rooms": page, "offset": from, "total_rooms": total}
	if next := from + len(page); next < total {
		body["next_batch"] = next
	}
	if from > 0 {
		body["prev_batch"] = max(from-limit, 0)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}

func adminRoomID(req *http.Request) (string, *util.JSONResponse) {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		r := util.ErrorResponse(err)
		return "", &r
	}
	id := vars["roomId"]
	if _, err = spec.NewRoomID(id); err != nil {
		return "", &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid room ID")}
	}
	return id, nil
}

// SynapseAdminGetRoom implements GET /_synapse/admin/v1/rooms/{roomId}.
func SynapseAdminGetRoom(req *http.Request, cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	id, errResp := adminRoomID(req)
	if errResp != nil {
		return *errResp
	}
	var state roomserverAPI.QueryLatestEventsAndStateResponse
	if err := rsAPI.QueryLatestEventsAndState(req.Context(), &roomserverAPI.QueryLatestEventsAndStateRequest{RoomID: id}, &state); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryLatestEventsAndState failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !state.RoomExists {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room not found")}
	}
	rooms, err := adminRooms(req, cfg, rsAPI, []string{id})
	if err != nil || len(rooms) != 1 {
		util.GetLogger(req.Context()).WithError(err).Error("adminRooms failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if err = addCreateInfo(req, rsAPI, rooms); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("addCreateInfo failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	count := len(state.StateEvents)
	rooms[0].StateEvents = &count
	return util.JSONResponse{Code: http.StatusOK, JSON: rooms[0]}
}

// SynapseAdminRoomMembers implements GET /_synapse/admin/v1/rooms/{roomId}/members.
func SynapseAdminRoomMembers(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	id, errResp := adminRoomID(req)
	if errResp != nil {
		return *errResp
	}
	var res roomserverAPI.QueryMembershipsForRoomResponse
	if err := rsAPI.QueryMembershipsForRoom(req.Context(), &roomserverAPI.QueryMembershipsForRoomRequest{RoomID: id, JoinedOnly: true}, &res); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryMembershipsForRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	members := make([]string, 0, len(res.JoinEvents))
	for _, ev := range res.JoinEvents {
		members = append(members, ev.Sender)
	}
	sort.Strings(members)
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"members": members, "total": len(members)}}
}

// SynapseAdminRoomState implements GET /_synapse/admin/v1/rooms/{roomId}/state.
func SynapseAdminRoomState(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	id, errResp := adminRoomID(req)
	if errResp != nil {
		return *errResp
	}
	var state roomserverAPI.QueryLatestEventsAndStateResponse
	if err := rsAPI.QueryLatestEventsAndState(req.Context(), &roomserverAPI.QueryLatestEventsAndStateRequest{RoomID: id}, &state); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryLatestEventsAndState failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !state.RoomExists {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room not found")}
	}
	events := make([]synctypes.ClientEvent, 0, len(state.StateEvents))
	for _, ev := range state.StateEvents {
		client, err := synctypes.ToClientEvent(ev.PDU, synctypes.FormatAll, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return rsAPI.QueryUserIDForSender(req.Context(), roomID, senderID)
		})
		if err != nil {
			continue
		}
		events = append(events, *client)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"state": events}}
}

// roomDeletion is what is kept of a deletion for its status.
type roomDeletion struct {
	Status       string   `json:"status"`
	Error        string   `json:"error,omitempty"`
	KickedUsers  []string `json:"kicked_users,omitempty"`
	FailedToKick []string `json:"failed_to_kick_users,omitempty"`
	LocalAliases []string `json:"local_aliases,omitempty"`
	NewRoomID    *string  `json:"new_room_id,omitempty"`
	RoomID       string   `json:"-"`
	finishedAt   time.Time
}

var (
	roomDeletionsMu sync.Mutex
	roomDeletions   = map[string]*roomDeletion{}
)

func rememberRoomDeletion(id string, d *roomDeletion) {
	roomDeletionsMu.Lock()
	defer roomDeletionsMu.Unlock()
	d.finishedAt = time.Now()
	for k, v := range roomDeletions {
		if time.Since(v.finishedAt) > 24*time.Hour {
			delete(roomDeletions, k)
		}
	}
	roomDeletions[id] = d
}

// SynapseAdminDeleteRoom implements DELETE /_synapse/admin/v1/rooms/{roomId} and /v2/rooms/{roomId}: the local users
// leave the room and, unless purge is false, everything about the room is removed from this server, and with block
// the room is blocked as well, also if it is unknown here. Sending a message to its users and moving them to a new room
// are not supported.
func SynapseAdminDeleteRoom(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI, v2 bool) util.JSONResponse {
	id, errResp := adminRoomID(req)
	if errResp != nil {
		return *errResp
	}
	var body struct {
		Block       bool   `json:"block"`
		Purge       *bool  `json:"purge"`
		Message     string `json:"message"`
		NewRoomUser string `json:"new_room_user_id"`
		RoomName    string `json:"room_name"`
		ForcePurge  bool   `json:"force_purge"`
	}
	if req.Body != nil {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
		}
	}
	if body.NewRoomUser != "" || body.Message != "" || body.RoomName != "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Sending a message or moving the users to a new room is not supported")}
	}
	purge := body.Purge == nil || *body.Purge

	result := &roomDeletion{Status: "complete", RoomID: id, KickedUsers: []string{}, FailedToKick: []string{}, LocalAliases: []string{}}
	// The block goes in first so that nobody joins the room while it is being emptied.
	if body.Block {
		if err := rsAPI.PerformAdminBlockRoom(req.Context(), id, true); err != nil {
			util.GetLogger(req.Context()).WithError(err).WithField("room_id", id).Error("Failed to block room")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}
	affected, err := rsAPI.PerformAdminEvacuateRoom(req.Context(), id)
	switch err.(type) {
	case nil:
		result.KickedUsers = append(result.KickedUsers, affected...)
	case eventutil.ErrRoomNoExists:
		// A room that is not known here can still be blocked.
		if !body.Block {
			return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room not found")}
		}
		purge = false
	default:
		util.GetLogger(req.Context()).WithError(err).WithField("room_id", id).Error("Failed to evacuate room")
		result.Status, result.Error = "failed", err.Error()
	}
	if result.Status == "complete" && purge {
		if err = rsAPI.PerformAdminPurgeRoom(req.Context(), id); err != nil {
			util.GetLogger(req.Context()).WithError(err).WithField("room_id", id).Error("Failed to purge room")
			result.Status, result.Error = "failed", err.Error()
		}
	}

	if !v2 {
		if result.Status != "complete" {
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.Unknown(result.Error)}
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{
			"kicked_users": result.KickedUsers, "failed_to_kick_users": result.FailedToKick,
			"local_aliases": result.LocalAliases, "new_room_id": nil,
		}}
	}
	deleteID := util.RandomString(16)
	rememberRoomDeletion(deleteID, result)
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"delete_id": deleteID}}
}

// SynapseAdminRoomDeleteStatus implements GET /_synapse/admin/v2/rooms/{roomId}/delete_status[/{deleteId}].
func SynapseAdminRoomDeleteStatus(req *http.Request) util.JSONResponse {
	deleteID := mux.Vars(req)["deleteId"]
	id := ""
	if _, ok := mux.Vars(req)["roomId"]; ok || deleteID == "" {
		var errResp *util.JSONResponse
		if id, errResp = adminRoomID(req); errResp != nil {
			return *errResp
		}
	}
	roomDeletionsMu.Lock()
	defer roomDeletionsMu.Unlock()
	if deleteID != "" {
		if d, ok := roomDeletions[deleteID]; ok && (id == "" || d.RoomID == id) {
			return util.JSONResponse{Code: http.StatusOK, JSON: d}
		}
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("No delete status found")}
	}
	results := []*roomDeletion{}
	for _, d := range roomDeletions {
		if d.RoomID == id {
			results = append(results, d)
		}
	}
	if len(results) == 0 {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("No delete status found")}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"results": results}}
}

// SynapseAdminRoomBlock implements GET and PUT /_synapse/admin/v1/rooms/{roomId}/block. The room need not exist on this
// server. Unlike Synapse, GET does not say which admin blocked the room.
func SynapseAdminRoomBlock(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	id, errResp := adminRoomID(req)
	if errResp != nil {
		return *errResp
	}
	if req.Method == http.MethodPut {
		var body struct {
			Block *bool `json:"block"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
		}
		if body.Block == nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Param 'block' must be a boolean")}
		}
		if err := rsAPI.PerformAdminBlockRoom(req.Context(), id, *body.Block); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("PerformAdminBlockRoom failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]bool{"block": *body.Block}}
	}
	blocked, err := rsAPI.QueryRoomBlocked(req.Context(), id)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryRoomBlocked failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]bool{"block": blocked}}
}
