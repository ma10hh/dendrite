package slidingsync

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/element-hq/dendrite/test"
)

// TestLoadLargeAccount times the main requests of an account with many rooms. It only runs when LOAD_TEST is
// set, with LOAD_ROOMS rooms (default 500): LOAD_TEST=1 LOAD_ROOMS=2000 go test ./syncapi/slidingsync -run LoadLargeAccount -v
// The numbers are logged, not asserted: they depend on the machine and the database.
func TestLoadLargeAccount(t *testing.T) {
	if os.Getenv("LOAD_TEST") == "" {
		t.Skip("set LOAD_TEST=1 to run")
	}
	rooms := 500
	if v, err := strconv.Atoi(os.Getenv("LOAD_ROOMS")); err == nil && v > 0 {
		rooms = v
	}
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)

		start := time.Now()
		all := make([]*test.Room, rooms)
		space := test.NewRoom(t, hn.alice)
		hn.write(space.Events()...)
		for i := range all {
			r := test.NewRoom(t, hn.alice)
			for j := 0; j < 4; j++ {
				message(t, r, hn.alice, fmt.Sprintf("message %d in room %d", j, i))
			}
			if i%5 == 0 {
				space.CreateAndInsert(t, hn.alice, "m.space.child", map[string]any{"via": []string{"test"}}, test.WithStateKey(r.ID))
			}
			all[i] = r
		}
		hn.write(space.Events()[len(space.Events())-(rooms+4)/5:]...)
		for _, r := range all {
			hn.write(r.Events()...)
		}
		tags := map[string]map[string]json.RawMessage{}
		for i, r := range all {
			if i%4 == 0 {
				tags[r.ID] = map[string]json.RawMessage{"m.tag": json.RawMessage(`{"tags":{"m.favourite":{"order":0.5}}}`)}
			}
		}
		hn.users.room = tags
		t.Logf("%d rooms with %d events written in %v", rooms, rooms*9, time.Since(start).Round(time.Millisecond))

		timed := func(name, pos, body string) gjsonResult {
			t.Helper()
			start := time.Now()
			code, res := hn.post(pos, body)
			d := time.Since(start)
			if code != 200 {
				t.Fatalf("%s: HTTP %d %s", name, code, res.Raw)
			}
			t.Logf("%-46s %8v  %d rooms, %d bytes", name, d.Round(time.Millisecond), len(res.Get("rooms").Map()), len(res.Raw))
			return gjsonResult{res.Get("pos").String()}
		}

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		timed("initial, window of 20, timeline 1 (first)", "", listReq("[[0,19]]", 1, `,"required_state":[["m.room.name",""],["m.room.create",""]]`))
		timed("initial, window of 100, timeline 10", "", listReq("[[0,99]]", 10, ""))
		timed("initial, everything, timeline 1", "", listReq("[[0,"+strconv.Itoa(rooms)+"]]", 1, ""))
		timed("initial, favourites (tags filter)", "", listReq("[[0,99]]", 1, `,"filters":{"tags":["m.favourite"]}`))
		timed("initial, not favourites (not_tags filter)", "", listReq("[[0,99]]", 1, `,"filters":{"not_tags":["m.favourite"]}`))
		timed("initial, children of a space (spaces filter)", "", listReq("[[0,99]]", 1, `,"filters":{"spaces":["`+space.ID+`"]}`))
		timed("initial, encrypted filter", "", listReq("[[0,99]]", 1, `,"filters":{"is_encrypted":true}`))

		// Each request without pos replaces the session, so the incremental steps start from a fresh one.
		first := timed("initial, window of 20, timeline 1", "", listReq("[[0,19]]", 1, `,"required_state":[["m.room.name",""],["m.room.create",""]]`))
		// An incremental request with nothing new, then one after a message in a room far down the list.
		next := timed("incremental, nothing changed", first.pos, listReq("[[0,19]]", 1, `,"required_state":[["m.room.name",""],["m.room.create",""]]`))
		ev := message(t, all[rooms-1], hn.alice, "late message")
		hn.write(ev)
		timed("incremental, one new message", next.pos, listReq("[[0,19]]", 1, `,"required_state":[["m.room.name",""],["m.room.create",""]]`))

		runtime.GC()
		runtime.ReadMemStats(&after)
		t.Logf("heap in use after the requests: %d KiB (before: %d KiB)", after.HeapInuse/1024, before.HeapInuse/1024)
	})
}

type gjsonResult struct{ pos string }
