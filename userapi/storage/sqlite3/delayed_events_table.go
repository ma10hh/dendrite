// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/element-hq/dendrite/userapi/storage/tables"
)

const delayedEventsSchema = `
-- Events that a user asked to be sent later (MSC4140), until they are sent, cancelled or expire.
CREATE TABLE IF NOT EXISTS userapi_delayed_events (
	delay_id TEXT NOT NULL PRIMARY KEY,
	user_id TEXT NOT NULL,
	device_id TEXT NOT NULL,
	room_id TEXT NOT NULL,
	event_type TEXT NOT NULL,
	-- NULL for events that are not state events.
	state_key TEXT,
	content TEXT NOT NULL,
	delay_ms BIGINT NOT NULL,
	-- When the delay started, or was last restarted (milliseconds).
	running_since_ms BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS userapi_delayed_events_user_idx ON userapi_delayed_events (user_id);
CREATE INDEX IF NOT EXISTS userapi_delayed_events_state_idx ON userapi_delayed_events (room_id, event_type, state_key);
`

const delayedEventColumns = "delay_id, user_id, device_id, room_id, event_type, state_key, content, delay_ms, running_since_ms"

const insertDelayedEventSQL = "INSERT INTO userapi_delayed_events (" + delayedEventColumns + ") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
const selectDelayedEventSQL = "SELECT " + delayedEventColumns + " FROM userapi_delayed_events WHERE delay_id = $1"
const selectDelayedEventsForUserSQL = "SELECT " + delayedEventColumns + " FROM userapi_delayed_events WHERE user_id = $1 ORDER BY running_since_ms + delay_ms ASC, delay_id ASC"
const selectAllDelayedEventsSQL = "SELECT " + delayedEventColumns + " FROM userapi_delayed_events"
const deleteDelayedEventSQL = "DELETE FROM userapi_delayed_events WHERE delay_id = $1"
const restartDelayedEventSQL = "UPDATE userapi_delayed_events SET running_since_ms = $1 WHERE delay_id = $2"
const selectDelayedStateEventIDsSQL = "SELECT delay_id FROM userapi_delayed_events WHERE room_id = $1 AND event_type = $2 AND state_key = $3"
const countDelayedEventsForUserSQL = "SELECT COUNT(*) FROM userapi_delayed_events WHERE user_id = $1"

type delayedEventsStatements struct {
	insertStmt            *sql.Stmt
	selectStmt            *sql.Stmt
	selectForUserStmt     *sql.Stmt
	selectAllStmt         *sql.Stmt
	deleteStmt            *sql.Stmt
	restartStmt           *sql.Stmt
	selectStateEventsStmt *sql.Stmt
	countForUserStmt      *sql.Stmt
}

func NewSQLiteDelayedEventsTable(db *sql.DB) (tables.DelayedEventsTable, error) {
	s := &delayedEventsStatements{}
	if _, err := db.Exec(delayedEventsSchema); err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.insertStmt, insertDelayedEventSQL},
		{&s.selectStmt, selectDelayedEventSQL},
		{&s.selectForUserStmt, selectDelayedEventsForUserSQL},
		{&s.selectAllStmt, selectAllDelayedEventsSQL},
		{&s.deleteStmt, deleteDelayedEventSQL},
		{&s.restartStmt, restartDelayedEventSQL},
		{&s.selectStateEventsStmt, selectDelayedStateEventIDsSQL},
		{&s.countForUserStmt, countDelayedEventsForUserSQL},
	}.Prepare(db)
}

func scanDelayedEvent(row interface{ Scan(...any) error }) (api.DelayedEvent, error) {
	var e api.DelayedEvent
	var stateKey sql.NullString
	var content string
	if err := row.Scan(&e.DelayID, &e.UserID, &e.DeviceID, &e.RoomID, &e.EventType, &stateKey, &content, &e.DelayMS, &e.RunningSinceMS); err != nil {
		return api.DelayedEvent{}, err
	}
	if stateKey.Valid {
		e.StateKey = &stateKey.String
	}
	e.Content = []byte(content)
	return e, nil
}

func (s *delayedEventsStatements) Insert(ctx context.Context, txn *sql.Tx, e api.DelayedEvent) error {
	var stateKey sql.NullString
	if e.StateKey != nil {
		stateKey = sql.NullString{String: *e.StateKey, Valid: true}
	}
	_, err := sqlutil.TxStmt(txn, s.insertStmt).ExecContext(ctx, e.DelayID, e.UserID, e.DeviceID, e.RoomID, e.EventType, stateKey, string(e.Content), e.DelayMS, e.RunningSinceMS)
	return err
}

func (s *delayedEventsStatements) Select(ctx context.Context, txn *sql.Tx, delayID string) (*api.DelayedEvent, error) {
	e, err := scanDelayedEvent(sqlutil.TxStmt(txn, s.selectStmt).QueryRowContext(ctx, delayID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func scanDelayedEvents(ctx context.Context, rows *sql.Rows) ([]api.DelayedEvent, error) {
	defer internal.CloseAndLogIfError(ctx, rows, "scanDelayedEvents: rows.close() failed")
	var out []api.DelayedEvent
	for rows.Next() {
		e, err := scanDelayedEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *delayedEventsStatements) SelectForUser(ctx context.Context, txn *sql.Tx, userID string) ([]api.DelayedEvent, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectForUserStmt).QueryContext(ctx, userID)
	if err != nil {
		return nil, err
	}
	return scanDelayedEvents(ctx, rows)
}

func (s *delayedEventsStatements) SelectAll(ctx context.Context, txn *sql.Tx) ([]api.DelayedEvent, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectAllStmt).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	return scanDelayedEvents(ctx, rows)
}

func (s *delayedEventsStatements) Delete(ctx context.Context, txn *sql.Tx, delayID string) (bool, error) {
	res, err := sqlutil.TxStmt(txn, s.deleteStmt).ExecContext(ctx, delayID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *delayedEventsStatements) Restart(ctx context.Context, txn *sql.Tx, delayID string, at int64) (bool, error) {
	res, err := sqlutil.TxStmt(txn, s.restartStmt).ExecContext(ctx, at, delayID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *delayedEventsStatements) DeleteState(ctx context.Context, txn *sql.Tx, roomID, eventType, stateKey string) ([]string, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectStateEventsStmt).QueryContext(ctx, roomID, eventType, stateKey)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			internal.CloseAndLogIfError(ctx, rows, "DeleteState: rows.close() failed")
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		internal.CloseAndLogIfError(ctx, rows, "DeleteState: rows.close() failed")
		return nil, err
	}
	internal.CloseAndLogIfError(ctx, rows, "DeleteState: rows.close() failed")
	for _, id := range ids {
		if _, err = sqlutil.TxStmt(txn, s.deleteStmt).ExecContext(ctx, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (s *delayedEventsStatements) CountForUser(ctx context.Context, txn *sql.Tx, userID string) (int, error) {
	var n int
	err := sqlutil.TxStmt(txn, s.countForUserStmt).QueryRowContext(ctx, userID).Scan(&n)
	return n, err
}
