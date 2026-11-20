package storage_test

import (
	"context"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/test"
)

func TestMediaRetentionStorage(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateDatabase(t, dbType)
		defer close()
		ctx := context.Background()
		store := func(id string, origin spec.ServerName, hash, user string, size int64) {
			require.NoError(t, db.StoreMediaMetadata(ctx, &types.MediaMetadata{
				MediaID: types.MediaID(id), Origin: origin, ContentType: "text/plain", FileSizeBytes: types.FileSizeBytes(size),
				UploadName: "f", Base64Hash: types.Base64Hash(hash), UserID: types.MatrixUserID(user),
			}))
		}
		store("a", "local", "h1", "@alice:local", 10)
		store("b", "local", "h2", "@alice:local", 5)
		store("c", "local", "h3", "@bob:local", 7)
		store("r", "remote", "h4", "", 100)
		store("q", "remote", "h5", "", 100)
		_, err := db.SetMediaQuarantined(ctx, "q", "remote", true)
		require.NoError(t, err)

		used, err := db.UserMediaBytes(ctx, "@alice:local")
		require.NoError(t, err)
		assert.Equal(t, int64(15), used)
		used, err = db.UserMediaBytes(ctx, "@nobody:local")
		require.NoError(t, err)
		assert.Equal(t, int64(0), used)

		ids := func(media []types.MediaMetadata) (out []string) {
			for _, m := range media {
				out = append(out, string(m.MediaID))
			}
			return
		}
		future := spec.Timestamp(1 << 60)
		local, err := db.UnusedMedia(ctx, true, "local", future, 10)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"a", "b", "c"}, ids(local))
		remote, err := db.UnusedMedia(ctx, false, "local", future, 10)
		require.NoError(t, err)
		assert.Equal(t, []string{"r"}, ids(remote), "quarantined media is left")
		limited, err := db.UnusedMedia(ctx, true, "local", future, 2)
		require.NoError(t, err)
		assert.Len(t, limited, 2)

		// A download after the upload counts, an earlier one does not.
		require.NoError(t, db.TouchMedia(ctx, "a", "local", future))
		require.NoError(t, db.TouchMedia(ctx, "b", "local", 1))
		local, err = db.UnusedMedia(ctx, true, "local", future-1, 10)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"b", "c"}, ids(local))

		// Media of different origins share a file by its hash.
		store("twin", "remote", "h1", "", 10)
		_, inUse, err := db.DeleteMedia(ctx, "a", "local")
		require.NoError(t, err)
		assert.True(t, inUse)
		_, inUse, err = db.DeleteMedia(ctx, "twin", "remote")
		require.NoError(t, err)
		assert.False(t, inUse)
	})
}
