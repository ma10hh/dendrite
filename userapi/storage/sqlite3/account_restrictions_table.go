// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"

	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/element-hq/dendrite/userapi/storage/tables"
)

const accountRestrictionsSchema = `
-- What an administrator has set for an account that the account does not have by default.
CREATE TABLE IF NOT EXISTS userapi_account_restrictions (
	localpart TEXT NOT NULL,
	server_name TEXT NOT NULL,
	-- The events and invites of the user are dropped, without telling the user.
	shadow_banned BOOLEAN NOT NULL DEFAULT 0,
	-- Replaces the rate limit of the server for this user, when messages_per_second is not NULL.
	ratelimit_messages_per_second REAL,
	ratelimit_burst_count BIGINT,
	-- The time (milliseconds) at which the account stops working, 0 for never.
	expires_ts BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (localpart, server_name)
);
`

const selectAccountRestrictionsSQL = "" +
	"SELECT shadow_banned, ratelimit_messages_per_second, ratelimit_burst_count, expires_ts FROM userapi_account_restrictions WHERE localpart = $1 AND server_name = $2"

const upsertShadowBanSQL = "" +
	"INSERT INTO userapi_account_restrictions (localpart, server_name, shadow_banned) VALUES ($1, $2, $3)" +
	" ON CONFLICT (localpart, server_name) DO UPDATE SET shadow_banned = excluded.shadow_banned"

const upsertRateLimitSQL = "" +
	"INSERT INTO userapi_account_restrictions (localpart, server_name, ratelimit_messages_per_second, ratelimit_burst_count) VALUES ($1, $2, $3, $4)" +
	" ON CONFLICT (localpart, server_name) DO UPDATE SET ratelimit_messages_per_second = excluded.ratelimit_messages_per_second, ratelimit_burst_count = excluded.ratelimit_burst_count"

const upsertExpirySQL = "" +
	"INSERT INTO userapi_account_restrictions (localpart, server_name, expires_ts) VALUES ($1, $2, $3)" +
	" ON CONFLICT (localpart, server_name) DO UPDATE SET expires_ts = excluded.expires_ts"

type accountRestrictionsStatements struct {
	selectStmt       *sql.Stmt
	upsertShadowBan  *sql.Stmt
	upsertRateLimit  *sql.Stmt
	upsertExpiryStmt *sql.Stmt
}

func NewSQLiteAccountRestrictionsTable(db *sql.DB) (tables.AccountRestrictionsTable, error) {
	s := &accountRestrictionsStatements{}
	if _, err := db.Exec(accountRestrictionsSchema); err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.selectStmt, selectAccountRestrictionsSQL},
		{&s.upsertShadowBan, upsertShadowBanSQL},
		{&s.upsertRateLimit, upsertRateLimitSQL},
		{&s.upsertExpiryStmt, upsertExpirySQL},
	}.Prepare(db)
}

func (s *accountRestrictionsStatements) SelectRestrictions(ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName) (api.AccountRestrictions, error) {
	var r api.AccountRestrictions
	var perSecond sql.NullFloat64
	var burst sql.NullInt64
	err := sqlutil.TxStmt(txn, s.selectStmt).QueryRowContext(ctx, localpart, serverName).Scan(&r.ShadowBanned, &perSecond, &burst, &r.ExpiresTS)
	if err == sql.ErrNoRows {
		return api.AccountRestrictions{}, nil
	}
	if err != nil {
		return api.AccountRestrictions{}, err
	}
	if perSecond.Valid {
		r.RateLimit = &api.RateLimitOverride{MessagesPerSecond: perSecond.Float64, BurstCount: int(burst.Int64)}
	}
	return r, nil
}

func (s *accountRestrictionsStatements) SetShadowBanned(ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName, banned bool) error {
	_, err := sqlutil.TxStmt(txn, s.upsertShadowBan).ExecContext(ctx, localpart, serverName, banned)
	return err
}

func (s *accountRestrictionsStatements) SetRateLimit(ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName, limit *api.RateLimitOverride) error {
	var perSecond sql.NullFloat64
	var burst sql.NullInt64
	if limit != nil {
		perSecond = sql.NullFloat64{Float64: limit.MessagesPerSecond, Valid: true}
		burst = sql.NullInt64{Int64: int64(limit.BurstCount), Valid: true}
	}
	_, err := sqlutil.TxStmt(txn, s.upsertRateLimit).ExecContext(ctx, localpart, serverName, perSecond, burst)
	return err
}

func (s *accountRestrictionsStatements) SetExpiry(ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName, expiresTS int64) error {
	_, err := sqlutil.TxStmt(txn, s.upsertExpiryStmt).ExecContext(ctx, localpart, serverName, expiresTS)
	return err
}
