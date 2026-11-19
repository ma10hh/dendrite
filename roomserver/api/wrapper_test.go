package api

import (
	"testing"

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
