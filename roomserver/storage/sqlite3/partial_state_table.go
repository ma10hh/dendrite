// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/storage/tables"
	"github.com/element-hq/dendrite/roomserver/types"
)

const partialStateSchema = `
-- The rooms that were joined with the memberships of their state left out. A room is in here until the rest of its
-- state has been fetched.
CREATE TABLE IF NOT EXISTS roomserver_partial_state_rooms (
    room_nid BIGINT PRIMARY KEY,
    join_event_id TEXT NOT NULL,
    -- A JSON array of the servers that were in the room.
    servers TEXT NOT NULL,
    joined_at BIGINT NOT NULL
);
`

const insertPartialStateSQL = "" +
	"INSERT INTO roomserver_partial_state_rooms (room_nid, join_event_id, servers, joined_at) VALUES ($1, $2, $3, $4)" +
	" ON CONFLICT (room_nid) DO UPDATE SET join_event_id = $2, servers = $3, joined_at = $4"

const selectPartialStateSQL = "" +
	"SELECT join_event_id, servers, joined_at FROM roomserver_partial_state_rooms WHERE room_nid = $1"

const selectAllPartialStateSQL = "" +
	"SELECT room_nid, join_event_id, servers, joined_at FROM roomserver_partial_state_rooms ORDER BY joined_at ASC, room_nid ASC"

const deletePartialStateSQL = "" +
	"DELETE FROM roomserver_partial_state_rooms WHERE room_nid = $1"

type partialStateStatements struct {
	insertStmt    *sql.Stmt
	selectStmt    *sql.Stmt
	selectAllStmt *sql.Stmt
	deleteStmt    *sql.Stmt
}

func CreatePartialStateTable(db *sql.DB) error {
	_, err := db.Exec(partialStateSchema)
	return err
}

func PreparePartialStateTable(db *sql.DB) (tables.PartialStateRooms, error) {
	s := &partialStateStatements{}
	return s, sqlutil.StatementList{
		{&s.insertStmt, insertPartialStateSQL},
		{&s.selectStmt, selectPartialStateSQL},
		{&s.selectAllStmt, selectAllPartialStateSQL},
		{&s.deleteStmt, deletePartialStateSQL},
	}.Prepare(db)
}

func (s *partialStateStatements) InsertPartialState(ctx context.Context, txn *sql.Tx, state types.PartialState) error {
	servers, err := json.Marshal(state.Servers)
	if err != nil {
		return err
	}
	_, err = sqlutil.TxStmt(txn, s.insertStmt).ExecContext(ctx, state.RoomNID, state.JoinEventID, string(servers), state.JoinedAt)
	return err
}

func (s *partialStateStatements) SelectPartialState(ctx context.Context, txn *sql.Tx, roomNID types.RoomNID) (*types.PartialState, error) {
	state := types.PartialState{RoomNID: roomNID}
	var servers string
	err := sqlutil.TxStmt(txn, s.selectStmt).QueryRowContext(ctx, roomNID).Scan(&state.JoinEventID, &servers, &state.JoinedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(servers), &state.Servers); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *partialStateStatements) SelectAllPartialState(ctx context.Context, txn *sql.Tx) ([]types.PartialState, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectAllStmt).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectAllPartialState: rows.close() failed")
	var out []types.PartialState
	for rows.Next() {
		var state types.PartialState
		var servers string
		if err = rows.Scan(&state.RoomNID, &state.JoinEventID, &servers, &state.JoinedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(servers), &state.Servers); err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, rows.Err()
}

func (s *partialStateStatements) DeletePartialState(ctx context.Context, txn *sql.Tx, roomNID types.RoomNID) error {
	_, err := sqlutil.TxStmt(txn, s.deleteStmt).ExecContext(ctx, roomNID)
	return err
}
