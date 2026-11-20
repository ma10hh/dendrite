// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/storage/tables"
	"github.com/element-hq/dendrite/roomserver/types"
)

const partialStateEventsSchema = `
-- The events that were stored while their room had partial state, until it has its full state.
CREATE TABLE IF NOT EXISTS roomserver_partial_state_events (
    event_nid BIGINT PRIMARY KEY,
    room_nid BIGINT NOT NULL,
    -- Whether the event was accepted without being checked against the state before it.
    recheck BOOLEAN NOT NULL
);
CREATE INDEX IF NOT EXISTS roomserver_partial_state_events_room_idx ON roomserver_partial_state_events (room_nid);
`

const insertPartialStateEventSQL = "" +
	"INSERT INTO roomserver_partial_state_events (event_nid, room_nid, recheck) VALUES ($1, $2, $3)" +
	" ON CONFLICT (event_nid) DO NOTHING"

const selectPartialStateEventsSQL = "" +
	"SELECT event_nid, recheck FROM roomserver_partial_state_events WHERE room_nid = $1 ORDER BY event_nid ASC"

const deletePartialStateEventsSQL = "" +
	"DELETE FROM roomserver_partial_state_events WHERE room_nid = $1"

type partialStateEventsStatements struct {
	insertStmt *sql.Stmt
	selectStmt *sql.Stmt
	deleteStmt *sql.Stmt
}

func CreatePartialStateEventsTable(db *sql.DB) error {
	_, err := db.Exec(partialStateEventsSchema)
	return err
}

func PreparePartialStateEventsTable(db *sql.DB) (tables.PartialStateEvents, error) {
	s := &partialStateEventsStatements{}
	return s, sqlutil.StatementList{
		{&s.insertStmt, insertPartialStateEventSQL},
		{&s.selectStmt, selectPartialStateEventsSQL},
		{&s.deleteStmt, deletePartialStateEventsSQL},
	}.Prepare(db)
}

func (s *partialStateEventsStatements) InsertPartialStateEvent(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, eventNID types.EventNID, recheck bool,
) error {
	_, err := sqlutil.TxStmt(txn, s.insertStmt).ExecContext(ctx, eventNID, roomNID, recheck)
	return err
}

func (s *partialStateEventsStatements) SelectPartialStateEvents(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID,
) ([]types.PartialStateEvent, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectStmt).QueryContext(ctx, roomNID)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectPartialStateEvents: rows.close() failed")
	var out []types.PartialStateEvent
	for rows.Next() {
		var e types.PartialStateEvent
		if err = rows.Scan(&e.EventNID, &e.Recheck); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *partialStateEventsStatements) DeletePartialStateEvents(ctx context.Context, txn *sql.Tx, roomNID types.RoomNID) error {
	_, err := sqlutil.TxStmt(txn, s.deleteStmt).ExecContext(ctx, roomNID)
	return err
}
