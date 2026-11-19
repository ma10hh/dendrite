// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"reflect"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/test"
)

func TestFilterState(t *testing.T) {
	user := test.NewUser(t)
	other := test.NewUser(t)
	room := test.NewRoom(t, user)
	name := room.CreateAndInsert(t, user, spec.MRoomName, map[string]interface{}{"name": "n"}, test.WithStateKey(""))
	member := room.CreateAndInsert(t, other, spec.MRoomMember, map[string]interface{}{"membership": "join"}, test.WithStateKey(other.ID))
	in := []*rstypes.HeaderedEvent{name, member}
	ids := func(events []*rstypes.HeaderedEvent) (out []string) {
		for _, e := range events {
			out = append(out, e.EventID())
		}
		return
	}
	for _, tc := range []struct {
		desc   string
		filter synctypes.StateFilter
		want   []*rstypes.HeaderedEvent
	}{
		{"no filter", synctypes.StateFilter{}, in},
		{"types", synctypes.StateFilter{Types: &[]string{spec.MRoomName}}, []*rstypes.HeaderedEvent{name}},
		{"type prefix", synctypes.StateFilter{Types: &[]string{"m.room.*"}}, in},
		{"not types", synctypes.StateFilter{NotTypes: &[]string{spec.MRoomMember}}, []*rstypes.HeaderedEvent{name}},
		{"senders", synctypes.StateFilter{Senders: &[]string{other.ID}}, []*rstypes.HeaderedEvent{member}},
		{"not senders", synctypes.StateFilter{NotSenders: &[]string{other.ID}}, []*rstypes.HeaderedEvent{name}},
	} {
		if got, want := ids(filterState(in, &tc.filter)), ids(tc.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", tc.desc, got, want)
		}
	}
}
