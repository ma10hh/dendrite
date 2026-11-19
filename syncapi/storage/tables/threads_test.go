// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal/sqlutil"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage/postgres"
	"github.com/element-hq/dendrite/syncapi/storage/sqlite3"
	"github.com/element-hq/dendrite/syncapi/storage/tables"
	"github.com/element-hq/dendrite/test"
)

func TestSelectThreads(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice, bob, carol := test.NewUser(t), test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, alice)
		other := test.NewRoom(t, alice)
		for _, u := range []*test.User{bob, carol} {
			room.CreateAndInsert(t, u, "m.room.member", map[string]string{"membership": "join"}, test.WithStateKey(u.ID))
		}
		other.CreateAndInsert(t, bob, "m.room.member", map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))

		events, db, close := newOutputRoomEventsTable(t, dbType)
		defer close()
		var relations tables.Relations
		var err error
		switch dbType {
		case test.DBTypePostgres:
			relations, err = postgres.NewPostgresRelationsTable(db)
		case test.DBTypeSQLite:
			var stream sqlite3.StreamIDStatements
			if err = stream.Prepare(db); err != nil {
				t.Fatal(err)
			}
			relations, err = sqlite3.NewSqliteRelationsTable(db, &stream)
		}
		if err != nil {
			t.Fatal(err)
		}

		message := func(r *test.Room, sender *test.User) *rstypes.HeaderedEvent {
			ev := r.CreateAndInsert(t, sender, "m.room.message", map[string]string{"msgtype": "m.text", "body": "x"})
			// The sync API consumer sets this before it stores an event, and the sender column is filled from it.
			ev.UserID = *mustUserID(t, ev.SenderID())
			err := sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
				_, err := events.InsertEvent(ctx, txn, ev, nil, nil, nil, false, gomatrixserverlib.HistoryVisibilityShared)
				return err
			})
			if err != nil {
				t.Fatalf("InsertEvent: %s", err)
			}
			return ev
		}
		reply := func(r *test.Room, root *rstypes.HeaderedEvent, sender *test.User, relType string) {
			ev := message(r, sender)
			if err := relations.InsertRelation(ctx, nil, r.ID, root.EventID(), ev.EventID(), "m.room.message", relType); err != nil {
				t.Fatalf("InsertRelation: %s", err)
			}
		}

		root1, root2, root3, root4 := message(room, alice), message(room, bob), message(room, bob), message(room, bob)
		otherRoot := message(other, alice)
		// The order of the last replies makes the threads, newest first: 2, 1, 4, 3.
		reply(room, root1, bob, "m.thread")
		reply(room, root3, carol, "m.thread")
		reply(room, root2, carol, "m.thread")
		reply(room, root4, bob, "m.thread")
		reply(room, root1, bob, "m.thread")
		reply(room, root2, alice, "m.thread")
		reply(room, root3, bob, "m.annotation") // not a thread reply
		reply(other, otherRoot, bob, "m.thread")

		roots := func(onlyParticipated bool, before int64, limit int) []string {
			t.Helper()
			got, err := relations.SelectThreads(ctx, nil, room.ID, alice.ID, onlyParticipated, before, limit)
			if err != nil {
				t.Fatalf("SelectThreads: %s", err)
			}
			var out []string
			for _, e := range got {
				out = append(out, e.RootEventID)
			}
			return out
		}
		const noLimit = int64(1) << 60

		if got, want := roots(false, noLimit, 10), []string{root2.EventID(), root1.EventID(), root4.EventID(), root3.EventID()}; !reflect.DeepEqual(got, want) {
			t.Errorf("all threads: got %v, want %v", got, want)
		}
		if got, want := roots(true, noLimit, 10), []string{root2.EventID(), root1.EventID()}; !reflect.DeepEqual(got, want) {
			t.Errorf("participated: got %v, want %v (alice sent root1 and a reply in root2)", got, want)
		}
		if got, want := roots(false, noLimit, 2), []string{root2.EventID(), root1.EventID()}; !reflect.DeepEqual(got, want) {
			t.Errorf("limit 2: got %v, want %v", got, want)
		}

		all, err := relations.SelectThreads(ctx, nil, room.ID, alice.ID, false, noLimit, 10)
		if err != nil {
			t.Fatal(err)
		}
		wantDetails := map[string]struct {
			count        int
			participated bool
		}{root1.EventID(): {2, true}, root2.EventID(): {2, true}, root3.EventID(): {1, false}, root4.EventID(): {1, false}}
		for _, e := range all {
			want := wantDetails[e.RootEventID]
			if e.Count != want.count || e.Participated != want.participated || e.LatestEventID == "" || e.LatestRelationID == 0 {
				t.Errorf("thread %s: %+v, want count %d participated %v and a latest event", e.RootEventID, e, want.count, want.participated)
			}
		}
		for i := 1; i < len(all); i++ {
			if all[i].LatestRelationID >= all[i-1].LatestRelationID {
				t.Errorf("threads must be ordered by their latest reply, newest first: %+v", all)
			}
		}

		// Paging goes on from the latest reply of the last thread of the page before.
		first, err := relations.SelectThreads(ctx, nil, room.ID, alice.ID, false, noLimit, 2)
		if err != nil || len(first) != 2 {
			t.Fatalf("first page: %v %v", first, err)
		}
		if got, want := roots(false, first[1].LatestRelationID, 10), []string{root4.EventID(), root3.EventID()}; !reflect.DeepEqual(got, want) {
			t.Errorf("second page: got %v, want %v", got, want)
		}
		if got := roots(false, 1, 10); len(got) != 0 {
			t.Errorf("before the first relation there are no threads, got %v", got)
		}
		// Other rooms and users have their own view.
		if got, err := relations.SelectThreads(ctx, nil, room.ID, carol.ID, true, noLimit, 10); err != nil || len(got) != 2 {
			t.Errorf("carol took part in roots 2 and 3: %v %v", got, err)
		}
	})
}

func mustUserID(t *testing.T, sender spec.SenderID) *spec.UserID {
	t.Helper()
	id, err := spec.NewUserID(string(sender), true)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
