package sqlite3

import (
	"database/sql"
	"slices"
	"sort"
	"testing"
)

// The wildcard of a type filter matches the way it does with Postgres: the case of the type counts, and ? and [ are
// ordinary characters.
func TestTypeClause(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("CREATE TABLE e (type TEXT)"); err != nil {
		t.Fatal(err)
	}
	all := []string{"m.room.message", "m.room.name", "M.ROOM.name", "m.Room.x", "m.room", "m.roomx", "m.reaction", "org.[a]", "org.?", "org.b"}
	for _, ty := range all {
		if _, err = db.Exec("INSERT INTO e (type) VALUES (?)", ty); err != nil {
			t.Fatal(err)
		}
	}
	matches := func(patterns []string, negate bool) []string {
		clause, params, _ := typeClause(patterns, negate, nil, 0)
		for i := range params {
			_ = i
		}
		rows, err := db.Query("SELECT type FROM e WHERE "+clause, params...)
		if err != nil {
			t.Fatalf("%v: %v", patterns, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var ty string
			if err = rows.Scan(&ty); err != nil {
				t.Fatal(err)
			}
			out = append(out, ty)
		}
		sort.Strings(out)
		return out
	}
	for name, tc := range map[string]struct {
		patterns []string
		negate   bool
		want     []string
	}{
		"prefix, with the case of the type": {[]string{"m.room.*"}, false, []string{"m.room.message", "m.room.name"}},
		"exact and prefix":                  {[]string{"m.reaction", "m.room.n*"}, false, []string{"m.reaction", "m.room.name"}},
		"a ? and a [ are characters":        {[]string{"org.?", "org.[*"}, false, []string{"org.?", "org.[a]"}},
		"a [ is not a class":                {[]string{"org.[a]*"}, false, []string{"org.[a]"}},
		"not":                               {[]string{"m.room.*", "org.*"}, true, []string{"M.ROOM.name", "m.Room.x", "m.reaction", "m.room", "m.roomx"}},
	} {
		got := matches(tc.patterns, tc.negate)
		want := slices.Clone(tc.want)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}
