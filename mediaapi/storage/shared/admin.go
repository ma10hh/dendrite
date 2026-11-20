// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/mediaapi/types"
)

// SetMediaQuarantined sets whether the media is kept from being served, and says if there was such media.
func (d *Database) SetMediaQuarantined(ctx context.Context, mediaID types.MediaID, origin spec.ServerName, quarantined bool) (bool, error) {
	var affected int64
	err := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		res, err := txn.ExecContext(ctx, "UPDATE mediaapi_media_repository SET quarantined = $1 WHERE media_id = $2 AND media_origin = $3", quarantined, mediaID, origin)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected > 0, err
}

// SetUserMediaQuarantined does the same for all the media a user uploaded, and returns how many that is.
func (d *Database) SetUserMediaQuarantined(ctx context.Context, userID string, quarantined bool) (int64, error) {
	var affected int64
	err := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		res, err := txn.ExecContext(ctx, "UPDATE mediaapi_media_repository SET quarantined = $1 WHERE user_id = $2", quarantined, userID)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

var mediaOrderColumns = map[string]string{
	"":                     "creation_ts",
	"media_id":             "media_id",
	"upload_name":          "upload_name",
	"created_ts":           "creation_ts",
	"last_access_ts":       "creation_ts",
	"media_length":         "file_size_bytes",
	"media_type":           "content_type",
	"quarantined_by":       "quarantined",
	"safe_from_quarantine": "creation_ts",
}

// ListUserMedia lists the media a user uploaded, with how much there is of it in all.
func (d *Database) ListUserMedia(
	ctx context.Context, userID string, from, limit int, orderBy string, backwards bool,
) ([]types.MediaMetadata, int, error) {
	column, ok := mediaOrderColumns[orderBy]
	if !ok {
		return nil, 0, fmt.Errorf("unknown order %q", orderBy)
	}
	var total int
	if err := d.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM mediaapi_media_repository WHERE user_id = $1", userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	direction := "ASC"
	if backwards {
		direction = "DESC"
	}
	rows, err := d.DB.QueryContext(ctx,
		"SELECT media_id, media_origin, content_type, file_size_bytes, creation_ts, upload_name, base64hash, user_id, quarantined"+
			" FROM mediaapi_media_repository WHERE user_id = $1 ORDER BY "+column+" "+direction+", media_id ASC LIMIT $2 OFFSET $3",
		userID, limit, from)
	if err != nil {
		return nil, 0, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "ListUserMedia: rows.close() failed")
	out, err := scanMedia(rows)
	return out, total, err
}

// MediaToDelete returns the media of this server that is older than the timestamp (in ms) and larger than the size,
// if they are not zero.
func (d *Database) MediaToDelete(ctx context.Context, origin spec.ServerName, beforeTS spec.Timestamp, largerThan int64) ([]types.MediaMetadata, error) {
	query := "SELECT media_id, media_origin, content_type, file_size_bytes, creation_ts, upload_name, base64hash, user_id, quarantined" +
		" FROM mediaapi_media_repository WHERE media_origin = $1 AND creation_ts < $2"
	args := []any{origin, int64(beforeTS)}
	if largerThan > 0 {
		query += " AND file_size_bytes > $3"
		args = append(args, largerThan)
	}
	rows, err := d.DB.QueryContext(ctx, query+" ORDER BY creation_ts ASC", args...)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "MediaToDelete: rows.close() failed")
	return scanMedia(rows)
}

func scanMedia(rows *sql.Rows) ([]types.MediaMetadata, error) {
	var out []types.MediaMetadata
	for rows.Next() {
		var m types.MediaMetadata
		if err := rows.Scan(&m.MediaID, &m.Origin, &m.ContentType, &m.FileSizeBytes, &m.CreationTimestamp, &m.UploadName, &m.Base64Hash, &m.UserID, &m.Quarantined); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMedia removes the metadata of media and of its thumbnails, and says whether the file the media was made of is
// still used by other media: if it is not, the caller removes the file.
func (d *Database) DeleteMedia(ctx context.Context, mediaID types.MediaID, origin spec.ServerName) (meta *types.MediaMetadata, fileInUse bool, err error) {
	meta, err = d.GetMediaMetadata(ctx, mediaID, origin)
	if err != nil || meta == nil {
		return nil, false, err
	}
	err = d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		if _, derr := txn.ExecContext(ctx, "DELETE FROM mediaapi_thumbnail WHERE media_id = $1 AND media_origin = $2", mediaID, origin); derr != nil {
			return derr
		}
		if _, derr := txn.ExecContext(ctx, "DELETE FROM mediaapi_media_repository WHERE media_id = $1 AND media_origin = $2", mediaID, origin); derr != nil {
			return derr
		}
		var others int
		if derr := txn.QueryRowContext(ctx, "SELECT COUNT(*) FROM mediaapi_media_repository WHERE base64hash = $1 AND media_origin = $2", meta.Base64Hash, origin).Scan(&others); derr != nil {
			return derr
		}
		fileInUse = others > 0
		return nil
	})
	return meta, fileInUse, err
}
