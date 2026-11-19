package internal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"

	"github.com/element-hq/dendrite/clientapi/userutil"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// Matrix localparts are restricted to the historical user ID grammar.
var delegatedLocalpartRegexp = regexp.MustCompile(`^[a-z0-9._=\-/+]+$`)

// queryDelegatedAuthToken validates an access token issued by the external provider
// (MSC3861).
//
// With a generic provider the account and the device the token refers to are created on first
// use, as the provider is the source of truth for both. With Matrix Authentication Service they
// are provisioned through the /_synapse/mas/ endpoints instead, so a token whose user or device
// does not exist is refused. That is also how a logout takes effect: MAS deletes the device.
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
		// The provider vouches for a user this server cannot represent. That is a problem with
		// the token, not a server fault, so clients must not retry it.
		util.GetLogger(ctx).Warnf("token introspection returned an unusable username %q", token.Username)
		res.Err = "Forbidden: the access token belongs to a user this server cannot represent"
		return nil
	}
	masMode := a.DelegatedAuth.MASMode()

	acc, err := a.DB.GetAccountByLocalpart(ctx, localpart, serverName)
	switch {
	case errors.Is(err, sql.ErrNoRows) && masMode:
		res.Err = "Forbidden: this account has not been provisioned"
		return nil
	case errors.Is(err, sql.ErrNoRows):
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
	case err != nil:
		return err
	}
	if acc.Deactivated {
		res.Err = "Forbidden: this account has been deactivated"
		return nil
	}

	device, err := a.DB.GetDeviceByID(ctx, localpart, serverName, token.DeviceID)
	switch {
	case errors.Is(err, sql.ErrNoRows) && masMode:
		// MAS deleted the device, so the token must stop working even if it is still cached.
		a.DelegatedAuth.Invalidate(accessToken)
		return nil
	case errors.Is(err, sql.ErrNoRows):
		if device, err = a.createDelegatedDevice(ctx, localpart, serverName, token.DeviceID); err != nil {
			return err
		}
	case err != nil:
		return err
	}

	device.AccountType = acc.AccountType
	if token.IsAdmin {
		device.AccountType = api.AccountTypeAdmin
	}
	res.Device = device
	return nil
}

// createDelegatedDevice creates the device a token refers to, unless a concurrent request already did.
func (a *UserInternalAPI) createDelegatedDevice(ctx context.Context, localpart string, serverName spec.ServerName, deviceID string) (*api.Device, error) {
	// Serialise creation so that concurrent first requests do not recreate (and thereby revoke) the device.
	a.delegatedAuthMu.Lock()
	defer a.delegatedAuthMu.Unlock()
	device, err := a.DB.GetDeviceByID(ctx, localpart, serverName, deviceID)
	if err == nil {
		return device, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Devices need an access token in the database. Nobody knows this one and
	// it is never consulted while authentication is delegated.
	placeholder, err := randomToken()
	if err != nil {
		return nil, err
	}
	var created api.PerformDeviceCreationResponse
	if err = a.PerformDeviceCreation(ctx, &api.PerformDeviceCreationRequest{
		Localpart:   localpart,
		ServerName:  serverName,
		DeviceID:    &deviceID,
		AccessToken: placeholder,
	}, &created); err != nil {
		return nil, err
	}
	return created.Device, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
