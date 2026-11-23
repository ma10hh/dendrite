// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package deltas

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/element-hq/dendrite/syncapi/storage/eventmeta"
)

// UpEventTimestamp adds the time an event was sent at and whether it is a state event, which the deletion of old
// messages and the look-up of an event by time go by, and fills them for the events that are stored.
func UpEventTimestamp(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		ALTER TABLE syncapi_output_room_events ADD COLUMN IF NOT EXISTS origin_server_ts BIGINT NOT NULL DEFAULT 0;
		ALTER TABLE syncapi_output_room_events ADD COLUMN IF NOT EXISTS is_state BOOL NOT NULL DEFAULT FALSE;
	`)
	if err != nil {
		return fmt.Errorf("failed to execute upgrade: %w", err)
	}
	if err = eventmeta.Backfill(ctx, tx); err != nil {
		return fmt.Errorf("failed to fill the new columns: %w", err)
	}
	_, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS syncapi_output_room_events_room_ts_idx ON syncapi_output_room_events (room_id, origin_server_ts);`)
	if err != nil {
		return fmt.Errorf("failed to create the index: %w", err)
	}
	return nil
}
