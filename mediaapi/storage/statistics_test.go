package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/test"
)

func TestUserMediaStatisticsStorage(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateDatabase(t, dbType)
		defer close()
		ctx := context.Background()
		store := func(id, _, user string, size int64) {
			require.NoError(t, db.StoreMediaMetadata(ctx, &types.MediaMetadata{
				MediaID: types.MediaID(id), Origin: "local", ContentType: "text/plain", FileSizeBytes: types.FileSizeBytes(size),
				UploadName: "f", Base64Hash: types.Base64Hash("h" + id), UserID: types.MatrixUserID(user),
			}))
		}
		store("a", "", "@alice:local", 10)
		store("b", "", "@alice:local", 5)
		store("c", "", "@bob_x:local", 7)
		require.NoError(t, db.StoreMediaMetadata(ctx, &types.MediaMetadata{
			MediaID: "r", Origin: "remote", ContentType: "text/plain", FileSizeBytes: 1000, UploadName: "f", Base64Hash: "hr", UserID: "@carol:remote",
		}))

		stats, total, err := db.UserMediaStatistics(ctx, types.UserMediaStatisticsFilter{Origin: "local", Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, 2, total, "the copies of remote media are not counted")
		assert.Equal(t, []types.UserMediaStatistics{
			{UserID: "@alice:local", MediaCount: 2, MediaLength: 15},
			{UserID: "@bob_x:local", MediaCount: 1, MediaLength: 7},
		}, stats)

		stats, _, err = db.UserMediaStatistics(ctx, types.UserMediaStatisticsFilter{Origin: "local", OrderBy: "media_length", Backwards: true, Limit: 1, From: 1})
		require.NoError(t, err)
		require.Len(t, stats, 1)
		assert.Equal(t, "@bob_x:local", stats[0].UserID)

		_, total, err = db.UserMediaStatistics(ctx, types.UserMediaStatisticsFilter{Origin: "local", Search: "_x", Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, 1, total, "the underscore is not a wildcard, so it finds bob_x and not alice")

		_, _, err = db.UserMediaStatistics(ctx, types.UserMediaStatisticsFilter{Origin: "local", OrderBy: "nope", Limit: 10})
		assert.ErrorIs(t, err, types.ErrUnknownOrder)
	})
}
