package state

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

var fuzzUsers = []string{"@alice:example.com", "@bob:example.com", "@carol:example.com"}

type fuzzTuple struct{ eventType, stateKey string }

// fuzzRoom builds the events of a room from the bytes: a fixed start, then one state event for each three bytes.
func fuzzRoom(t testing.TB, data []byte) (events []gomatrixserverlib.PDU, tuples map[string]fuzzTuple) {
	version := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV10)
	tuples = map[string]fuzzTuple{}
	add := func(sender, eventType, stateKey string, ts int, content any) {
		id := fmt.Sprintf("$e%d:example.com", len(events))
		raw, err := json.Marshal(map[string]any{
			"event_id": id, "room_id": "!room:example.com", "sender": sender, "type": eventType, "state_key": stateKey,
			"origin_server_ts": ts, "depth": len(events) + 1, "content": content,
			"auth_events": []string{}, "prev_events": []string{}, "origin": "example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		ev, err := version.NewEventFromTrustedJSON(raw, false)
		if err != nil {
			t.Fatalf("building an event: %v", err)
		}
		events = append(events, ev)
		tuples[ev.EventID()] = fuzzTuple{eventType, stateKey}
	}
	add(fuzzUsers[0], spec.MRoomCreate, "", 1, map[string]any{"creator": fuzzUsers[0], "room_version": "10"})
	add(fuzzUsers[0], spec.MRoomMember, fuzzUsers[0], 2, map[string]any{"membership": "join"})
	add(fuzzUsers[0], spec.MRoomPowerLevels, "", 3, map[string]any{"users": map[string]int{fuzzUsers[0]: 100}})
	add(fuzzUsers[0], spec.MRoomJoinRules, "", 4, map[string]any{"join_rule": "public"})

	memberships := []string{"join", "leave", "ban", "invite"}
	for i := 0; i+2 < len(data) && i < 3*24; i += 3 {
		kind, who, value := int(data[i]), int(data[i+1]), int(data[i+2])
		sender := fuzzUsers[who%len(fuzzUsers)]
		ts := 10 + (kind>>4)*7 + i
		switch kind % 5 {
		case 0, 1:
			target := fuzzUsers[(who/len(fuzzUsers))%len(fuzzUsers)]
			add(sender, spec.MRoomMember, target, ts, map[string]any{"membership": memberships[value%len(memberships)]})
		case 2:
			add(sender, spec.MRoomPowerLevels, "", ts, map[string]any{"users": map[string]int{
				fuzzUsers[0]: 100, fuzzUsers[1]: (value%4)*50 - 50, fuzzUsers[2]: value % 101,
			}})
		case 3:
			add(sender, spec.MRoomJoinRules, "", ts, map[string]any{"join_rule": []string{"public", "invite", "knock"}[value%3]})
		default:
			add(sender, "m.room.topic", "", ts, map[string]any{"topic": fmt.Sprint(value)})
		}
	}
	return events, tuples
}

func resolveForFuzz(events []gomatrixserverlib.PDU, tuples map[string]fuzzTuple, shuffle func([]gomatrixserverlib.PDU)) []string {
	byTuple := map[fuzzTuple][]gomatrixserverlib.PDU{}
	for _, ev := range events {
		byTuple[tuples[ev.EventID()]] = append(byTuple[tuples[ev.EventID()]], ev)
	}
	var conflicted, unconflicted []gomatrixserverlib.PDU
	for _, group := range byTuple {
		if len(group) > 1 {
			conflicted = append(conflicted, group...)
		} else {
			unconflicted = append(unconflicted, group...)
		}
	}
	auth := slices.Clone(events)
	if shuffle != nil {
		shuffle(conflicted)
		shuffle(unconflicted)
		shuffle(auth)
	}
	resolved := gomatrixserverlib.ResolveStateConflictsV2(conflicted, unconflicted, auth,
		func(_ spec.RoomID, sender spec.SenderID) (*spec.UserID, error) {
			return spec.NewUserID(string(sender), true)
		},
		func(string) bool { return false },
	)
	ids := make([]string, 0, len(resolved))
	for _, ev := range resolved {
		ids = append(ids, ev.EventID())
	}
	slices.Sort(ids)
	return ids
}

// FuzzResolveStateV2 checks that resolving the same events always gives the same state whatever order they are in,
// that the state has one event for each type and state key, and that resolving does not panic.
func FuzzResolveStateV2(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 1, 0, 3, 1, 2, 0, 50, 1, 4, 0, 5})
	f.Add([]byte{0, 1, 2, 0, 1, 2, 5, 3, 7, 4, 3, 9, 3, 1, 0, 1, 7, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		events, tuples := fuzzRoom(t, data)
		first := resolveForFuzz(events, tuples, nil)

		seen := map[fuzzTuple]string{}
		for _, id := range first {
			tuple := tuples[id]
			if other, ok := seen[tuple]; ok {
				t.Fatalf("two events for %v: %s and %s", tuple, other, id)
			}
			seen[tuple] = id
		}
		for round := 0; round < 4; round++ {
			r := round
			again := resolveForFuzz(events, tuples, func(evs []gomatrixserverlib.PDU) {
				slices.Reverse(evs)
				if r%2 == 1 && len(evs) > 1 {
					evs[0], evs[len(evs)-1] = evs[len(evs)-1], evs[0]
				}
			})
			if !slices.Equal(first, again) {
				t.Fatalf("the order of the input changed the result:\n%v\n%v", first, again)
			}
		}
	})
}
