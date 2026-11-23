package eventmeta_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/storage/eventmeta"
	"github.com/element-hq/dendrite/test"
)

func TestBackfill(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		opts := &config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
		cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
		db, err := storage.NewSyncServerDatasource(ctx, cm, opts)
		if err != nil {
			t.Fatal(err)
		}

		alice := test.NewUser(t)
		room := test.NewRoom(t, alice)
		sent := time.UnixMilli(1700000000123)
		msg := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "x"}, test.WithTimestamp(sent))
		for _, ev := range room.Events() {
			var add []string
			if ev.StateKey() != nil {
				ev.StateKeyResolved = ev.StateKey()
				add = append(add, ev.EventID())
			}
			if _, err = db.WriteEvent(ctx, ev, nil, add, nil, nil, false, gomatrixserverlib.HistoryVisibilityShared); err != nil {
				t.Fatal(err)
			}
		}

		// Make the rows look like they were written before the columns existed.
		raw, writer, err := cm.Connection(opts)
		if err != nil {
			t.Fatal(err)
		}
		_ = writer
		if _, err = raw.ExecContext(ctx, "UPDATE syncapi_output_room_events SET origin_server_ts = 0, is_state = FALSE"); err != nil {
			t.Fatal(err)
		}
		if err = sqlutil.WithTransaction(raw, func(tx *sql.Tx) error { return eventmeta.Backfill(ctx, tx) }); err != nil {
			t.Fatal(err)
		}

		var ts int64
		var isState bool
		err = raw.QueryRowContext(ctx, "SELECT origin_server_ts, is_state FROM syncapi_output_room_events WHERE event_id = $1", msg.EventID()).Scan(&ts, &isState)
		assert.NoError(t, err)
		assert.Equal(t, sent.UnixMilli(), ts)
		assert.False(t, isState)

		var states, zero int
		assert.NoError(t, raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM syncapi_output_room_events WHERE is_state").Scan(&states))
		assert.NoError(t, raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM syncapi_output_room_events WHERE origin_server_ts = 0").Scan(&zero))
		assert.Greater(t, states, 0, "the state events of the room are marked")
		assert.Equal(t, 0, zero)
	})
}
