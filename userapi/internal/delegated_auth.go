package internal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"

	"github.com/element-hq/dendrite/clientapi/userutil"
	"github.com/element-hq/dendrite/userapi/api"
)

// Matrix localparts are restricted to the historical user ID grammar.
var delegatedLocalpartRegexp = regexp.MustCompile(`^[a-z0-9._=\-/+]+$`)

// queryDelegatedAuthToken validates an access token issued by the external provider
// (MSC3861). The account and device the token refers to are created on first use,
// as the provider is the source of truth for both.
func (a *UserInternalAPI) queryDelegatedAuthToken(ctx context.Context, accessToken string, res *api.QueryAccessTokenResponse) error {
	token, err := a.DelegatedAuth.Introspect(ctx, accessToken)
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if !token.HasAPIScope || token.DeviceID == "" {
		res.Err = "Forbidden: the access token needs exactly one device scope and the full client API scope"
		return nil
	}

	localpart, serverName, err := userutil.ParseUsernameParam(token.Username, a.Config.Matrix)
	if err != nil || !delegatedLocalpartRegexp.MatchString(localpart) {
		return fmt.Errorf("token introspection returned an unusable username %q", token.Username)
	}

	acc, err := a.DB.GetAccountByLocalpart(ctx, localpart, serverName)
	if errors.Is(err, sql.ErrNoRows) {
		var created api.PerformAccountCreationResponse
		if err = a.PerformAccountCreation(ctx, &api.PerformAccountCreationRequest{
			AccountType: api.AccountTypeUser,
			Localpart:   localpart,
			ServerName:  serverName,
			OnConflict:  api.ConflictUpdate,
		}, &created); err != nil {
			return err
		}
		acc = created.Account
	} else if err != nil {
		return err
	}

	device, err := a.DB.GetDeviceByID(ctx, localpart, serverName, token.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		// Serialise creation so that concurrent first requests do not recreate (and thereby revoke) the device.
		a.delegatedAuthMu.Lock()
		defer a.delegatedAuthMu.Unlock()
		if device, err = a.DB.GetDeviceByID(ctx, localpart, serverName, token.DeviceID); err == nil {
			device.AccountType = acc.AccountType
			res.Device = device
			return nil
		}
		// Devices need an access token in the database. Nobody knows this one and
		// it is never consulted while authentication is delegated.
		placeholder, err := randomToken()
		if err != nil {
			return err
		}
		var created api.PerformDeviceCreationResponse
		if err = a.PerformDeviceCreation(ctx, &api.PerformDeviceCreationRequest{
			Localpart:   localpart,
			ServerName:  serverName,
			DeviceID:    &token.DeviceID,
			AccessToken: placeholder,
		}, &created); err != nil {
			return err
		}
		device = created.Device
	} else if err != nil {
		return err
	}

	device.AccountType = acc.AccountType
	res.Device = device
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
