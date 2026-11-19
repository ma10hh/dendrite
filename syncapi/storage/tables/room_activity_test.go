// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal/sqlutil"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
)

func TestSelectRoomActivity(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		// Built per subtest: the sqlite and postgres subtests run in parallel and must not share event objects.
		alice := test.NewUser(t)
		busy := test.NewRoom(t, alice)
		quiet := test.NewRoom(t, alice)

		tab, db, close := newOutputRoomEventsTable(t, dbType)
		defer close()

		insert := func(txn *sql.Tx, ev *rstypes.HeaderedEvent, excludeFromSync bool) types.StreamPosition {
			t.Helper()
			pos, err := tab.InsertEvent(ctx, txn, ev, nil, nil, nil, excludeFromSync, gomatrixserverlib.HistoryVisibilityShared)
			if err != nil {
				t.Fatalf("InsertEvent: %s", err)
			}
			return pos
		}

		var lastBusy, lastBusyMessage, lastQuiet types.StreamPosition
		err := sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
			for _, ev := range busy.Events() {
				lastBusy = insert(txn, ev, false)
			}
			message := busy.CreateAndInsert(t, alice, "m.room.message", map[string]string{"msgtype": "m.text", "body": "hi"})
			lastBusyMessage = insert(txn, message, false)
			// A later event that must not bump the room.
			topic := busy.CreateAndInsert(t, alice, "m.room.topic", map[string]string{"topic": "x"}, test.WithStateKey(""))
			lastBusy = insert(txn, topic, false)
			// A later backfilled event is invisible to sync and must be ignored entirely.
			backfilled := busy.CreateAndInsert(t, alice, "m.room.message", map[string]string{"msgtype": "m.text", "body": "old"})
			insert(txn, backfilled, true)

			for _, ev := range quiet.Events() {
				lastQuiet = insert(txn, ev, false)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		bumpTypes := []string{spec.MRoomCreate, "m.room.message"}
		got, err := tab.SelectRoomActivity(ctx, nil, []string{busy.ID, quiet.ID, "!unknown:test"}, bumpTypes)
		if err != nil {
			t.Fatalf("SelectRoomActivity: %s", err)
		}

		if a := got[busy.ID]; a.LastEvent != lastBusy || a.LastBump != lastBusyMessage {
			t.Errorf("busy room: got %+v, want LastEvent=%d LastBump=%d", a, lastBusy, lastBusyMessage)
		}
		if lastBusy <= lastBusyMessage {
			t.Fatalf("test setup: the topic event must be newer than the message")
		}
		// The quiet room only has its creation events, and m.room.create is a bump type.
		if a := got[quiet.ID]; a.LastEvent != lastQuiet || a.LastBump == 0 || a.LastBump > lastQuiet {
			t.Errorf("quiet room: got %+v, want LastEvent=%d and a non-zero LastBump", a, lastQuiet)
		}
		a, ok := got["!unknown:test"]
		if !ok {
			t.Fatal("every requested room must be present in the result")
		}
		if a != (types.RoomActivity{}) {
			t.Errorf("unknown room: got %+v, want zero value", a)
		}

		// No bump types at all: ordering information is still returned.
		got, err = tab.SelectRoomActivity(ctx, nil, []string{busy.ID}, nil)
		if err != nil {
			t.Fatalf("SelectRoomActivity without bump types: %s", err)
		}
		if got[busy.ID].LastEvent != lastBusy || got[busy.ID].LastBump != 0 {
			t.Errorf("without bump types: got %+v", got[busy.ID])
		}

		// Many rooms at once, more than one SQLite statement chunk.
		ids := make([]string, 0, 1200)
		for i := 0; i < 1200; i++ {
			ids = append(ids, fmt.Sprintf("!room%d:test", i))
		}
		ids = append(ids, busy.ID)
		got, err = tab.SelectRoomActivity(ctx, nil, ids, bumpTypes)
		if err != nil {
			t.Fatalf("SelectRoomActivity with many rooms: %s", err)
		}
		if got[busy.ID].LastEvent != lastBusy {
			t.Errorf("many rooms: busy room got %+v, want LastEvent=%d", got[busy.ID], lastBusy)
		}
	})
}

func TestSelectStateEventsForRooms(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		first := test.NewRoom(t, alice)
		second := test.NewRoom(t, alice)

		tab, db, close := newCurrentRoomStateTable(t, dbType)
		defer close()

		err := sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
			for _, room := range []*test.Room{first, second} {
				for i, ev := range room.CurrentState() {
					ev.StateKeyResolved = ev.StateKey()
					userID, err := spec.NewUserID(string(ev.SenderID()), true)
					if err != nil {
						return err
					}
					ev.UserID = *userID
					if err = tab.UpsertRoomState(ctx, txn, ev, nil, types.StreamPosition(i)); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := tab.SelectStateEventsForRooms(ctx, nil, []string{first.ID, second.ID, "!unknown:test"}, spec.MRoomCreate, "")
		if err != nil {
			t.Fatalf("SelectStateEventsForRooms: %s", err)
		}
		if len(got) != 2 || got[first.ID] == nil || got[second.ID] == nil {
			t.Fatalf("got %d rooms, want exactly the two known rooms: %v", len(got), got)
		}
		if got[first.ID].EventID() == got[second.ID].EventID() || got[first.ID].RoomID().String() != first.ID {
			t.Errorf("each room must get its own create event")
		}

		got, err = tab.SelectStateEventsForRooms(ctx, nil, []string{first.ID}, "m.room.encryption", "")
		if err != nil {
			t.Fatalf("SelectStateEventsForRooms: %s", err)
		}
		if len(got) != 0 {
			t.Errorf("no room is encrypted, got %v", got)
		}

		got, err = tab.SelectStateEventsForRooms(ctx, nil, []string{first.ID}, spec.MRoomMember, alice.ID)
		if err != nil {
			t.Fatalf("SelectStateEventsForRooms: %s", err)
		}
		if got[first.ID] == nil {
			t.Errorf("the creator's membership event must be found by state key")
		}
	})
}

func TestSelectStateEventsWithKeys(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice, bob := test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, alice)
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(bob.ID))
		other := test.NewRoom(t, alice)

		tab, db, close := newCurrentRoomStateTable(t, dbType)
		defer close()

		err := sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
			for _, r := range []*test.Room{room, other} {
				for i, ev := range r.CurrentState() {
					ev.StateKeyResolved = ev.StateKey()
					userID, err := spec.NewUserID(string(ev.SenderID()), true)
					if err != nil {
						return err
					}
					ev.UserID = *userID
					if err = tab.UpsertRoomState(ctx, txn, ev, nil, types.StreamPosition(i)); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := tab.SelectStateEventsWithKeys(ctx, nil, room.ID, spec.MRoomMember, []string{alice.ID, bob.ID, "@nobody:test"})
		if err != nil {
			t.Fatalf("SelectStateEventsWithKeys: %s", err)
		}
		keys := map[string]bool{}
		for _, ev := range got {
			keys[*ev.StateKey()] = true
		}
		if len(got) != 2 || !keys[alice.ID] || !keys[bob.ID] {
			t.Errorf("got members %v, want alice and bob only", keys)
		}

		// Other rooms and other event types are never returned.
		if got, _ = tab.SelectStateEventsWithKeys(ctx, nil, room.ID, spec.MRoomCreate, []string{alice.ID}); len(got) != 0 {
			t.Errorf("a create event has no state key of a user, got %d events", len(got))
		}
		if got, _ = tab.SelectStateEventsWithKeys(ctx, nil, room.ID, spec.MRoomMember, nil); len(got) != 0 {
			t.Errorf("no keys must return nothing, got %d", len(got))
		}

		// More keys than fit in one SQLite statement.
		many := make([]string, 0, 1200)
		for i := 0; i < 1200; i++ {
			many = append(many, fmt.Sprintf("@user%d:test", i))
		}
		many = append(many, bob.ID)
		if got, err = tab.SelectStateEventsWithKeys(ctx, nil, room.ID, spec.MRoomMember, many); err != nil || len(got) != 1 {
			t.Errorf("many keys: got %d events, err %v, want exactly bob", len(got), err)
		}
	})
}
