// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package eventmeta fills the columns of syncapi_output_room_events that were added after the table existed
// (origin_server_ts and is_state) for the events that were stored before.
package eventmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

const batchSize = 1000

type meta struct {
	id    int64
	ts    int64
	state bool
}

// Backfill reads the stored events that have no timestamp yet and writes their origin_server_ts and is_state.
// It runs inside the migration transaction. Events whose JSON has no timestamp keep 0, which retention skips.
func Backfill(ctx context.Context, tx *sql.Tx) error {
	var after int64
	for {
		batch, last, err := readBatch(ctx, tx, after)
		if err != nil {
			return err
		}
		if last == 0 {
			return nil
		}
		after = last
		for _, m := range batch {
			if _, err = tx.ExecContext(ctx,
				"UPDATE syncapi_output_room_events SET origin_server_ts = $1, is_state = $2 WHERE id = $3",
				m.ts, m.state, m.id,
			); err != nil {
				return fmt.Errorf("failed to update event: %w", err)
			}
		}
	}
}

// readBatch returns the events after the position that can be read, and the position of the last row it looked at
// (0 if there was none).
func readBatch(ctx context.Context, tx *sql.Tx, after int64) ([]meta, int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT id, headered_event_json FROM syncapi_output_room_events WHERE id > $1 AND origin_server_ts = 0 ORDER BY id ASC LIMIT $2",
		after, batchSize,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to select events: %w", err)
	}
	defer rows.Close() // nolint: errcheck
	var batch []meta
	var last int64
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, 0, fmt.Errorf("failed to scan event: %w", err)
		}
		last = id
		var ev struct {
			TS       int64   `json:"origin_server_ts"`
			StateKey *string `json:"state_key"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		batch = append(batch, meta{id: id, ts: ev.TS, state: ev.StateKey != nil})
	}
	return batch, last, rows.Err()
}
