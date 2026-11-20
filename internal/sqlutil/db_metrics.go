// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlutil

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/element-hq/dendrite/setup/config"
)

// dbLabel names a database for metrics without its credentials: host and database for Postgres, the file for SQLite.
func dbLabel(cs config.DataSource) string {
	s := string(cs)
	if cs.IsSQLite() {
		s = strings.TrimPrefix(s, "file:")
		if i := strings.IndexByte(s, '?'); i >= 0 {
			s = s[:i]
		}
		return filepath.Base(s)
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host + u.Path
	}
	// A "host=... dbname=..." string: only the parts that are not secret.
	var parts []string
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "host=") || strings.HasPrefix(field, "dbname=") {
			parts = append(parts, field)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return "unknown"
}

// registerDBStats exposes the connection pool of the database: open, idle and in use connections, and how long
// requests waited for one. A database that is already registered, as happens when tests open the same one twice, is
// left as it is.
func registerDBStats(db *sql.DB, cs config.DataSource) {
	_ = prometheus.Register(collectors.NewDBStatsCollector(db, dbLabel(cs)))
}
