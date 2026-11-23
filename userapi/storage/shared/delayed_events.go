// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"

	"github.com/element-hq/dendrite/userapi/api"
)

func (d *Database) InsertDelayedEvent(ctx context.Context, e api.DelayedEvent) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error { return d.DelayedEvents.Insert(ctx, txn, e) })
}

func (d *Database) DelayedEvent(ctx context.Context, delayID string) (*api.DelayedEvent, error) {
	return d.DelayedEvents.Select(ctx, nil, delayID)
}

func (d *Database) DelayedEventsForUser(ctx context.Context, userID string) ([]api.DelayedEvent, error) {
	return d.DelayedEvents.SelectForUser(ctx, nil, userID)
}

func (d *Database) AllDelayedEvents(ctx context.Context) ([]api.DelayedEvent, error) {
	return d.DelayedEvents.SelectAll(ctx, nil)
}

func (d *Database) DeleteDelayedEvent(ctx context.Context, delayID string) (found bool, err error) {
	err = d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		found, err = d.DelayedEvents.Delete(ctx, txn, delayID)
		return err
	})
	return
}

func (d *Database) RestartDelayedEvent(ctx context.Context, delayID string, at int64) (found bool, err error) {
	err = d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		found, err = d.DelayedEvents.Restart(ctx, txn, delayID, at)
		return err
	})
	return
}

func (d *Database) DeleteDelayedStateEvents(ctx context.Context, roomID, eventType, stateKey string) (ids []string, err error) {
	err = d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		ids, err = d.DelayedEvents.DeleteState(ctx, txn, roomID, eventType, stateKey)
		return err
	})
	return
}

func (d *Database) CountDelayedEvents(ctx context.Context, userID string) (int, error) {
	return d.DelayedEvents.CountForUser(ctx, nil, userID)
}
