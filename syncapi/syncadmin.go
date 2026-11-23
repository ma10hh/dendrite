// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/element-hq/dendrite/internal/fulltext"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/syncapi/retention"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// syncAdmin answers what the admin API of the client API asks of the timeline.
type syncAdmin struct {
	db         storage.Database
	rsAPI      api.SyncRoomserverAPI
	fts        *fulltext.Search
	serverName spec.ServerName
}

var _ api.SyncAdmin = (*syncAdmin)(nil)

// likeEscaper makes a string literal for a LIKE pattern with the backslash as escape.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *syncAdmin) PurgeHistory(ctx context.Context, roomID string, beforeTS int64, deleteLocal bool) (int, error) {
	keep := ""
	if !deleteLocal {
		keep = "%:" + likeEscaper.Replace(string(s.serverName))
	}
	return retention.PurgeRoomBefore(ctx, s.db, s.rsAPI, s.fts, roomID, beforeTS, keep)
}

func (s *syncAdmin) EventTimestamp(ctx context.Context, roomID, eventID string) (int64, error) {
	snapshot, err := s.db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	defer snapshot.Rollback() // nolint: errcheck
	events, err := snapshot.Events(ctx, []string{eventID})
	if err != nil {
		return 0, err
	}
	for _, ev := range events {
		if ev.RoomID().String() == roomID {
			return int64(ev.OriginServerTS()), nil
		}
	}
	return 0, api.ErrSyncAdminNotFound
}

func (s *syncAdmin) EventsBySender(ctx context.Context, userID string, afterPosition int64, limit int) ([]api.SyncAdminEvent, error) {
	events, err := s.db.EventsBySender(ctx, userID, afterPosition, limit)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out := make([]api.SyncAdminEvent, len(events))
	for i, e := range events {
		out[i] = api.SyncAdminEvent{Position: e.Position, EventID: e.EventID, RoomID: e.RoomID}
	}
	return out, nil
}
