// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/userapi/api"
)

const (
	defaultAccountListLimit = 100
	maxAccountListLimit     = 1000
)

var accountOrderColumns = map[string]string{
	"":            "a.localpart || ':' || a.server_name",
	"name":        "a.localpart || ':' || a.server_name",
	"creation_ts": "a.created_ts",
	"displayname": "LOWER(COALESCE(p.display_name, ''))",
	"admin":       "(a.account_type = 3)",
	"guest":       "(a.account_type = 2)",
	"deactivated": "COALESCE(a.is_deactivated, FALSE)",
}

// escapeLike makes a string literal for a LIKE pattern with ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(s))
}

// AdminListAccounts lists accounts with their profiles, filtered and ordered as asked. It also returns how many
// accounts match, without the page.
func (d *Database) AdminListAccounts(ctx context.Context, f api.AccountListFilter) ([]api.AccountListEntry, int, error) {
	order, ok := accountOrderColumns[f.OrderBy]
	if !ok {
		return nil, 0, fmt.Errorf("unknown order %q", f.OrderBy)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAccountListLimit
	}
	limit = min(limit, maxAccountListLimit)

	const from = " FROM userapi_accounts a LEFT JOIN userapi_profiles p ON p.localpart = a.localpart AND p.server_name = a.server_name LEFT JOIN userapi_account_restrictions r ON r.localpart = a.localpart AND r.server_name = a.server_name"
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Name != "" {
		pattern := arg("%" + escapeLike(f.Name) + "%")
		where = append(where, fmt.Sprintf(`(LOWER('@' || a.localpart || ':' || a.server_name) LIKE %s ESCAPE '\' OR LOWER(COALESCE(p.display_name, '')) LIKE %s ESCAPE '\')`, pattern, pattern))
	}
	if f.UserID != "" {
		where = append(where, fmt.Sprintf(`LOWER('@' || a.localpart || ':' || a.server_name) LIKE %s ESCAPE '\'`, arg("%"+escapeLike(f.UserID)+"%")))
	}
	if !f.Guests {
		where = append(where, "a.account_type <> 2")
	}
	if !f.Deactivated {
		where = append(where, "COALESCE(a.is_deactivated, FALSE) = FALSE")
	}
	condition := ""
	if len(where) > 0 {
		condition = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := d.DB.QueryRowContext(ctx, "SELECT COUNT(*)"+from+condition, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	direction := "ASC"
	if f.Backwards {
		direction = "DESC"
	}
	query := "SELECT a.localpart, a.server_name, a.created_ts, COALESCE(a.appservice_id, ''), a.account_type," +
		" COALESCE(a.is_deactivated, FALSE), COALESCE(p.display_name, ''), COALESCE(p.avatar_url, ''), COALESCE(r.shadow_banned, FALSE)" +
		from + condition +
		fmt.Sprintf(" ORDER BY %s %s, a.localpart ASC, a.server_name ASC LIMIT %s OFFSET %s", order, direction, arg(limit), arg(max(f.From, 0)))
	rows, err := d.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "AdminListAccounts: rows.close() failed")
	var out []api.AccountListEntry
	for rows.Next() {
		var e api.AccountListEntry
		var server string
		if err = rows.Scan(&e.Localpart, &server, &e.CreatedTS, &e.AppService, &e.AccountType, &e.Deactivated, &e.DisplayName, &e.AvatarURL, &e.ShadowBanned); err != nil {
			return nil, 0, err
		}
		e.ServerName = spec.ServerName(server)
		e.UserID = fmt.Sprintf("@%s:%s", e.Localpart, server)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// SetAccountType changes the type of an account, to make it an admin or a plain user.
func (d *Database) SetAccountType(ctx context.Context, localpart string, serverName spec.ServerName, accountType api.AccountType) error {
	return d.Writer.Do(d.DB, nil, func(_ *sql.Tx) error {
		return d.Accounts.UpdateAccountType(ctx, localpart, serverName, accountType)
	})
}

// AccountRestrictions returns what an administrator has set for the account.
func (d *Database) AccountRestrictions(ctx context.Context, localpart string, serverName spec.ServerName) (api.AccountRestrictions, error) {
	return d.Restrictions.SelectRestrictions(ctx, nil, localpart, serverName)
}

// SetShadowBanned turns the shadow ban of an account on or off.
func (d *Database) SetShadowBanned(ctx context.Context, localpart string, serverName spec.ServerName, banned bool) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		return d.Restrictions.SetShadowBanned(ctx, txn, localpart, serverName, banned)
	})
}

// SetRateLimitOverride sets the rate limit of an account, nil removes it.
func (d *Database) SetRateLimitOverride(ctx context.Context, localpart string, serverName spec.ServerName, limit *api.RateLimitOverride) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		return d.Restrictions.SetRateLimit(ctx, txn, localpart, serverName, limit)
	})
}

// SetAccountExpiry sets the time (milliseconds) at which an account expires, 0 for never.
func (d *Database) SetAccountExpiry(ctx context.Context, localpart string, serverName spec.ServerName, expiresTS int64) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		return d.Restrictions.SetExpiry(ctx, txn, localpart, serverName, expiresTS)
	})
}
