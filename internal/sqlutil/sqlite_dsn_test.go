package sqlutil

import "testing"

func TestSQLiteDSNExtension(t *testing.T) {
	got := sqliteDSNExtension("file:/tmp/x.db")
	if got == "file:/tmp/x.db" {
		t.Errorf("no busy timeout was added: %q", got)
	}
	if again := sqliteDSNExtension(got); again != got {
		t.Errorf("a DSN that has a timeout is changed: %q -> %q", got, again)
	}
}
