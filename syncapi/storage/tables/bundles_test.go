// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/internal/sqlutil"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage/postgres"
	"github.com/element-hq/dendrite/syncapi/storage/sqlite3"
	"github.com/element-hq/dendrite/syncapi/storage/tables"
	"github.com/element-hq/dendrite/test"
)

func TestSelectBundles(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice, bob := test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, alice)
		other := test.NewRoom(t, alice)
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))

		events, db, close := newOutputRoomEventsTable(t, dbType)
		defer close()
		var relations tables.Relations
		var err error
		switch dbType {
		case test.DBTypePostgres:
			relations, err = postgres.NewPostgresRelationsTable(db)
		case test.DBTypeSQLite:
			var stream sqlite3.StreamIDStatements
			require.NoError(t, stream.Prepare(db))
			relations, err = sqlite3.NewSqliteRelationsTable(db, &stream)
		}
		require.NoError(t, err)

		send := func(r *test.Room, sender *test.User, eventType string, content map[string]any) *rstypes.HeaderedEvent {
			ev := r.CreateAndInsert(t, sender, eventType, content)
			ev.UserID = *mustUserID(t, ev.SenderID())
			require.NoError(t, sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
				_, err := events.InsertEvent(ctx, txn, ev, nil, nil, nil, false, gomatrixserverlib.HistoryVisibilityShared)
				return err
			}))
			return ev
		}
		relate := func(r *test.Room, target, child *rstypes.HeaderedEvent, relType string) {
			require.NoError(t, relations.InsertRelation(ctx, nil, r.ID, target.EventID(), child.EventID(), child.Type(), relType))
		}
		msg := map[string]any{"msgtype": "m.text", "body": "x"}

		original := send(room, alice, "m.room.message", msg)
		plain := send(room, alice, "m.room.message", msg)
		goodEdit := send(room, alice, "m.room.message", msg)
		editByOther := send(room, bob, "m.room.message", msg)
		editOfOtherType := send(room, alice, "m.sticker", msg)
		reply1, reply2 := send(room, bob, "m.room.message", msg), send(room, alice, "m.room.message", msg)
		ref := send(room, bob, "m.key.verification.done", msg)
		inOther := send(other, alice, "m.room.message", msg)
		otherEdit := send(other, alice, "m.room.message", msg)
		relate(room, original, goodEdit, "m.replace")
		relate(room, original, editByOther, "m.replace")
		relate(room, original, editOfOtherType, "m.replace")
		relate(room, original, reply1, "m.thread")
		relate(room, original, reply2, "m.thread")
		relate(room, original, ref, "m.reference")
		relate(other, inOther, otherEdit, "m.replace")

		got, err := relations.SelectBundles(ctx, nil, room.ID, []string{original.EventID(), plain.EventID(), inOther.EventID()}, bob.ID, true)
		require.NoError(t, err)

		assert.Equal(t, []string{goodEdit.EventID()}, got.Edits[original.EventID()], "only an edit by the sender of the event, of its type, in its room")
		assert.NotContains(t, got.Edits, inOther.EventID(), "other rooms are not asked about")
		assert.NotContains(t, got.Edits, plain.EventID())

		require.Contains(t, got.Threads, original.EventID())
		thread := got.Threads[original.EventID()]
		assert.Equal(t, 2, thread.Count)
		assert.Equal(t, reply2.EventID(), thread.LatestEventID)
		assert.True(t, thread.Participated, "bob sent a reply")
		assert.Equal(t, []string{reply1.EventID(), reply2.EventID()}, []string{got.ThreadChildren[original.EventID()][0].EventID, got.ThreadChildren[original.EventID()][1].EventID})
		assert.Equal(t, bob.ID, got.ThreadChildren[original.EventID()][0].Sender)

		assert.Equal(t, []string{ref.EventID()}, []string{got.References[original.EventID()][0].EventID})
		assert.Len(t, got.References[original.EventID()], 1)

		noChildren, err := relations.SelectBundles(ctx, nil, room.ID, []string{original.EventID()}, alice.ID, false)
		require.NoError(t, err)
		assert.Empty(t, noChildren.ThreadChildren)
		assert.True(t, noChildren.Threads[original.EventID()].Participated)

		nothing, err := relations.SelectBundles(ctx, nil, room.ID, nil, alice.ID, true)
		require.NoError(t, err)
		assert.Empty(t, nothing.Threads)
	})
}
