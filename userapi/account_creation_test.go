package userapi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/element-hq/dendrite/userapi/internal"
	"github.com/element-hq/dendrite/userapi/storage"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type failingCreate struct {
	storage.UserDatabase
}

func (failingCreate) CreateAccount(context.Context, string, spec.ServerName, string, string, api.AccountType) (*api.Account, error) {
	return nil, errors.New("the database is not there")
}

// An account that could not be created is an error for the caller. The answer used to be success with
// AccountCreated=false, which MAS took for "it exists" when it provisioned a user.
func TestAccountCreationErrorIsReturned(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		userAPI, _, close := MustMakeInternalAPI(t, apiTestOpts{}, dbType, nil)
		defer close()
		internalAPI := userAPI.(*internal.UserInternalAPI)
		internalAPI.DB = failingCreate{internalAPI.DB}

		var res api.PerformAccountCreationResponse
		err := userAPI.PerformAccountCreation(context.Background(), &api.PerformAccountCreationRequest{
			AccountType: api.AccountTypeUser, Localpart: "newuser", ServerName: serverName, OnConflict: api.ConflictUpdate,
		}, &res)
		if err == nil {
			t.Fatalf("no error for an account that was not created: %+v", res)
		}
	})
}
