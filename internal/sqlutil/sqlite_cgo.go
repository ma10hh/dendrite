//go:build cgo
// +build cgo

package sqlutil

import (
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const SQLITE_DRIVER_NAME = "sqlite3"

func sqliteDSNExtension(dsn string) string {
	// Wait as long as the driver without cgo does before erroring because the database is locked: with several
	// requests that write at the same time, the default of 5 seconds is not always enough.
	if strings.Contains(dsn, "_busy_timeout") || strings.Contains(dsn, "_timeout=") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	return dsn + "_busy_timeout=10000"
}
