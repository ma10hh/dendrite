// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/storage/tables"
)

const blockedRoomsSchema = `
-- The rooms that local users may not join, by room ID. The room need not exist on this server.
CREATE TABLE IF NOT EXISTS roomserver_blocked_rooms (
    room_id TEXT PRIMARY KEY,
    -- When the room was blocked, in milliseconds since the epoch.
    blocked_at BIGINT NOT NULL
);
`

const insertBlockedRoomSQL = "" +
	"INSERT INTO roomserver_blocked_rooms (room_id, blocked_at) VALUES ($1, $2) ON CONFLICT (room_id) DO NOTHING"

const deleteBlockedRoomSQL = "" +
	"DELETE FROM roomserver_blocked_rooms WHERE room_id = $1"

const selectBlockedRoomSQL = "" +
	"SELECT 1 FROM roomserver_blocked_rooms WHERE room_id = $1"

type blockedRoomsStatements struct {
	insertStmt *sql.Stmt
	deleteStmt *sql.Stmt
	selectStmt *sql.Stmt
}

func CreateBlockedRoomsTable(db *sql.DB) error {
	_, err := db.Exec(blockedRoomsSchema)
	return err
}

func PrepareBlockedRoomsTable(db *sql.DB) (tables.BlockedRooms, error) {
	s := &blockedRoomsStatements{}
	return s, sqlutil.StatementList{
		{&s.insertStmt, insertBlockedRoomSQL},
		{&s.deleteStmt, deleteBlockedRoomSQL},
		{&s.selectStmt, selectBlockedRoomSQL},
	}.Prepare(db)
}

func (s *blockedRoomsStatements) InsertBlockedRoom(ctx context.Context, txn *sql.Tx, roomID string) error {
	_, err := sqlutil.TxStmt(txn, s.insertStmt).ExecContext(ctx, roomID, time.Now().UnixMilli())
	return err
}

func (s *blockedRoomsStatements) DeleteBlockedRoom(ctx context.Context, txn *sql.Tx, roomID string) error {
	_, err := sqlutil.TxStmt(txn, s.deleteStmt).ExecContext(ctx, roomID)
	return err
}

func (s *blockedRoomsStatements) IsRoomBlocked(ctx context.Context, txn *sql.Tx, roomID string) (bool, error) {
	var one int
	err := sqlutil.TxStmt(txn, s.selectStmt).QueryRowContext(ctx, roomID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
