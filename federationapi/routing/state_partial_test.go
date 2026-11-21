package routing

import (
	"context"
	"net/http"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/fclient"

	"github.com/element-hq/dendrite/roomserver/api"
)

type partialRS struct {
	api.FederationRoomserverAPI
	partial bool
}

func (p partialRS) QueryServerJoinedToRoom(_ context.Context, _ *api.QueryServerJoinedToRoomRequest, res *api.QueryServerJoinedToRoomResponse) error {
	res.RoomExists, res.IsInRoom, res.PartialState = true, true, p.partial
	return nil
}

// A server that has joined a room with partial state does not hand out the state of the room: the other server would
// take it for the whole state.
func TestStateEndpointsRefuseRoomsWithPartialState(t *testing.T) {
	path := "/_matrix/federation/v1/state_ids/!room:remote.example?event_id=$e"
	request := fclient.NewFederationRequest("GET", "origin.example", "local.example", path)
	for name, handle := range map[string]func(partialRS) int{
		"state_ids": func(rs partialRS) int {
			return GetStateIDs(context.Background(), &request, rs, "!room:remote.example").Code
		},
		"state": func(rs partialRS) int {
			return GetState(context.Background(), &request, rs, "!room:remote.example").Code
		},
	} {
		if code := handle(partialRS{partial: true}); code != http.StatusNotFound {
			t.Errorf("%s of a room with partial state: %d, want 404", name, code)
		}
	}
}
