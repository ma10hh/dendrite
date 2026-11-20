// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"

	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/mediaapi/types"
)

// TouchMedia records that the media was downloaded at the time.
func (d *Database) TouchMedia(ctx context.Context, mediaID types.MediaID, origin spec.ServerName, at spec.Timestamp) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		_, err := txn.ExecContext(ctx, "UPDATE mediaapi_media_repository SET last_access_ts = $1 WHERE media_id = $2 AND media_origin = $3", int64(at), mediaID, origin)
		return err
	})
}

// UserMediaBytes is the size of all the media the user has stored.
func (d *Database) UserMediaBytes(ctx context.Context, userID string) (int64, error) {
	var total int64
	err := d.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(file_size_bytes), 0) FROM mediaapi_media_repository WHERE user_id = $1", userID).Scan(&total)
	return total, err
}

// UnusedMedia returns up to limit media, oldest first, that was last used before the time and is not quarantined: of
// this server if local, else of the others. Quarantined media stays so that a copy of it cannot be fetched again.
func (d *Database) UnusedMedia(ctx context.Context, local bool, server spec.ServerName, before spec.Timestamp, limit int) ([]types.MediaMetadata, error) {
	comparison := "<>"
	if local {
		comparison = "="
	}
	rows, err := d.DB.QueryContext(ctx,
		"SELECT "+mediaColumns+" FROM mediaapi_media_repository WHERE media_origin "+comparison+" $1 AND "+lastUsed+" < $2"+
			" AND quarantined = $3 ORDER BY "+lastUsed+" ASC LIMIT $4",
		server, int64(before), false, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "UnusedMedia: rows.close() failed")
	return scanMedia(rows)
}
