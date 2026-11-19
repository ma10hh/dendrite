package routing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
)

func TestCanPreview(t *testing.T) {
	for name, tc := range map[string]struct {
		authenticated  bool
		membership     string
		joinRule       string
		worldReadable  bool
		inAllowedRoom  bool
		wantAccessible bool
	}{
		"public room, anonymous":          {false, "", spec.Public, false, false, true},
		"knockable room, anonymous":       {false, "", spec.Knock, false, false, true},
		"world readable invite only room": {false, "", spec.Invite, true, false, true},
		"invite only room, anonymous":     {false, "", spec.Invite, false, false, false},
		"invite only room, stranger":      {true, spec.Leave, spec.Invite, false, false, false},
		"invite only room, member":        {true, spec.Join, spec.Invite, false, false, true},
		"invite only room, invited":       {true, spec.Invite, spec.Invite, false, false, true},
		"invite only room, knocked":       {true, spec.Knock, spec.Invite, false, false, true},
		"banned from a public room":       {true, spec.Ban, spec.Public, false, false, false},
		"banned from a readable room":     {true, spec.Ban, spec.Invite, true, false, false},
		"restricted, not in allowed room": {true, "", spec.Restricted, false, false, false},
		"restricted, in allowed room":     {true, "", spec.Restricted, false, true, true},
		"restricted, anonymous":           {false, "", spec.Restricted, false, true, false},
		"knock restricted, in allowed":    {true, "", spec.KnockRestricted, false, true, true},
		"no join rule":                    {true, "", "", false, false, false},
	} {
		if got := canPreview(tc.authenticated, tc.membership, tc.joinRule, tc.worldReadable, tc.inAllowedRoom); got != tc.wantAccessible {
			t.Errorf("%s: canPreview = %v, want %v", name, got, tc.wantAccessible)
		}
	}
}

func TestRoomSummaryUnstableFieldNames(t *testing.T) {
	s := roomSummary{RoomID: "!r:test", Encryption: "m.megolm.v1.aes-sha2", RoomVersion: "10", Name: "x"}
	stable := gjson.ParseBytes(mustMarshal(t, s.toJSON(false)))
	if stable.Get("encryption").String() == "" || stable.Get("room_version").String() != "10" {
		t.Errorf("stable fields missing: %s", stable.Raw)
	}
	unstable := gjson.ParseBytes(mustMarshal(t, s.toJSON(true)))
	if unstable.Get("encryption").Exists() || unstable.Get("room_version").Exists() {
		t.Errorf("the unstable response must not use the stable names: %s", unstable.Raw)
	}
	if unstable.Get(`im\.nheko\.summary\.encryption`).String() != "m.megolm.v1.aes-sha2" || unstable.Get(`im\.nheko\.summary\.room_version`).String() != "10" {
		t.Errorf("unstable fields missing: %s", unstable.Raw)
	}
	if unstable.Get("name").String() != "x" || unstable.Get("num_joined_members").Type != gjson.Number {
		t.Errorf("other fields must stay the same: %s", unstable.Raw)
	}
	if empty := gjson.ParseBytes(mustMarshal(t, roomSummary{RoomID: "!r:test"}.toJSON(false))); empty.Get("encryption").Exists() || empty.Get("avatar_url").Exists() {
		t.Errorf("optional fields are omitted when empty: %s", empty.Raw)
	}
}

func TestAllowedRoomIDs(t *testing.T) {
	got := allowedRoomIDs([]byte(`{"join_rule":"restricted","allow":[{"type":"m.room_membership","room_id":"!a:test"},{"type":"other","room_id":"!b:test"},{"type":"m.room_membership"}]}`))
	if len(got) != 1 || got[0] != "!a:test" {
		t.Errorf("allowedRoomIDs = %v, want only !a:test", got)
	}
	if allowedRoomIDs([]byte(`not json`)) != nil {
		t.Error("malformed content has no allowed rooms")
	}
}

// summaryRS is a roomserver that knows a few rooms.
type summaryRS struct {
	roomserverAPI.ClientRoomserverAPI
	aliases     map[string]string
	local       map[string]bool
	state       map[string]map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent
	bulk        map[string]map[gomatrixserverlib.StateKeyTuple]string
	memberships map[string]string // room ID + "|" + user ID
}

func (s *summaryRS) GetRoomIDForAlias(_ context.Context, req *roomserverAPI.GetRoomIDForAliasRequest, res *roomserverAPI.GetRoomIDForAliasResponse) error {
	res.RoomID = s.aliases[req.Alias]
	return nil
}

func (s *summaryRS) QueryServerJoinedToRoom(_ context.Context, req *roomserverAPI.QueryServerJoinedToRoomRequest, res *roomserverAPI.QueryServerJoinedToRoomResponse) error {
	res.RoomExists, res.IsInRoom = s.local[req.RoomID], s.local[req.RoomID]
	return nil
}

func (s *summaryRS) QueryCurrentState(_ context.Context, req *roomserverAPI.QueryCurrentStateRequest, res *roomserverAPI.QueryCurrentStateResponse) error {
	res.StateEvents = map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent{}
	for _, tuple := range req.StateTuples {
		if ev := s.state[req.RoomID][tuple]; ev != nil {
			res.StateEvents[tuple] = ev
		}
	}
	return nil
}

func (s *summaryRS) QueryBulkStateContent(_ context.Context, req *roomserverAPI.QueryBulkStateContentRequest, res *roomserverAPI.QueryBulkStateContentResponse) error {
	res.Rooms = map[string]map[gomatrixserverlib.StateKeyTuple]string{}
	for _, id := range req.RoomIDs {
		if data, ok := s.bulk[id]; ok {
			res.Rooms[id] = data
		}
	}
	return nil
}

func (s *summaryRS) QueryMembershipForUser(_ context.Context, req *roomserverAPI.QueryMembershipForUserRequest, res *roomserverAPI.QueryMembershipForUserResponse) error {
	res.Membership = s.memberships[req.RoomID+"|"+req.UserID.String()]
	return nil
}

type summaryFed struct {
	fclient.FederationClient
	aliases   map[string]fclient.RespDirectory
	hierarchy map[string]fclient.RoomHierarchyResponse // by server name
	asked     []string
	lookups   int
}

func (f *summaryFed) LookupRoomAlias(_ context.Context, _, _ spec.ServerName, alias string) (fclient.RespDirectory, error) {
	f.lookups++
	if r, ok := f.aliases[alias]; ok {
		return r, nil
	}
	return fclient.RespDirectory{}, errors.New("not found")
}

func (f *summaryFed) RoomHierarchy(_ context.Context, _, dst spec.ServerName, _ string, _ bool) (fclient.RoomHierarchyResponse, error) {
	f.asked = append(f.asked, string(dst))
	if r, ok := f.hierarchy[string(dst)]; ok {
		return r, nil
	}
	return fclient.RoomHierarchyResponse{}, errors.New("unknown room")
}

// summaryRoom builds the state of a room of the given preset and the bulk content the room list code reads.
func summaryRoom(t *testing.T, id string, preset test.Preset, events ...func(*test.Room, *test.User)) (map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent, map[gomatrixserverlib.StateKeyTuple]string) {
	t.Helper()
	owner := test.NewUser(t)
	room := test.NewRoom(t, owner, test.RoomPreset(preset))
	for _, add := range events {
		add(room, owner)
	}
	state := map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent{}
	bulk := map[gomatrixserverlib.StateKeyTuple]string{}
	for _, ev := range room.Events() {
		if ev.StateKey() == nil {
			continue
		}
		tuple := gomatrixserverlib.StateKeyTuple{EventType: ev.Type(), StateKey: *ev.StateKey()}
		state[tuple] = ev
		switch ev.Type() {
		case spec.MRoomName:
			bulk[tuple] = gjson.GetBytes(ev.Content(), "name").String()
		case "m.room.topic":
			bulk[tuple] = gjson.GetBytes(ev.Content(), "topic").String()
		case spec.MRoomJoinRules:
			bulk[tuple] = gjson.GetBytes(ev.Content(), "join_rule").String()
		case spec.MRoomHistoryVisibility:
			bulk[tuple] = gjson.GetBytes(ev.Content(), "history_visibility").String()
		case spec.MRoomMember:
			bulk[tuple] = gjson.GetBytes(ev.Content(), "membership").String()
		}
	}
	return state, bulk
}

func summaryRequest(t *testing.T, rs *summaryRS, fed *summaryFed, device *userapi.Device, identifier string, unstable bool, query string) (int, gjson.Result) {
	t.Helper()
	cfg := &config.ClientAPI{Matrix: &config.Global{}}
	cfg.Matrix.ServerName = "test"
	req := httptest.NewRequest(http.MethodGet, "/room_summary/"+identifier+query, nil)
	res := GetRoomSummary(req, device, identifier, unstable, cfg, rs, fed)
	return res.Code, gjson.ParseBytes(mustMarshal(t, res.JSON))
}

func TestGetRoomSummary(t *testing.T) {
	const publicRoom, privateRoom, restrictedRoom, bannedRoom, parentRoom = "!public:test", "!private:test", "!restricted:test", "!banned:test", "!parent:test"
	rs := &summaryRS{
		aliases:     map[string]string{"#public:test": publicRoom},
		local:       map[string]bool{publicRoom: true, privateRoom: true, restrictedRoom: true, bannedRoom: true},
		state:       map[string]map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent{},
		bulk:        map[string]map[gomatrixserverlib.StateKeyTuple]string{},
		memberships: map[string]string{},
	}
	rs.state[publicRoom], rs.bulk[publicRoom] = summaryRoom(t, publicRoom, test.PresetPublicChat, func(r *test.Room, o *test.User) {
		r.CreateAndInsert(t, o, "m.room.encryption", map[string]string{"algorithm": "m.megolm.v1.aes-sha2"}, test.WithStateKey(""))
		r.CreateAndInsert(t, o, "m.room.name", map[string]string{"name": "Public room"}, test.WithStateKey(""))
	})
	aliceUser, bobUser := test.NewUser(t), test.NewUser(t)
	aliceUser.ID, bobUser.ID = "@alice:test", "@bob:test"
	rs.state[privateRoom], rs.bulk[privateRoom] = summaryRoom(t, privateRoom, test.PresetPrivateChat, func(r *test.Room, o *test.User) {
		r.CreateAndInsert(t, o, spec.MRoomMember, map[string]string{"membership": "invite"}, test.WithStateKey(bobUser.ID))
		r.CreateAndInsert(t, bobUser, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bobUser.ID))
	})
	rs.state[restrictedRoom], rs.bulk[restrictedRoom] = summaryRoom(t, restrictedRoom, test.PresetPrivateChat, func(r *test.Room, o *test.User) {
		r.CreateAndInsert(t, o, "m.room.join_rules", map[string]any{
			"join_rule": "restricted", "allow": []map[string]string{{"type": "m.room_membership", "room_id": parentRoom}},
		}, test.WithStateKey(""))
	})
	rs.state[bannedRoom], rs.bulk[bannedRoom] = summaryRoom(t, bannedRoom, test.PresetPublicChat, func(r *test.Room, o *test.User) {
		r.CreateAndInsert(t, o, spec.MRoomMember, map[string]string{"membership": "ban"}, test.WithStateKey(aliceUser.ID))
	})
	alice := &userapi.Device{UserID: "@alice:test", ID: "A"}
	bob := &userapi.Device{UserID: "@bob:test", ID: "B"}
	rs.memberships[parentRoom+"|@alice:test"] = spec.Join

	// Public room, no authentication.
	code, body := summaryRequest(t, rs, &summaryFed{}, nil, publicRoom, false, "")
	if code != http.StatusOK {
		t.Fatalf("public room, anonymous: %d %s", code, body.Raw)
	}
	for field, want := range map[string]string{
		"room_id": publicRoom, "name": "Public room", "join_rule": "public", "encryption": "m.megolm.v1.aes-sha2", "room_version": "9",
	} {
		if got := body.Get(field).String(); got != want {
			t.Errorf("public room: %s = %q, want %q (%s)", field, got, want, body.Raw)
		}
	}
	if body.Get("membership").Exists() {
		t.Error("membership must be absent for an unauthenticated request")
	}

	// The same through the alias, and with the unstable names.
	if code, body = summaryRequest(t, rs, &summaryFed{}, alice, "#public:test", true, ""); code != http.StatusOK || body.Get("room_id").String() != publicRoom {
		t.Errorf("alias: %d %s", code, body.Raw)
	}
	if body.Get(`im\.nheko\.summary\.encryption`).String() == "" || body.Get("encryption").Exists() {
		t.Errorf("the unstable endpoint uses the prefixed field names: %s", body.Raw)
	}
	if got := body.Get("membership").String(); got != "leave" {
		t.Errorf("a user who never joined has membership %q, want leave", got)
	}

	// A member of a private room can preview it, and is told so.
	if code, body = summaryRequest(t, rs, &summaryFed{}, bob, privateRoom, false, ""); code != http.StatusOK || body.Get("membership").String() != "join" {
		t.Errorf("private room, member: %d %s", code, body.Raw)
	}

	// A room that cannot be previewed is indistinguishable from one that does not exist.
	for name, tc := range map[string]struct {
		device *userapi.Device
		room   string
	}{
		"private room, anonymous":       {nil, privateRoom},
		"private room, stranger":        {alice, privateRoom},
		"restricted room, not in space": {bob, restrictedRoom},
		"public room, banned":           {alice, bannedRoom},
		"unknown room":                  {alice, "!nothing:test"},
		"unknown alias":                 {alice, "#nothing:test"},
	} {
		code, body := summaryRequest(t, rs, &summaryFed{}, tc.device, tc.room, false, "")
		if code != http.StatusNotFound || body.Get("errcode").String() != "M_NOT_FOUND" {
			t.Errorf("%s: %d %s, want 404 M_NOT_FOUND", name, code, body.Raw)
		}
	}

	// A restricted room can be previewed from a room that lets people in.
	code, body = summaryRequest(t, rs, &summaryFed{}, alice, restrictedRoom, false, "")
	if code != http.StatusOK || body.Get("allowed_room_ids.0").String() != parentRoom {
		t.Errorf("restricted room, member of the allowed room: %d %s", code, body.Raw)
	}

	for _, bad := range []string{"room", "$event:test", "!:"} {
		if code, _ = summaryRequest(t, rs, &summaryFed{}, alice, bad, false, ""); code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400", bad, code)
		}
	}
}

func TestGetRoomSummaryOverFederation(t *testing.T) {
	remote := fclient.RoomHierarchyResponse{Room: fclient.RoomHierarchyRoom{
		PublicRoom: fclient.PublicRoom{RoomID: "!far:remote.test", Name: "Far away", JoinedMembersCount: 7, JoinRule: "knock", Topic: "hi"},
		RoomType:   "m.space",
	}}
	wrongRoom := fclient.RoomHierarchyResponse{Room: fclient.RoomHierarchyRoom{PublicRoom: fclient.PublicRoom{RoomID: "!other:evil.test", Name: "Not what you asked for"}}}
	newRS := func() *summaryRS {
		return &summaryRS{
			local: map[string]bool{}, state: map[string]map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent{},
			bulk: map[string]map[gomatrixserverlib.StateKeyTuple]string{}, memberships: map[string]string{"!far:remote.test|@alice:test": spec.Knock},
		}
	}
	alice := &userapi.Device{UserID: "@alice:test", ID: "A"}

	// Unauthenticated requests never make this server send requests to others.
	fed := &summaryFed{hierarchy: map[string]fclient.RoomHierarchyResponse{"remote.test": remote}}
	if code, body := summaryRequest(t, newRS(), fed, nil, "!far:remote.test", false, ""); code != http.StatusNotFound || len(fed.asked) != 0 {
		t.Errorf("anonymous: %d %s, asked %v; want 404 and no federation request", code, body.Raw, fed.asked)
	}
	fed = &summaryFed{aliases: map[string]fclient.RespDirectory{"#far:remote.test": {RoomID: "!far:remote.test", Servers: []spec.ServerName{"remote.test"}}}, hierarchy: map[string]fclient.RoomHierarchyResponse{"remote.test": remote}}
	if code, _ := summaryRequest(t, newRS(), fed, nil, "#far:remote.test", false, ""); code != http.StatusNotFound || fed.lookups != 0 {
		t.Errorf("anonymous alias on another server: got %d with %d lookups, want 404 and none", code, fed.lookups)
	}

	// A user gets the summary from the server of the room ID, and the membership this server knows.
	fed = &summaryFed{hierarchy: map[string]fclient.RoomHierarchyResponse{"remote.test": remote}}
	code, body := summaryRequest(t, newRS(), fed, alice, "!far:remote.test", false, "")
	if code != http.StatusOK || body.Get("name").String() != "Far away" || body.Get("room_type").String() != "m.space" || body.Get("num_joined_members").Int() != 7 {
		t.Fatalf("remote room: %d %s", code, body.Raw)
	}
	if body.Get("membership").String() != "knock" {
		t.Errorf("membership = %q, want the local knock", body.Get("membership").String())
	}
	if body.Get("room_version").Exists() || body.Get("encryption").Exists() {
		t.Errorf("the federation API does not tell the room version and encryption: %s", body.Raw)
	}

	// Through an alias on that server, using the servers it names.
	fed = &summaryFed{aliases: map[string]fclient.RespDirectory{"#far:remote.test": {RoomID: "!far:remote.test", Servers: []spec.ServerName{"other.test", "remote.test"}}}, hierarchy: map[string]fclient.RoomHierarchyResponse{"remote.test": remote}}
	if code, body = summaryRequest(t, newRS(), fed, alice, "#far:remote.test", false, ""); code != http.StatusOK || body.Get("room_id").String() != "!far:remote.test" {
		t.Errorf("remote alias: %d %s", code, body.Raw)
	}

	// An answer about another room is ignored, own and repeated servers are skipped, and only a few servers are asked.
	fed = &summaryFed{hierarchy: map[string]fclient.RoomHierarchyResponse{"evil.test": wrongRoom}}
	query := "?via=evil.test&via=evil.test&via=test&via=a.test&via=b.test&via=c.test&via=d.test"
	if code, body = summaryRequest(t, newRS(), fed, alice, "!far:remote.test", false, query); code != http.StatusNotFound {
		t.Errorf("wrong room: %d %s, want 404", code, body.Raw)
	}
	if len(fed.asked) != maxSummaryVias || fed.asked[0] != "evil.test" {
		t.Errorf("asked %v, want %d different servers, none of them this one", fed.asked, maxSummaryVias)
	}
	for _, s := range fed.asked {
		if s == "test" {
			t.Error("this server must not be asked")
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
