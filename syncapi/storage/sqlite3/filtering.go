package sqlite3

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/element-hq/dendrite/internal/sqlutil"
)

type FilterOrder int

const (
	FilterOrderNone = iota
	FilterOrderAsc
	FilterOrderDesc
)

// prepareWithFilters returns a prepared statement with the
// relevant filters included. It also includes an []interface{}
// list of all the relevant parameters to pass straight to
// QueryContext, QueryRowContext etc.
// We don't take the filter object directly here because the
// fields might come from either a StateFilter or an EventFilter,
// and it's easier just to have the caller extract the relevant
// parts.
func prepareWithFilters(
	db *sql.DB, txn *sql.Tx, query string, params []interface{},
	senders, notsenders, types, nottypes *[]string, excludeEventIDs []string,
	containsURL *bool, limit int, order FilterOrder,
) (*sql.Stmt, []interface{}, error) {
	offset := len(params)
	if senders != nil {
		if count := len(*senders); count > 0 {
			query += " AND sender IN " + sqlutil.QueryVariadicOffset(count, offset)
			for _, v := range *senders {
				params, offset = append(params, v), offset+1
			}
		} else {
			query += ` AND sender = ""`
		}
	}
	if notsenders != nil {
		if count := len(*notsenders); count > 0 {
			query += " AND sender NOT IN " + sqlutil.QueryVariadicOffset(count, offset)
			for _, v := range *notsenders {
				params, offset = append(params, v), offset+1
			}
		} else {
			query += ` AND sender != ""`
		}
	}
	if types != nil {
		if len(*types) > 0 {
			var clause string
			clause, params, offset = typeClause(*types, false, params, offset)
			query += " AND " + clause
		} else {
			query += ` AND type = ""`
		}
	}
	if nottypes != nil {
		if len(*nottypes) > 0 {
			var clause string
			clause, params, offset = typeClause(*nottypes, true, params, offset)
			query += " AND " + clause
		} else {
			query += ` AND type != ""`
		}
	}
	if containsURL != nil {
		query += fmt.Sprintf(" AND contains_url = %v", *containsURL)
	}
	if count := len(excludeEventIDs); count > 0 {
		query += " AND event_id NOT IN " + sqlutil.QueryVariadicOffset(count, offset)
		for _, v := range excludeEventIDs {
			params, offset = append(params, v), offset+1
		}
	}
	switch order {
	case FilterOrderAsc:
		query += " ORDER BY id ASC"
	case FilterOrderDesc:
		query += " ORDER BY id DESC"
	}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", offset+1)
		params = append(params, limit)
	}

	var stmt *sql.Stmt
	var err error
	if txn != nil {
		stmt, err = txn.Prepare(query)
	} else {
		stmt, err = db.Prepare(query)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("s.db.Prepare: %w", err)
	}
	return stmt, params, nil
}

// typeClause matches the type column against event types, any of which may end in a * that stands for any characters.
// The clause is true for a type that matches one of them, or for one that matches none of them if negate is set.
func typeClause(patterns []string, negate bool, params []interface{}, offset int) (string, []interface{}, int) {
	// GLOB, not LIKE: LIKE ignores the case of ASCII letters in SQLite, and event types are case sensitive (as they are
	// with Postgres). In a GLOB pattern * stands for any characters, and ? and [ have to be taken out of their meaning.
	var exact, like []string
	for _, p := range patterns {
		if strings.Contains(p, "*") {
			like = append(like, strings.NewReplacer("[", "[[]", "?", "[?]").Replace(p))
		} else {
			exact = append(exact, p)
		}
	}
	var parts []string
	if len(exact) > 0 {
		op := "IN"
		if negate {
			op = "NOT IN"
		}
		parts = append(parts, "type "+op+" "+sqlutil.QueryVariadicOffset(len(exact), offset))
		for _, v := range exact {
			params, offset = append(params, v), offset+1
		}
	}
	for _, v := range like {
		op := "GLOB"
		if negate {
			op = "NOT GLOB"
		}
		parts = append(parts, fmt.Sprintf(`type %s $%d`, op, offset+1))
		params, offset = append(params, v), offset+1
	}
	join := " OR "
	if negate {
		join = " AND "
	}
	return "(" + strings.Join(parts, join) + ")", params, offset
}
