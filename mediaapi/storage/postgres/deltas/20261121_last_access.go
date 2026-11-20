package deltas

import (
	"context"
	"database/sql"
	"fmt"
)

// UpLastAccess adds the time a media was last downloaded, which the removal of unused media goes by.
func UpLastAccess(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `ALTER TABLE mediaapi_media_repository ADD COLUMN IF NOT EXISTS last_access_ts BIGINT NOT NULL DEFAULT 0;`)
	if err != nil {
		return fmt.Errorf("failed to execute upgrade: %w", err)
	}
	return nil
}
