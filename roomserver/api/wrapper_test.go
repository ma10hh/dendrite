package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/stretchr/testify/assert"
)

func TestFilterPublicRooms(t *testing.T) {
	rooms := []fclient.PublicRoom{
		{RoomID: "!space", Name: "Team space", RoomType: "m.space"},
		{RoomID: "!plain", Name: "Team chat", Topic: "Lunch"},
		{RoomID: "!other", Name: "Other", CanonicalAlias: "#lunch:example.org", RoomType: "org.example.custom"},
	}
	ids := func(rooms []fclient.PublicRoom) (out []string) {
		out = []string{}
		for _, r := range rooms {
			out = append(out, r.RoomID)
		}
		return
	}
	for _, tc := range []struct {
		name  string
		term  string
		types []string
		want  []string
	}{
		{"no filter", "", nil, []string{"!space", "!plain", "!other"}},
		{"a type", "", []string{"m.space"}, []string{"!space"}},
		{"no type", "", []string{""}, []string{"!plain"}},
		{"types and no type", "", []string{"m.space", ""}, []string{"!space", "!plain"}},
		{"a term in the name, any case", "TEAM", nil, []string{"!space", "!plain"}},
		{"a term in the topic", "lunch", nil, []string{"!plain", "!other"}},
		{"a term and a type", "lunch", []string{""}, []string{"!plain"}},
		{"nothing matches", "nothing", nil, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ids(FilterPublicRooms(rooms, tc.term, tc.types)))
		})
	}
}

type bulkState struct {
	rooms map[string]map[gomatrixserverlib.StateKeyTuple]string
}

func (b bulkState) QueryBulkStateContent(_ context.Context, _ *QueryBulkStateContentRequest, res *QueryBulkStateContentResponse) error {
	res.Rooms = b.rooms
	return nil
}

// A client pages through the rooms with an offset, so the rooms come in the order of the IDs that were asked for, not
// in the random order of a map.
func TestPopulatePublicRoomsKeepsTheOrder(t *testing.T) {
	rooms := map[string]map[gomatrixserverlib.StateKeyTuple]string{}
	var ids []string
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("!room%02d:example.com", i)
		ids = append(ids, id)
		rooms[id] = map[gomatrixserverlib.StateKeyTuple]string{{EventType: "m.room.name"}: id}
	}
	for round := 0; round < 20; round++ {
		got, err := PopulatePublicRooms(context.Background(), ids, bulkState{rooms})
		if err != nil {
			t.Fatal(err)
		}
		for i, room := range got {
			if room.RoomID != ids[i] {
				t.Fatalf("round %d: room %d is %s, want %s", round, i, room.RoomID, ids[i])
			}
		}
	}
	// A room the roomserver does not know is left out, and the others stay in order.
	got, err := PopulatePublicRooms(context.Background(), []string{ids[3], "!gone:example.com", ids[1]}, bulkState{rooms})
	if err != nil || len(got) != 2 || got[0].RoomID != ids[3] || got[1].RoomID != ids[1] {
		t.Errorf("got %+v, %v", got, err)
	}
}
