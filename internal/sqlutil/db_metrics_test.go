package sqlutil

import (
	"testing"

	"github.com/element-hq/dendrite/setup/config"
)

func TestDBLabelHasNoCredentials(t *testing.T) {
	for cs, want := range map[string]string{
		"postgresql://dendrite:s3cret@db.example:5432/media?sslmode=disable": "db.example:5432/media",
		"host=db.example user=dendrite password=s3cret dbname=sync":          "host=db.example dbname=sync",
		"file:/var/lib/dendrite/roomserver.db":                               "roomserver.db",
		"file:media.db?_busy_timeout=10000":                                  "media.db",
		"user=a password=b":                                                  "unknown",
	} {
		if got := dbLabel(config.DataSource(cs)); got != want {
			t.Errorf("dbLabel(%q) = %q, want %q", cs, got, want)
		}
	}
}
