// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/syncapi/types"
)

// InClause returns the condition for a column to be one of the values, using the parameters from the first one, and
// the values to bind to them. It differs between the databases.
type InClause func(column string, first int, values []string) (string, []any)

// SelectRelationBundles reads, for each of the events, the summary of its thread, the events that may be valid
// replacements, and the events that reference it. The user is the one the summary is for. With withThreadChildren
// it also returns the replies of each thread, for when some senders have to be left out.
func SelectRelationBundles(
	ctx context.Context, db *sql.DB, txn *sql.Tx, in InClause,
	roomID string, eventIDs []string, userID string, withThreadChildren bool,
) (*types.RelationBundles, error) {
	out := &types.RelationBundles{
		Threads:        map[string]types.ThreadEntry{},
		ThreadChildren: map[string][]types.RelationChild{},
		Edits:          map[string][]string{},
		References:     map[string][]types.RelationChild{},
	}
	if len(eventIDs) == 0 {
		return out, nil
	}
	query := func(q string, args []any) (*sql.Rows, error) {
		if txn != nil {
			return txn.QueryContext(ctx, q, args...)
		}
		return db.QueryContext(ctx, q, args...)
	}

	// Threads: the number of replies, the latest, and whether the user took part.
	cond, ids := in("event_id", 3, eventIDs)
	rows, err := query(
		"SELECT t.event_id, t.latest, t.cnt,"+
			" (SELECT l.child_event_id FROM syncapi_relations l WHERE l.id = t.latest),"+
			" (EXISTS (SELECT 1 FROM syncapi_output_room_events r WHERE r.event_id = t.event_id AND r.sender = $1)"+
			" OR EXISTS (SELECT 1 FROM syncapi_relations c JOIN syncapi_output_room_events ce ON ce.event_id = c.child_event_id"+
			" WHERE c.room_id = $2 AND c.event_id = t.event_id AND c.rel_type = 'm.thread' AND ce.sender = $1))"+
			" FROM (SELECT event_id, MAX(id) AS latest, COUNT(*) AS cnt FROM syncapi_relations"+
			" WHERE room_id = $2 AND rel_type = 'm.thread' AND "+cond+" GROUP BY event_id) t",
		append([]any{userID, roomID}, ids...),
	)
	if err != nil {
		return nil, fmt.Errorf("threads: %w", err)
	}
	for rows.Next() {
		var t types.ThreadEntry
		var latest sql.NullString
		if err = rows.Scan(&t.RootEventID, &t.LatestRelationID, &t.Count, &latest, &t.Participated); err != nil {
			internal.CloseAndLogIfError(ctx, rows, "SelectRelationBundles: rows.close() failed")
			return nil, err
		}
		t.LatestEventID = latest.String
		out.Threads[t.RootEventID] = t
	}
	internal.CloseAndLogIfError(ctx, rows, "SelectRelationBundles: rows.close() failed")
	if err = rows.Err(); err != nil {
		return nil, err
	}

	// Edits by the sender of the event, of the same type. What else makes them valid is left to the caller.
	cond, ids = in("r.event_id", 2, eventIDs)
	rows, err = query(
		"SELECT r.event_id, r.child_event_id FROM syncapi_relations r"+
			" JOIN syncapi_output_room_events ce ON ce.event_id = r.child_event_id"+
			" JOIN syncapi_output_room_events te ON te.event_id = r.event_id"+
			" WHERE r.rel_type = 'm.replace' AND r.room_id = $1 AND ce.sender = te.sender AND ce.type = te.type AND "+cond+
			" ORDER BY r.id",
		append([]any{roomID}, ids...),
	)
	if err != nil {
		return nil, fmt.Errorf("edits: %w", err)
	}
	for rows.Next() {
		var target, child string
		if err = rows.Scan(&target, &child); err != nil {
			internal.CloseAndLogIfError(ctx, rows, "SelectRelationBundles: rows.close() failed")
			return nil, err
		}
		out.Edits[target] = append(out.Edits[target], child)
	}
	internal.CloseAndLogIfError(ctx, rows, "SelectRelationBundles: rows.close() failed")
	if err = rows.Err(); err != nil {
		return nil, err
	}

	children := func(relType string, into map[string][]types.RelationChild) error {
		cond, ids := in("r.event_id", 3, eventIDs)
		rows, err := query(
			"SELECT r.event_id, r.child_event_id, ce.sender FROM syncapi_relations r"+
				" JOIN syncapi_output_room_events ce ON ce.event_id = r.child_event_id"+
				" WHERE r.rel_type = $1 AND r.room_id = $2 AND "+cond+" ORDER BY r.id",
			append([]any{relType, roomID}, ids...),
		)
		if err != nil {
			return fmt.Errorf("%s: %w", relType, err)
		}
		defer internal.CloseAndLogIfError(ctx, rows, "SelectRelationBundles: rows.close() failed")
		for rows.Next() {
			var target string
			var child types.RelationChild
			if err = rows.Scan(&target, &child.EventID, &child.Sender); err != nil {
				return err
			}
			into[target] = append(into[target], child)
		}
		return rows.Err()
	}
	if err = children("m.reference", out.References); err != nil {
		return nil, err
	}
	if withThreadChildren && len(out.Threads) > 0 {
		if err = children("m.thread", out.ThreadChildren); err != nil {
			return nil, err
		}
	}
	return out, nil
}
