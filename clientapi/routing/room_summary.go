package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/element-hq/dendrite/clientapi/auth"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"
)

// maxSummaryVias bounds how many servers one request asks over federation.
const maxSummaryVias = 3

// roomSummary is the response of the room summary API (MSC3266).
type roomSummary struct {
	RoomID           string   `json:"room_id"`
	AvatarURL        string   `json:"avatar_url,omitempty"`
	CanonicalAlias   string   `json:"canonical_alias,omitempty"`
	GuestCanJoin     bool     `json:"guest_can_join"`
	JoinRule         string   `json:"join_rule,omitempty"`
	Name             string   `json:"name,omitempty"`
	NumJoinedMembers int      `json:"num_joined_members"`
	RoomType         string   `json:"room_type,omitempty"`
	Topic            string   `json:"topic,omitempty"`
	WorldReadable    bool     `json:"world_readable"`
	AllowedRoomIDs   []string `json:"allowed_room_ids,omitempty"`
	Encryption       string   `json:"encryption,omitempty"`
	Membership       string   `json:"membership,omitempty"`
	RoomVersion      string   `json:"room_version,omitempty"`
}

// toJSON renders the summary. The unstable endpoints name the fields that the MSC added with the im.nheko.summary prefix.
func (s roomSummary) toJSON(unstable bool) map[string]any {
	raw, _ := json.Marshal(s)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if unstable {
		for _, field := range []string{"encryption", "room_version"} {
			if v, ok := out[field]; ok {
				delete(out, field)
				out["im.nheko.summary."+field] = v
			}
		}
	}
	return out
}

// optionalDevice authenticates the request if it carries an access token. A token that is not valid is an
// error and not an anonymous request. Without a token it returns a nil device.
func optionalDevice(req *http.Request, userAPI userapi.QueryAcccessTokenAPI) (*userapi.Device, *util.JSONResponse) {
	if req.Header.Get("Authorization") == "" && req.URL.Query().Get("access_token") == "" {
		return nil, nil
	}
	device, errRes := auth.VerifyUserFromRequest(req, userAPI)
	if errRes != nil {
		return nil, errRes
	}
	return device, nil
}

// canPreview decides whether the room may be summarised for the requester (MSC3266). It never needs more than
// what the room state says, so that the answer for a room that is not accessible is the same as for one that does not exist.
func canPreview(authenticated bool, membership, joinRule string, worldReadable bool, inAllowedRoom bool) bool {
	switch membership {
	case spec.Join, spec.Invite, spec.Knock:
		return true
	case spec.Ban:
		return false
	}
	if worldReadable {
		return true
	}
	switch joinRule {
	case spec.Public, spec.Knock:
		return true
	case spec.Restricted, spec.KnockRestricted:
		// Only someone who could join, that is, who is in one of the allowed rooms, may look.
		return authenticated && inAllowedRoom
	}
	return false
}

// allowedRoomIDs returns the rooms that membership of lets someone join a restricted room.
func allowedRoomIDs(content []byte) []string {
	var jr gomatrixserverlib.JoinRuleContent
	if err := json.Unmarshal(content, &jr); err != nil {
		return nil
	}
	var ids []string
	for _, allow := range jr.Allow {
		if allow.Type == spec.MRoomMembership && allow.RoomID != "" {
			ids = append(ids, allow.RoomID)
		}
	}
	return ids
}

func summaryNotFound() util.JSONResponse {
	return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room not found or not accessible")}
}

// GetRoomSummary handles GET /room_summary/{roomIdOrAlias}. A nil device means the request is not authenticated.
func GetRoomSummary(
	req *http.Request, device *userapi.Device, roomIDOrAlias string, unstable bool,
	cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI, federation fclient.FederationClient,
) util.JSONResponse {
	ctx := req.Context()
	vias := req.URL.Query()["via"]

	var roomID string
	switch {
	case strings.HasPrefix(roomIDOrAlias, "!"):
		roomID = roomIDOrAlias
		if parsed, err := spec.NewRoomID(roomIDOrAlias); err != nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid room ID")}
		} else if domain := parsed.Domain(); domain != "" {
			vias = append(vias, string(domain))
		}
	case strings.HasPrefix(roomIDOrAlias, "#"):
		_, domain, err := gomatrixserverlib.SplitID('#', roomIDOrAlias)
		if err != nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid room alias")}
		}
		var res roomserverAPI.GetRoomIDForAliasResponse
		if err = rsAPI.GetRoomIDForAlias(ctx, &roomserverAPI.GetRoomIDForAliasRequest{Alias: roomIDOrAlias, IncludeAppservices: true}, &res); err != nil {
			return internalError(req, "GetRoomIDForAlias", err)
		}
		roomID = res.RoomID
		// Looking up an alias on another server is a request to it, which only users may cause.
		if roomID == "" && device != nil && !cfg.Matrix.IsLocalServerName(domain) {
			fedRes, fedErr := federation.LookupRoomAlias(ctx, cfg.Matrix.ServerName, domain, roomIDOrAlias)
			if fedErr == nil {
				roomID = fedRes.RoomID
				vias = append(vias, string(domain))
				for _, s := range fedRes.Servers {
					vias = append(vias, string(s))
				}
			} else {
				util.GetLogger(ctx).WithError(fedErr).Debug("room summary: alias lookup failed")
			}
		}
		if roomID == "" {
			return summaryNotFound()
		}
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Expected a room ID or a room alias")}
	}

	var joined roomserverAPI.QueryServerJoinedToRoomResponse
	if err := rsAPI.QueryServerJoinedToRoom(ctx, &roomserverAPI.QueryServerJoinedToRoomRequest{
		RoomID: roomID, ServerName: cfg.Matrix.ServerName,
	}, &joined); err != nil {
		return internalError(req, "QueryServerJoinedToRoom", err)
	}

	var summary *roomSummary
	if joined.RoomExists && joined.IsInRoom {
		var errRes *util.JSONResponse
		if summary, errRes = localRoomSummary(req, device, roomID, cfg, rsAPI); errRes != nil {
			return *errRes
		}
	} else if device != nil {
		// Ask the servers in the room, the way the room hierarchy does.
		summary = remoteRoomSummary(req, roomID, vias, cfg, federation)
	}
	if summary == nil {
		return summaryNotFound()
	}

	if device != nil && summary.Membership == "" {
		summary.Membership = spec.Leave
		if userID, err := spec.NewUserID(device.UserID, true); err == nil {
			var m roomserverAPI.QueryMembershipForUserResponse
			if err = rsAPI.QueryMembershipForUser(ctx, &roomserverAPI.QueryMembershipForUserRequest{RoomID: roomID, UserID: *userID}, &m); err == nil && m.Membership != "" {
				summary.Membership = m.Membership
			}
		}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: summary.toJSON(unstable), Headers: map[string]string{"Cache-Control": "no-store"}}
}

// localRoomSummary builds the summary from the state this server holds. It returns nil, nil if the requester may not see the room.
func localRoomSummary(
	req *http.Request, device *userapi.Device, roomID string, cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI,
) (*roomSummary, *util.JSONResponse) {
	ctx := req.Context()
	createTuple := gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomCreate, StateKey: ""}
	joinRuleTuple := gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomJoinRules, StateKey: ""}
	encryptionTuple := gomatrixserverlib.StateKeyTuple{EventType: "m.room.encryption", StateKey: ""}
	tuples := []gomatrixserverlib.StateKeyTuple{createTuple, joinRuleTuple, encryptionTuple}
	var memberTuple gomatrixserverlib.StateKeyTuple
	if device != nil {
		memberTuple = gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: device.UserID}
		tuples = append(tuples, memberTuple)
	}
	var state roomserverAPI.QueryCurrentStateResponse
	if err := rsAPI.QueryCurrentState(ctx, &roomserverAPI.QueryCurrentStateRequest{RoomID: roomID, StateTuples: tuples}, &state); err != nil {
		res := internalError(req, "QueryCurrentState", err)
		return nil, &res
	}
	create := state.StateEvents[createTuple]
	if create == nil {
		res := summaryNotFound()
		return nil, &res
	}

	pub, err := roomserverAPI.PopulatePublicRooms(ctx, []string{roomID}, rsAPI)
	if err != nil {
		res := internalError(req, "PopulatePublicRooms", err)
		return nil, &res
	}
	if len(pub) == 0 {
		res := summaryNotFound()
		return nil, &res
	}
	s := roomSummary{
		RoomID:           roomID,
		AvatarURL:        pub[0].AvatarURL,
		CanonicalAlias:   pub[0].CanonicalAlias,
		GuestCanJoin:     pub[0].GuestCanJoin,
		JoinRule:         pub[0].JoinRule,
		Name:             pub[0].Name,
		NumJoinedMembers: pub[0].JoinedMembersCount,
		Topic:            pub[0].Topic,
		WorldReadable:    pub[0].WorldReadable,
	}
	var createContent gomatrixserverlib.CreateContent
	if err = json.Unmarshal(create.Content(), &createContent); err == nil {
		s.RoomType = createContent.RoomType
	}
	s.RoomVersion = roomVersionOf(create.Content())
	if enc := state.StateEvents[encryptionTuple]; enc != nil {
		s.Encryption = jsonString(enc.Content(), "algorithm")
	}
	if joinRuleEv := state.StateEvents[joinRuleTuple]; joinRuleEv != nil && (s.JoinRule == spec.Restricted || s.JoinRule == spec.KnockRestricted) {
		s.AllowedRoomIDs = allowedRoomIDs(joinRuleEv.Content())
	}

	membership := ""
	if device != nil {
		if memberEv := state.StateEvents[memberTuple]; memberEv != nil {
			membership, _ = memberEv.Membership()
			s.Membership = membership
		}
	}
	inAllowedRoom := false
	if device != nil && membership != spec.Join {
		inAllowedRoom = joinedToAny(ctx, rsAPI, device.UserID, s.AllowedRoomIDs)
	}
	if !canPreview(device != nil, membership, s.JoinRule, s.WorldReadable, inAllowedRoom) {
		return nil, nil
	}
	return &s, nil
}

// joinedToAny reports whether the user is joined to one of the rooms.
func joinedToAny(ctx context.Context, rsAPI roomserverAPI.ClientRoomserverAPI, userID string, roomIDs []string) bool {
	parsed, err := spec.NewUserID(userID, true)
	if err != nil {
		return false
	}
	for _, roomID := range roomIDs {
		var res roomserverAPI.QueryMembershipForUserResponse
		if err = rsAPI.QueryMembershipForUser(ctx, &roomserverAPI.QueryMembershipForUserRequest{RoomID: roomID, UserID: *parsed}, &res); err == nil && res.Membership == spec.Join {
			return true
		}
	}
	return false
}

// roomVersionOf returns the room version of a create event, which is "1" when it names none.
func roomVersionOf(createContent []byte) string {
	if v := jsonString(createContent, "room_version"); v != "" {
		return v
	}
	return "1"
}

func jsonString(content []byte, key string) string {
	return gjson.GetBytes(content, key).String()
}

// remoteRoomSummary asks the given servers for the room and takes the first answer that is about that room.
func remoteRoomSummary(
	req *http.Request, roomID string, vias []string, cfg *config.ClientAPI, federation fclient.FederationClient,
) *roomSummary {
	tried := map[string]bool{}
	for _, via := range vias {
		if len(tried) >= maxSummaryVias {
			break
		}
		if via == "" || tried[via] || cfg.Matrix.IsLocalServerName(spec.ServerName(via)) {
			continue
		}
		tried[via] = true
		res, err := federation.RoomHierarchy(req.Context(), cfg.Matrix.ServerName, spec.ServerName(via), roomID, false)
		if err != nil {
			util.GetLogger(req.Context()).WithError(err).WithField("via", via).Debug("room summary: hierarchy request failed")
			continue
		}
		if res.Room.RoomID != roomID {
			continue
		}
		return &roomSummary{
			RoomID:           roomID,
			AvatarURL:        res.Room.AvatarURL,
			CanonicalAlias:   res.Room.CanonicalAlias,
			GuestCanJoin:     res.Room.GuestCanJoin,
			JoinRule:         res.Room.JoinRule,
			Name:             res.Room.Name,
			NumJoinedMembers: res.Room.JoinedMembersCount,
			RoomType:         res.Room.RoomType,
			Topic:            res.Room.Topic,
			WorldReadable:    res.Room.WorldReadable,
			AllowedRoomIDs:   res.Room.AllowedRoomIDs,
		}
	}
	return nil
}
