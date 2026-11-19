// Copyright 2024 New Vector Ltd.
// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/syncapi/storage/tables"
	"github.com/element-hq/dendrite/syncapi/types"
)

const relationsSchema = `
CREATE SEQUENCE IF NOT EXISTS syncapi_relation_id;

CREATE TABLE IF NOT EXISTS syncapi_relations (
	id BIGINT PRIMARY KEY DEFAULT nextval('syncapi_relation_id'),
	room_id TEXT NOT NULL,
	event_id TEXT NOT NULL,
	child_event_id TEXT NOT NULL,
	child_event_type TEXT NOT NULL,
	rel_type TEXT NOT NULL,
	CONSTRAINT syncapi_relations_unique UNIQUE (room_id, event_id, child_event_id, rel_type)
);
`

const insertRelationSQL = "" +
	"INSERT INTO syncapi_relations (" +
	"  room_id, event_id, child_event_id, child_event_type, rel_type" +
	") VALUES ($1, $2, $3, $4, $5) " +
	" ON CONFLICT DO NOTHING"

const deleteRelationSQL = "" +
	"DELETE FROM syncapi_relations WHERE room_id = $1 AND child_event_id = $2"

const selectRelationsInRangeAscSQL = "" +
	"SELECT id, child_event_id, rel_type FROM syncapi_relations" +
	" WHERE room_id = $1 AND event_id = $2" +
	" AND ( $3 = '' OR rel_type = $3 )" +
	" AND ( $4 = '' OR child_event_type = $4 )" +
	" AND id > $5 AND id <= $6" +
	" ORDER BY id ASC LIMIT $7"

const selectRelationsInRangeDescSQL = "" +
	"SELECT id, child_event_id, rel_type FROM syncapi_relations" +
	" WHERE room_id = $1 AND event_id = $2" +
	" AND ( $3 = '' OR rel_type = $3 )" +
	" AND ( $4 = '' OR child_event_type = $4 )" +
	" AND id >= $5 AND id < $6" +
	" ORDER BY id DESC LIMIT $7"

const selectMaxRelationIDSQL = "" +
	"SELECT COALESCE(MAX(id), 0) FROM syncapi_relations"

type relationsStatements struct {
	insertRelationStmt             *sql.Stmt
	selectRelationsInRangeAscStmt  *sql.Stmt
	selectRelationsInRangeDescStmt *sql.Stmt
	deleteRelationStmt             *sql.Stmt
	selectMaxRelationIDStmt        *sql.Stmt
	selectThreadsStmt              *sql.Stmt
	selectParticipatedThreadsStmt  *sql.Stmt
}

func NewPostgresRelationsTable(db *sql.DB) (tables.Relations, error) {
	s := &relationsStatements{}
	_, err := db.Exec(relationsSchema)
	if err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.insertRelationStmt, insertRelationSQL},
		{&s.selectRelationsInRangeAscStmt, selectRelationsInRangeAscSQL},
		{&s.selectRelationsInRangeDescStmt, selectRelationsInRangeDescSQL},
		{&s.deleteRelationStmt, deleteRelationSQL},
		{&s.selectMaxRelationIDStmt, selectMaxRelationIDSQL},
		{&s.selectThreadsStmt, selectThreadsSQL},
		{&s.selectParticipatedThreadsStmt, selectParticipatedThreadsSQL},
	}.Prepare(db)
}

func (s *relationsStatements) InsertRelation(
	ctx context.Context, txn *sql.Tx, roomID, eventID, childEventID, childEventType, relType string,
) (err error) {
	_, err = sqlutil.TxStmt(txn, s.insertRelationStmt).ExecContext(
		ctx, roomID, eventID, childEventID, childEventType, relType,
	)
	return
}

func (s *relationsStatements) DeleteRelation(
	ctx context.Context, txn *sql.Tx, roomID, childEventID string,
) error {
	stmt := sqlutil.TxStmt(txn, s.deleteRelationStmt)
	_, err := stmt.ExecContext(
		ctx, roomID, childEventID,
	)
	return err
}

// SelectRelationsInRange returns a map rel_type -> []child_event_id
func (s *relationsStatements) SelectRelationsInRange(
	ctx context.Context, txn *sql.Tx, roomID, eventID, relType, eventType string,
	r types.Range, limit int,
) (map[string][]types.RelationEntry, types.StreamPosition, error) {
	var lastPos types.StreamPosition
	var stmt *sql.Stmt
	if r.Backwards {
		stmt = sqlutil.TxStmt(txn, s.selectRelationsInRangeDescStmt)
	} else {
		stmt = sqlutil.TxStmt(txn, s.selectRelationsInRangeAscStmt)
	}
	rows, err := stmt.QueryContext(ctx, roomID, eventID, relType, eventType, r.Low(), r.High(), limit)
	if err != nil {
		return nil, lastPos, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "selectRelationsInRange: rows.close() failed")
	result := map[string][]types.RelationEntry{}
	var (
		id           types.StreamPosition
		childEventID string
		relationType string
	)
	for rows.Next() {
		if err = rows.Scan(&id, &childEventID, &relationType); err != nil {
			return nil, lastPos, err
		}
		if id > lastPos {
			lastPos = id
		}
		result[relationType] = append(result[relationType], types.RelationEntry{
			Position: id,
			EventID:  childEventID,
		})
	}
	if lastPos == 0 {
		lastPos = r.To
	}
	return result, lastPos, rows.Err()
}

func (s *relationsStatements) SelectMaxRelationID(
	ctx context.Context, txn *sql.Tx,
) (id int64, err error) {
	stmt := sqlutil.TxStmt(txn, s.selectMaxRelationIDStmt)
	err = stmt.QueryRowContext(ctx).Scan(&id)
	return
}

// The thread queries read the events table, so it has to exist before the statements of this table are prepared.
// Threads are the events with m.thread relations. The user took part in a thread if they sent its root or one of its replies.
const threadParticipationSQL = "" +
	"(EXISTS (SELECT 1 FROM syncapi_output_room_events r WHERE r.event_id = t.event_id AND r.sender = $2)" +
	" OR EXISTS (SELECT 1 FROM syncapi_relations c JOIN syncapi_output_room_events ce ON ce.event_id = c.child_event_id" +
	" WHERE c.room_id = $1 AND c.event_id = t.event_id AND c.rel_type = 'm.thread' AND ce.sender = $2))"

const selectThreadsSQL = "" +
	"SELECT t.event_id, t.latest, t.cnt," +
	" (SELECT l.child_event_id FROM syncapi_relations l WHERE l.id = t.latest)," +
	" " + threadParticipationSQL +
	" FROM (SELECT event_id, MAX(id) AS latest, COUNT(*) AS cnt FROM syncapi_relations" +
	" WHERE room_id = $1 AND rel_type = 'm.thread' GROUP BY event_id HAVING MAX(id) < $3" +
	" ORDER BY MAX(id) DESC LIMIT $4) t" +
	" ORDER BY t.latest DESC"

const selectParticipatedThreadsSQL = "" +
	"SELECT t.event_id, t.latest, t.cnt," +
	" (SELECT l.child_event_id FROM syncapi_relations l WHERE l.id = t.latest)," +
	" " + threadParticipationSQL +
	" FROM (SELECT event_id, MAX(id) AS latest, COUNT(*) AS cnt FROM syncapi_relations" +
	" WHERE room_id = $1 AND rel_type = 'm.thread' GROUP BY event_id HAVING MAX(id) < $3" +
	" AND (EXISTS (SELECT 1 FROM syncapi_output_room_events r WHERE r.event_id = syncapi_relations.event_id AND r.sender = $2)" +
	" OR EXISTS (SELECT 1 FROM syncapi_relations c JOIN syncapi_output_room_events ce ON ce.event_id = c.child_event_id" +
	" WHERE c.room_id = $1 AND c.event_id = syncapi_relations.event_id AND c.rel_type = 'm.thread' AND ce.sender = $2))" +
	" ORDER BY MAX(id) DESC LIMIT $4) t" +
	" ORDER BY t.latest DESC"

func (s *relationsStatements) SelectThreads(
	ctx context.Context, txn *sql.Tx, roomID, userID string, onlyParticipated bool, before int64, limit int,
) ([]types.ThreadEntry, error) {
	stmt := s.selectThreadsStmt
	if onlyParticipated {
		stmt = s.selectParticipatedThreadsStmt
	}
	rows, err := sqlutil.TxStmt(txn, stmt).QueryContext(ctx, roomID, userID, before, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectThreads: rows.close() failed")
	var threads []types.ThreadEntry
	for rows.Next() {
		var t types.ThreadEntry
		var latestEvent sql.NullString
		if err = rows.Scan(&t.RootEventID, &t.LatestRelationID, &t.Count, &latestEvent, &t.Participated); err != nil {
			return nil, err
		}
		t.LatestEventID = latestEvent.String
		threads = append(threads, t)
	}
	return threads, rows.Err()
}
