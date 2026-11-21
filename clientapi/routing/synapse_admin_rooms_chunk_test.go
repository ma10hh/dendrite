package routing

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"

	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
)

type chunkRS struct {
	roomserverAPI.ClientRoomserverAPI
	calls    []int
	maxQuery int
}

func (c *chunkRS) QueryPublishedRooms(context.Context, *roomserverAPI.QueryPublishedRoomsRequest, *roomserverAPI.QueryPublishedRoomsResponse) error {
	return nil
}

func (c *chunkRS) QueryBulkStateContent(_ context.Context, req *roomserverAPI.QueryBulkStateContentRequest, res *roomserverAPI.QueryBulkStateContentResponse) error {
	c.calls = append(c.calls, len(req.RoomIDs))
	res.Rooms = map[string]map[gomatrixserverlib.StateKeyTuple]string{}
	for _, id := range req.RoomIDs {
		res.Rooms[id] = map[gomatrixserverlib.StateKeyTuple]string{{EventType: "m.room.name"}: id}
	}
	return nil
}

// The state of the rooms is read a chunk at a time, however many rooms there are: one query with a variable for every
// event of every room is more than SQLite takes.
func TestAdminRoomsReadsTheStateInChunks(t *testing.T) {
	var ids []string
	for i := 0; i < 450; i++ {
		ids = append(ids, fmt.Sprintf("!room%03d:test", i))
	}
	rs := &chunkRS{}
	req := httptest.NewRequest("GET", "/_synapse/admin/v1/rooms", nil)
	rooms, err := adminRooms(req, &config.ClientAPI{}, rs, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 450 {
		t.Fatalf("%d rooms, want 450", len(rooms))
	}
	if len(rs.calls) != 3 || rs.calls[0] != adminRoomsChunk || rs.calls[2] != 450-2*adminRoomsChunk {
		t.Errorf("calls with %v rooms, want chunks of %d", rs.calls, adminRoomsChunk)
	}
	for i, room := range rooms {
		if room.RoomID != ids[i] || room.Name == nil || *room.Name != ids[i] {
			t.Fatalf("room %d is %+v, want the one of %s with its state", i, room, ids[i])
		}
	}
}
