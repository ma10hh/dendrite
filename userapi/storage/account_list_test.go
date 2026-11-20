package storage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/userapi/api"
)

func TestAdminListAccounts(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()

		mk := func(localpart, displayName string, accountType api.AccountType) {
			_, err := db.CreateAccount(ctx, localpart, "localhost", "pw", "", accountType)
			require.NoError(t, err)
			if displayName != "" {
				_, _, err = db.SetDisplayName(ctx, localpart, "localhost", displayName)
				require.NoError(t, err)
			}
		}
		mk("alice", "Alice Liddell", api.AccountTypeAdmin)
		mk("bob", "Bobby", api.AccountTypeUser)
		mk("carol", "", api.AccountTypeUser)
		mk("guest1", "", api.AccountTypeGuest) // guests get a numeric localpart,
		mk("dave_x", "Dave", api.AccountTypeUser)
		require.NoError(t, db.DeactivateAccount(ctx, "carol", "localhost"))

		ids := func(f api.AccountListFilter) []string {
			entries, _, err := db.AdminListAccounts(ctx, f)
			require.NoError(t, err)
			var out []string
			for _, e := range entries {
				out = append(out, e.UserID)
			}
			return out
		}

		assert.Equal(t, []string{"@alice:localhost", "@bob:localhost", "@dave_x:localhost"}, ids(api.AccountListFilter{}), "no guests or deactivated accounts by default")
		assert.Equal(t, []string{"@1:localhost", "@alice:localhost", "@bob:localhost", "@carol:localhost", "@dave_x:localhost"},
			ids(api.AccountListFilter{Guests: true, Deactivated: true}))
		assert.Equal(t, []string{"@dave_x:localhost", "@bob:localhost", "@alice:localhost"}, ids(api.AccountListFilter{Backwards: true}))
		assert.Equal(t, []string{"@alice:localhost"}, ids(api.AccountListFilter{Name: "LIDDELL"}), "the display name matches, in any case")
		assert.Equal(t, []string{"@bob:localhost"}, ids(api.AccountListFilter{Name: "bob"}))
		assert.Equal(t, []string{"@dave_x:localhost"}, ids(api.AccountListFilter{Name: "_x"}), "an underscore is not a wildcard")
		assert.Empty(t, ids(api.AccountListFilter{UserID: "Bobby"}), "the user ID filter does not look at display names")
		assert.Equal(t, []string{"@bob:localhost"}, ids(api.AccountListFilter{UserID: "@BOB:"}))
		assert.Equal(t, []string{"@alice:localhost", "@bob:localhost", "@dave_x:localhost"}, ids(api.AccountListFilter{OrderBy: "displayname"}), "Alice, Bobby, Dave")
		assert.Equal(t, []string{"@alice:localhost"}, ids(api.AccountListFilter{OrderBy: "admin", Backwards: true, Limit: 1}), "admins first when backwards")

		page, total, err := db.AdminListAccounts(ctx, api.AccountListFilter{Limit: 2})
		require.NoError(t, err)
		assert.Len(t, page, 2)
		assert.Equal(t, 3, total, "the total is not the size of the page")
		page, _, err = db.AdminListAccounts(ctx, api.AccountListFilter{Limit: 2, From: 2})
		require.NoError(t, err)
		assert.Len(t, page, 1)
		assert.Equal(t, "Dave", page[0].DisplayName)
		assert.NotZero(t, page[0].CreatedTS)

		_, _, err = db.AdminListAccounts(ctx, api.AccountListFilter{OrderBy: "name; DROP TABLE userapi_accounts"})
		assert.Error(t, err, "orders are from a fixed set")
	})
}
