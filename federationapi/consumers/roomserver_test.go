// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/element-hq/dendrite/federationapi/types"
)

func TestCombineNoOp(t *testing.T) {
	inputAdd1 := []string{"a", "b", "c"}
	inputDel1 := []string{"a", "b", "d"}
	inputAdd2 := []string{"a", "d", "e"}
	inputDel2 := []string{"a", "c", "e", "e"}

	gotAdd, gotDel := combineDeltas(inputAdd1, inputDel1, inputAdd2, inputDel2)

	if len(gotAdd) != 0 {
		t.Errorf("wanted combined adds to be an empty list, got %#v", gotAdd)
	}

	if len(gotDel) != 0 {
		t.Errorf("wanted combined removes to be an empty list, got %#v", gotDel)
	}
}

func TestCombineDedup(t *testing.T) {
	inputAdd1 := []string{"a", "a"}
	inputDel1 := []string{"b", "b"}
	inputAdd2 := []string{"a", "a"}
	inputDel2 := []string{"b", "b"}

	gotAdd, gotDel := combineDeltas(inputAdd1, inputDel1, inputAdd2, inputDel2)

	if len(gotAdd) != 1 || gotAdd[0] != "a" {
		t.Errorf("wanted combined adds to be %#v, got %#v", []string{"a"}, gotAdd)
	}

	if len(gotDel) != 1 || gotDel[0] != "b" {
		t.Errorf("wanted combined removes to be %#v, got %#v", []string{"b"}, gotDel)
	}
}

func TestWithPartialStateHosts(t *testing.T) {
	known := []types.JoinedHost{{MemberEventID: "$a", ServerName: "a.example"}}
	got := WithPartialStateHosts("!room:a.example", known, []string{"a.example", "b.example", "b.example", "c.example", "not a server name", "bad..example:99999", ""})
	want := []types.JoinedHost{
		{MemberEventID: "$a", ServerName: "a.example"},
		{MemberEventID: "partial-state:!room:a.example:b.example", ServerName: "b.example"},
		{MemberEventID: "partial-state:!room:a.example:c.example", ServerName: "c.example"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := WithPartialStateHosts("!room:a.example", known, nil); !reflect.DeepEqual(got, known) {
		t.Fatalf("nothing to add: got %v", got)
	}

	// The ID is different in every room: the table of joined hosts has a unique index on it, so a second room through
	// the same server would have had no stand-in at all.
	second := WithPartialStateHosts("!other:a.example", nil, []string{"b.example"})
	if len(second) != 1 || second[0].MemberEventID == want[1].MemberEventID {
		t.Fatalf("the stand-in of the second room has the ID of the first: %v", second)
	}

	// A server that answered the join cannot make this one send the room to an unbounded number of servers.
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, fmt.Sprintf("s%d.example", i))
	}
	if got := WithPartialStateHosts("!room:a.example", nil, many); len(got) != maxPartialStateHosts {
		t.Fatalf("%d stand-in hosts, want at most %d", len(got), maxPartialStateHosts)
	}
}
