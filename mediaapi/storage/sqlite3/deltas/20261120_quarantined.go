package deltas

import (
	"context"
	"database/sql"
	"fmt"
)

// UpQuarantined adds the flag that keeps media from being served.
func UpQuarantined(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `ALTER TABLE mediaapi_media_repository ADD COLUMN quarantined BOOLEAN NOT NULL DEFAULT FALSE;`)
	if err != nil {
		return fmt.Errorf("failed to execute upgrade: %w", err)
	}
	return nil
}
