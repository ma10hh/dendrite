package sqlite3

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// SQLite chooses the plan of a query from the indexes alone, because Dendrite never runs ANALYZE. For the
// timeline of a room it must use the room_id index. Choosing the index on exclude_from_sync instead reads the
// events of all rooms from the newest down, which makes rooms without recent events very slow.
func TestRecentEventsQueriesUseTheRoomIndex(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(outputRoomEventsSchema); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if _, err = db.Exec(
			`INSERT INTO syncapi_output_room_events (event_id, room_id, headered_event_json, type, sender, contains_url, exclude_from_sync)
			 VALUES (?, ?, '{}', 'm.room.message', '@a:test', 0, 0)`,
			fmt.Sprintf("$e%d", i), fmt.Sprintf("!room%d:test", i%50),
		); err != nil {
			t.Fatal(err)
		}
	}

	for name, query := range map[string]string{
		"timeline":      strings.ReplaceAll(selectRecentEventsForSyncSQL, "$", "?") + " ORDER BY id DESC LIMIT 21",
		"room activity": "SELECT room_id, MAX(id) FROM syncapi_output_room_events WHERE room_id IN (?, ?) AND +exclude_from_sync = FALSE GROUP BY room_id",
	} {
		args := []any{"!room1:test", 0, 1 << 40}
		if name == "room activity" {
			args = []any{"!room1:test", "!room2:test"}
		}
		rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if strings.Contains(joined, "exclude_from_sync_idx") || !strings.Contains(joined, "room_id_idx") {
			t.Errorf("%s: plan %q must use the room_id index and not the exclude_from_sync index", name, joined)
		}
	}
}
