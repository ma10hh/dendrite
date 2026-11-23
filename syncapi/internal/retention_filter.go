// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"

	"github.com/element-hq/dendrite/internal/retention"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
)

// retentionConfig is the message retention setup of the server, which the filter needs and its callers do not have.
// It is set once when the sync API starts. Nil, or a configuration that is not enabled, hides nothing.
var retentionConfig atomic.Pointer[config.MessageRetention]

// SetRetention tells the history filter which messages are past their lifetime.
func SetRetention(cfg *config.MessageRetention) {
	retentionConfig.Store(cfg)
}

type stateReader interface {
	GetStateEvent(ctx context.Context, roomID, evType, stateKey string) (*types.HeaderedEvent, error)
}

// hideExpiredEvents drops the events that are older than the retention policy of their room allows, without waiting
// for the deletion to run. State events are never hidden. All events are from the same room.
func hideExpiredEvents(ctx context.Context, state stateReader, events []*types.HeaderedEvent, now time.Time) ([]*types.HeaderedEvent, error) {
	cfg := retentionConfig.Load()
	if cfg == nil || !cfg.Enabled || len(events) == 0 {
		return events, nil
	}
	var room *retention.Policy
	ev, err := state.GetStateEvent(ctx, events[0].RoomID().String(), retention.EventType, "")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if ev != nil {
		p := retention.FromContent(ev.Content())
		room = &p
	}
	max := retention.Effective(cfg, room).MaxLifetime
	if max <= 0 {
		return events, nil
	}
	before := now.Add(-max).UnixMilli()
	kept := make([]*types.HeaderedEvent, 0, len(events))
	for _, e := range events {
		if e.StateKey() == nil {
			if ts := int64(e.OriginServerTS()); ts > 0 && ts < before {
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept, nil
}
