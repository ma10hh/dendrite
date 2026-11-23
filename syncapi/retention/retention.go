// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package retention deletes the messages that are older than the retention policy of their room.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/internal/fulltext"
	"github.com/element-hq/dendrite/internal/retention"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/storage"
)

var lastRun = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "dendrite",
	Subsystem: "syncapi",
	Name:      "retention_last_run_timestamp_seconds",
	Help:      "When the last run of the deletion of old messages finished, as a Unix time; 0 until the first run",
})

var deletedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "dendrite",
	Subsystem: "syncapi",
	Name:      "retention_deleted_total",
	Help:      "Messages deleted because they were older than the retention policy of their room",
})

// batchSize is how many messages of a room are deleted in one step.
const batchSize = 500

// Pruner strips the content of events in the roomserver.
type Pruner interface {
	PerformAdminPruneEvents(ctx context.Context, roomID string, eventIDs []string) (int, error)
}

// Run deletes the messages that are older than the policy of their room allows, and says how many it deleted. The
// roomserver is asked first: if the process stops in between, the next run finds the messages again.
func Run(
	ctx context.Context, cfg *config.MessageRetention, db storage.Database, rs Pruner, fts *fulltext.Search, now time.Time,
) (int, error) {
	evType := retention.EventType
	if cfg.DefaultPolicy.MaxLifetime > 0 {
		// Every room may be affected: the ones that have no policy get the default.
		evType = "m.room.create"
	}
	roomIDs, err := db.RoomIDsWithStateType(ctx, evType)
	if err != nil {
		return 0, fmt.Errorf("failed to list the rooms: %w", err)
	}
	total := 0
	for _, roomID := range roomIDs {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, err := runRoom(ctx, cfg, db, rs, fts, roomID, now)
		total += n
		if err != nil {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			log.WithError(err).WithField("room_id", roomID).Warn("Deleting old messages of a room failed")
		}
	}
	lastRun.Set(float64(time.Now().Unix()))
	return total, nil
}

func roomPolicy(ctx context.Context, db storage.Database, roomID string) (*retention.Policy, error) {
	snapshot, err := db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer snapshot.Rollback() // nolint: errcheck
	ev, err := snapshot.GetStateEvent(ctx, roomID, retention.EventType, "")
	if err != nil || ev == nil {
		return nil, err
	}
	p := retention.FromContent(ev.Content())
	return &p, nil
}

func runRoom(
	ctx context.Context, cfg *config.MessageRetention, db storage.Database, rs Pruner, fts *fulltext.Search,
	roomID string, now time.Time,
) (int, error) {
	room, err := roomPolicy(ctx, db, roomID)
	if err != nil {
		return 0, err
	}
	max := retention.Effective(cfg, room).MaxLifetime
	if max <= 0 {
		return 0, nil
	}
	before := now.Add(-max).UnixMilli()
	total := 0
	for ctx.Err() == nil {
		ids, err := db.RetentionCandidates(ctx, roomID, before, batchSize)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		if _, err = rs.PerformAdminPruneEvents(ctx, roomID, ids); err != nil {
			return total, fmt.Errorf("roomserver: %w", err)
		}
		if err = db.DeleteEvents(ctx, roomID, ids); err != nil {
			return total, err
		}
		if fts != nil {
			for _, id := range ids {
				if err = fts.Delete(id); err != nil {
					log.WithError(err).WithField("event_id", id).Warn("Removing a deleted message from the search index failed")
				}
			}
		}
		total += len(ids)
		deletedTotal.Add(float64(len(ids)))
		if len(ids) < batchSize {
			return total, nil
		}
	}
	return total, ctx.Err()
}

// Start runs the deletion every interval, until the context ends. It does nothing if retention is off.
func Start(ctx context.Context, cfg *config.MessageRetention, db storage.Database, rs Pruner, fts *fulltext.Search) {
	if !cfg.Enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := Run(ctx, cfg, db, rs, fts, time.Now())
				switch {
				case err != nil && ctx.Err() == nil:
					log.WithError(err).Error("Deleting old messages failed")
				case n > 0:
					log.WithField("messages", n).Info("Deleted messages that were older than the retention policy of their room")
				}
			}
		}
	}()
}
