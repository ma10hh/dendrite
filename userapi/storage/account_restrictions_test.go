package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/userapi/api"
)

func TestAccountRestrictions(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()

		_, err := db.CreateAccount(ctx, "alice", "localhost", "", "", api.AccountTypeUser)
		require.NoError(t, err)
		_, err = db.CreateAccount(ctx, "bob", "localhost", "", "", api.AccountTypeUser)
		require.NoError(t, err)

		r, err := db.AccountRestrictions(ctx, "alice", "localhost")
		require.NoError(t, err)
		assert.Equal(t, api.AccountRestrictions{}, r, "nothing is set by default")

		// Each setting is kept when another one changes, and settings of other accounts are not touched.
		require.NoError(t, db.SetShadowBanned(ctx, "alice", "localhost", true))
		require.NoError(t, db.SetRateLimitOverride(ctx, "alice", "localhost", &api.RateLimitOverride{MessagesPerSecond: 0.5, BurstCount: 3}))
		require.NoError(t, db.SetAccountExpiry(ctx, "alice", "localhost", 1234))
		r, err = db.AccountRestrictions(ctx, "alice", "localhost")
		require.NoError(t, err)
		assert.True(t, r.ShadowBanned)
		assert.Equal(t, &api.RateLimitOverride{MessagesPerSecond: 0.5, BurstCount: 3}, r.RateLimit)
		assert.EqualValues(t, 1234, r.ExpiresTS)
		r, err = db.AccountRestrictions(ctx, "bob", "localhost")
		require.NoError(t, err)
		assert.Equal(t, api.AccountRestrictions{}, r)

		require.NoError(t, db.SetShadowBanned(ctx, "alice", "localhost", false))
		require.NoError(t, db.SetRateLimitOverride(ctx, "alice", "localhost", nil))
		r, err = db.AccountRestrictions(ctx, "alice", "localhost")
		require.NoError(t, err)
		assert.False(t, r.ShadowBanned)
		assert.Nil(t, r.RateLimit)
		assert.EqualValues(t, 1234, r.ExpiresTS, "the expiry stays")

		// An override of zero is an override: it means no limit.
		require.NoError(t, db.SetRateLimitOverride(ctx, "bob", "localhost", &api.RateLimitOverride{}))
		r, err = db.AccountRestrictions(ctx, "bob", "localhost")
		require.NoError(t, err)
		require.NotNil(t, r.RateLimit)
		assert.Equal(t, api.RateLimitOverride{}, *r.RateLimit)

		// The account list shows the shadow ban.
		require.NoError(t, db.SetShadowBanned(ctx, "bob", "localhost", true))
		entries, _, err := db.AdminListAccounts(ctx, api.AccountListFilter{Guests: true, Deactivated: true})
		require.NoError(t, err)
		banned := map[string]bool{}
		for _, e := range entries {
			banned[e.Localpart] = e.ShadowBanned
		}
		assert.True(t, banned["bob"])
		assert.False(t, banned["alice"])
	})
}
