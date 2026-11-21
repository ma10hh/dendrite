package routing

import (
	"context"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"

	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
)

type countingBulk struct{ calls int }

func (c *countingBulk) QueryBulkStateContent(_ context.Context, req *roomserverAPI.QueryBulkStateContentRequest, res *roomserverAPI.QueryBulkStateContentResponse) error {
	c.calls++
	res.Rooms = map[string]map[gomatrixserverlib.StateKeyTuple]string{}
	for _, id := range req.RoomIDs {
		res.Rooms[id] = map[gomatrixserverlib.StateKeyTuple]string{{EventType: "m.room.name"}: id}
	}
	return nil
}

// The state of all the published rooms is read once for the requests that filter them, however often they come.
func TestPopulatedPublicRoomsAreCached(t *testing.T) {
	populated.byNetwork = nil
	rs := &countingBulk{}
	ctx := context.Background()
	now := time.Now()
	ids := []string{"!a:test", "!b:test"}
	for i := 0; i < 5; i++ {
		if _, err := populatedPublicRooms(ctx, rs, "", ids, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if rs.calls != 1 {
		t.Errorf("the state was read %d times for five requests, want once", rs.calls)
	}
	// Other rooms, another network and an old entry are read again.
	if _, err := populatedPublicRooms(ctx, rs, "", append(ids, "!c:test"), now.Add(6*time.Second)); err != nil || rs.calls != 2 {
		t.Errorf("a new room must be seen at once: %d calls, %v", rs.calls, err)
	}
	if _, err := populatedPublicRooms(ctx, rs, "other", ids, now.Add(7*time.Second)); err != nil || rs.calls != 3 {
		t.Errorf("another network has its own entry: %d calls, %v", rs.calls, err)
	}
	if _, err := populatedPublicRooms(ctx, rs, "", append(ids, "!c:test"), now.Add(6*time.Second+populatedTTL)); err != nil || rs.calls != 4 {
		t.Errorf("an old entry is read again: %d calls, %v", rs.calls, err)
	}
}
