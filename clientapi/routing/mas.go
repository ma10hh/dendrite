package routing

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/element-hq/dendrite/clientapi/auth/authtypes"
	clientutil "github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/clientapi/userutil"
	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/httputil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

const (
	maxDeviceIDLength = 255
	// How long MAS may replace cross-signing keys after allow_cross_signing_reset.
	crossSigningResetPeriod = 10 * time.Minute
)

// crossSigningResets remembers the users whose cross-signing keys may be replaced without
// re-authentication, as approved by MAS. It lives in memory: an approval is only valid for
// a few minutes, so losing it in a restart just means the user asks again.
var crossSigningResets = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

func allowCrossSigningReset(userID string) {
	crossSigningResets.Lock()
	defer crossSigningResets.Unlock()
	now := time.Now()
	for id, t := range crossSigningResets.until {
		if now.After(t) {
			delete(crossSigningResets.until, id)
		}
	}
	crossSigningResets.until[userID] = now.Add(crossSigningResetPeriod)
}

func consumeCrossSigningReset(userID string) {
	crossSigningResets.Lock()
	defer crossSigningResets.Unlock()
	delete(crossSigningResets.until, userID)
}

func crossSigningResetAllowed(userID string) bool {
	crossSigningResets.Lock()
	defer crossSigningResets.Unlock()
	return time.Now().Before(crossSigningResets.until[userID])
}

// masAPI implements the endpoints Matrix Authentication Service uses to manage the accounts
// and devices of this homeserver. They are the ones Synapse offers under /_synapse/mas/.
type masAPI struct {
	cfg     *config.ClientAPI
	userAPI userapi.ClientUserAPI
	rsAPI   roomserverAPI.ClientRoomserverAPI
	// Serialises device creation, which replaces a device that already exists.
	deviceMu sync.Mutex
}

// setupMASRoutes registers the endpoints under /_synapse/mas/. All of them need the shared secret.
func setupMASRoutes(
	router *mux.Router, cfg *config.ClientAPI,
	userAPI userapi.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI,
) {
	m := &masAPI{cfg: cfg, userAPI: userAPI, rsAPI: rsAPI}
	routes := []struct {
		path    string
		method  string
		handler func(*http.Request) util.JSONResponse
	}{
		{"query_user", http.MethodGet, m.queryUser},
		{"provision_user", http.MethodPost, m.provisionUser},
		{"is_localpart_available", http.MethodGet, m.isLocalpartAvailable},
		{"delete_user", http.MethodPost, m.deleteUser},
		{"reactivate_user", http.MethodPost, m.reactivateUser},
		{"set_displayname", http.MethodPost, m.setDisplayName},
		{"unset_displayname", http.MethodPost, m.unsetDisplayName},
		{"upsert_device", http.MethodPost, m.upsertDevice},
		{"delete_device", http.MethodPost, m.deleteDevice},
		{"update_device_display_name", http.MethodPost, m.updateDeviceDisplayName},
		{"sync_devices", http.MethodPost, m.syncDevices},
		{"allow_cross_signing_reset", http.MethodPost, m.allowCrossSigningReset},
	}
	for _, r := range routes {
		router.Handle("/mas/"+r.path, m.fromMAS("mas_"+r.path, r.handler)).Methods(r.method, http.MethodOptions)
	}
}

// fromMAS refuses everything that does not present the shared secret as bearer token.
func (m *masAPI) fromMAS(name string, f func(*http.Request) util.JSONResponse) http.Handler {
	return httputil.MakeExternalAPI(name, func(req *http.Request) util.JSONResponse {
		secret := m.cfg.MSC3861.SharedSecret
		token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if secret == "" || !ok || subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("This endpoint must only be called by MAS"),
			}
		}
		return f(req)
	})
}

type masUser struct {
	localpart string
	userID    string
	account   *userapi.Account
}

// existingUser finds the account for a localpart, or produces the 404 MAS expects.
func (m *masAPI) existingUser(req *http.Request, localpart string) (*masUser, *util.JSONResponse) {
	if localpart == "" {
		return nil, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("localpart")}
	}
	var res userapi.QueryAccountByLocalpartResponse
	err := m.userAPI.QueryAccountByLocalpart(req.Context(), &userapi.QueryAccountByLocalpartRequest{
		Localpart:  localpart,
		ServerName: m.cfg.Matrix.ServerName,
	}, &res)
	if err != nil || res.Account == nil {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			util.GetLogger(req.Context()).WithError(err).Error("QueryAccountByLocalpart failed")
			return nil, &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		return nil, &util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("User not found")}
	}
	return &masUser{
		localpart: localpart,
		userID:    userutil.MakeUserID(localpart, m.cfg.Matrix.ServerName),
		account:   res.Account,
	}, nil
}

func internalError(req *http.Request, what string, err error) util.JSONResponse {
	util.GetLogger(req.Context()).WithError(err).Error(what + " failed")
	return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
}

func emptyOK() util.JSONResponse {
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

func (m *masAPI) queryUser(req *http.Request) util.JSONResponse {
	user, resErr := m.existingUser(req, req.URL.Query().Get("localpart"))
	if resErr != nil {
		return *resErr
	}
	var displayName, avatarURL *string
	if profile, err := m.userAPI.QueryProfile(req.Context(), user.userID); err == nil && profile != nil {
		if profile.DisplayName != "" {
			displayName = &profile.DisplayName
		}
		if profile.AvatarURL != "" {
			avatarURL = &profile.AvatarURL
		}
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct {
			UserID        string  `json:"user_id"`
			DisplayName   *string `json:"display_name"`
			AvatarURL     *string `json:"avatar_url"`
			IsSuspended   bool    `json:"is_suspended"`
			IsDeactivated bool    `json:"is_deactivated"`
		}{user.userID, displayName, avatarURL, false, user.account.Deactivated},
	}
}

type provisionUserRequest struct {
	Localpart        string    `json:"localpart"`
	UnsetDisplayName bool      `json:"unset_displayname"`
	SetDisplayName   *string   `json:"set_displayname"`
	UnsetAvatarURL   bool      `json:"unset_avatar_url"`
	SetAvatarURL     *string   `json:"set_avatar_url"`
	UnsetEmails      bool      `json:"unset_emails"`
	SetEmails        *[]string `json:"set_emails"`
	// Dendrite has no locked accounts. MAS refuses tokens of locked users itself.
	Locked *bool `json:"locked"`
}

func (m *masAPI) provisionUser(req *http.Request) util.JSONResponse {
	var body provisionUserRequest
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	if (body.UnsetDisplayName && body.SetDisplayName != nil) ||
		(body.UnsetAvatarURL && body.SetAvatarURL != nil) ||
		(body.UnsetEmails && body.SetEmails != nil) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Cannot both set and unset a field")}
	}
	if err := internal.ValidateUsername(body.Localpart, m.cfg.Matrix.ServerName); err != nil {
		return *internal.UsernameResponse(err)
	}
	ctx := req.Context()
	userID := userutil.MakeUserID(body.Localpart, m.cfg.Matrix.ServerName)

	created := false
	if _, resErr := m.existingUser(req, body.Localpart); resErr != nil {
		if resErr.Code != http.StatusNotFound {
			return *resErr
		}
		var res userapi.PerformAccountCreationResponse
		if err := m.userAPI.PerformAccountCreation(ctx, &userapi.PerformAccountCreationRequest{
			AccountType: userapi.AccountTypeUser,
			Localpart:   body.Localpart,
			ServerName:  m.cfg.Matrix.ServerName,
			OnConflict:  userapi.ConflictUpdate,
		}, &res); err != nil {
			return internalError(req, "PerformAccountCreation", err)
		}
		created = res.AccountCreated
	}

	switch {
	case body.UnsetDisplayName:
		if resErr := m.updateDisplayName(req, body.Localpart, ""); resErr != nil {
			return *resErr
		}
	case body.SetDisplayName != nil:
		if resErr := m.updateDisplayName(req, body.Localpart, *body.SetDisplayName); resErr != nil {
			return *resErr
		}
	}
	switch {
	case body.UnsetAvatarURL:
		if resErr := m.updateAvatarURL(req, body.Localpart, ""); resErr != nil {
			return *resErr
		}
	case body.SetAvatarURL != nil:
		if resErr := m.updateAvatarURL(req, body.Localpart, *body.SetAvatarURL); resErr != nil {
			return *resErr
		}
	}
	switch {
	case body.UnsetEmails:
		if resErr := m.syncEmails(req, body.Localpart, nil); resErr != nil {
			return *resErr
		}
	case body.SetEmails != nil:
		if resErr := m.syncEmails(req, body.Localpart, *body.SetEmails); resErr != nil {
			return *resErr
		}
	}
	util.GetLogger(ctx).WithField("user_id", userID).Debug("MAS provisioned user")

	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	return util.JSONResponse{Code: code, JSON: struct{}{}}
}

func (m *masAPI) updateDisplayName(req *http.Request, localpart, displayName string) *util.JSONResponse {
	profile, changed, err := m.userAPI.SetDisplayName(req.Context(), localpart, m.cfg.Matrix.ServerName, displayName)
	if err != nil {
		res := internalError(req, "SetDisplayName", err)
		return &res
	}
	return m.announceProfile(req, localpart, profile, changed)
}

func (m *masAPI) updateAvatarURL(req *http.Request, localpart, avatarURL string) *util.JSONResponse {
	profile, changed, err := m.userAPI.SetAvatarURL(req.Context(), localpart, m.cfg.Matrix.ServerName, avatarURL)
	if err != nil {
		res := internalError(req, "SetAvatarURL", err)
		return &res
	}
	return m.announceProfile(req, localpart, profile, changed)
}

// announceProfile updates the membership events of the user in all their rooms, like a profile change by the user.
func (m *masAPI) announceProfile(req *http.Request, localpart string, profile *authtypes.Profile, changed bool) *util.JSONResponse {
	if !changed {
		return nil
	}
	userID := userutil.MakeUserID(localpart, m.cfg.Matrix.ServerName)
	if res, err := updateProfile(req.Context(), m.rsAPI, &userapi.Device{UserID: userID}, profile, userID, time.Now()); err != nil {
		return &res
	}
	return nil
}

func (m *masAPI) syncEmails(req *http.Request, localpart string, emails []string) *util.JSONResponse {
	ctx := req.Context()
	serverName := m.cfg.Matrix.ServerName
	var current userapi.QueryThreePIDsForLocalpartResponse
	if err := m.userAPI.QueryThreePIDsForLocalpart(ctx, &userapi.QueryThreePIDsForLocalpartRequest{
		Localpart: localpart, ServerName: serverName,
	}, &current); err != nil {
		res := internalError(req, "QueryThreePIDsForLocalpart", err)
		return &res
	}
	wanted := map[string]bool{}
	for _, e := range emails {
		wanted[strings.ToLower(e)] = true
	}
	have := map[string]bool{}
	for _, t := range current.ThreePIDs {
		if t.Medium != "email" {
			continue
		}
		have[strings.ToLower(t.Address)] = true
		if !wanted[strings.ToLower(t.Address)] {
			if err := m.userAPI.PerformForgetThreePID(ctx, &userapi.PerformForgetThreePIDRequest{
				ThreePID: t.Address, Medium: "email",
			}, &struct{}{}); err != nil {
				res := internalError(req, "PerformForgetThreePID", err)
				return &res
			}
		}
	}
	for _, e := range emails {
		if have[strings.ToLower(e)] {
			continue
		}
		have[strings.ToLower(e)] = true
		if err := m.userAPI.PerformSaveThreePIDAssociation(ctx, &userapi.PerformSaveThreePIDAssociationRequest{
			ThreePID: e, Localpart: localpart, ServerName: serverName, Medium: "email",
		}, &struct{}{}); err != nil {
			res := internalError(req, "PerformSaveThreePIDAssociation", err)
			return &res
		}
	}
	return nil
}

func (m *masAPI) isLocalpartAvailable(req *http.Request) util.JSONResponse {
	localpart := req.URL.Query().Get("localpart")
	if localpart == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("Missing localpart")}
	}
	// Reuse the checks of /register/available.
	check := req.Clone(req.Context())
	q := check.URL.Query()
	q.Set("username", localpart)
	check.URL.RawQuery = q.Encode()
	check.Host = string(m.cfg.Matrix.ServerName)
	res := RegisterAvailable(check, m.cfg, m.userAPI)
	if res.Code == http.StatusOK {
		return emptyOK()
	}
	return res
}

type localpartRequest struct {
	Localpart string `json:"localpart"`
}

func (m *masAPI) deleteUser(req *http.Request) util.JSONResponse {
	var body struct {
		localpartRequest
		// Erasing user data is not supported, the account is only deactivated.
		Erase bool `json:"erase"`
	}
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	var res userapi.PerformAccountDeactivationResponse
	if err := m.userAPI.PerformAccountDeactivation(req.Context(), &userapi.PerformAccountDeactivationRequest{
		Localpart: user.localpart, ServerName: m.cfg.Matrix.ServerName,
	}, &res); err != nil {
		return internalError(req, "PerformAccountDeactivation", err)
	}
	return emptyOK()
}

func (m *masAPI) reactivateUser(req *http.Request) util.JSONResponse {
	var body localpartRequest
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	var res userapi.PerformAccountReactivationResponse
	if err := m.userAPI.PerformAccountReactivation(req.Context(), &userapi.PerformAccountReactivationRequest{
		Localpart: user.localpart, ServerName: m.cfg.Matrix.ServerName,
	}, &res); err != nil {
		return internalError(req, "PerformAccountReactivation", err)
	}
	return emptyOK()
}

func (m *masAPI) setDisplayName(req *http.Request) util.JSONResponse {
	var body struct {
		localpartRequest
		DisplayName string `json:"displayname"`
	}
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	return m.changeDisplayName(req, body.Localpart, body.DisplayName)
}

func (m *masAPI) unsetDisplayName(req *http.Request) util.JSONResponse {
	var body localpartRequest
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	return m.changeDisplayName(req, body.Localpart, "")
}

func (m *masAPI) changeDisplayName(req *http.Request, localpart, displayName string) util.JSONResponse {
	user, resErr := m.existingUser(req, localpart)
	if resErr != nil {
		return *resErr
	}
	if resErr = m.updateDisplayName(req, user.localpart, displayName); resErr != nil {
		return *resErr
	}
	return emptyOK()
}

type deviceRequest struct {
	localpartRequest
	DeviceID string `json:"device_id"`
}

func validDeviceID(id string) bool {
	return id != "" && len(id) <= maxDeviceIDLength
}

func (m *masAPI) upsertDevice(req *http.Request) util.JSONResponse {
	var body struct {
		deviceRequest
		DisplayName *string `json:"display_name"`
	}
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	if !validDeviceID(body.DeviceID) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid device_id")}
	}
	created, err := m.ensureDevice(req, user, body.DeviceID, body.DisplayName)
	if err != nil {
		return internalError(req, "upserting the device", err)
	}
	if created {
		return util.JSONResponse{Code: http.StatusCreated, JSON: struct{}{}}
	}
	return emptyOK()
}

// ensureDevice creates the device, or updates its display name if it exists. It reports whether it was created.
func (m *masAPI) ensureDevice(req *http.Request, user *masUser, deviceID string, displayName *string) (bool, error) {
	m.deviceMu.Lock()
	defer m.deviceMu.Unlock()
	ctx := req.Context()

	var existing userapi.QueryDevicesResponse
	if err := m.userAPI.QueryDevices(ctx, &userapi.QueryDevicesRequest{UserID: user.userID}, &existing); err != nil {
		return false, err
	}
	for _, d := range existing.Devices {
		if d.ID == deviceID {
			if displayName != nil {
				var res userapi.PerformDeviceUpdateResponse
				return false, m.userAPI.PerformDeviceUpdate(ctx, &userapi.PerformDeviceUpdateRequest{
					RequestingUserID: user.userID, DeviceID: deviceID, DisplayName: displayName,
				}, &res)
			}
			return false, nil
		}
	}
	return m.createDevice(req, user, deviceID, displayName)
}

// createDevice creates a device that is known not to exist. The caller holds deviceMu.
func (m *masAPI) createDevice(req *http.Request, user *masUser, deviceID string, displayName *string) (bool, error) {
	ctx := req.Context()
	// Devices need an access token in the database. Nobody knows this one and
	// it is never consulted while authentication is delegated.
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return false, err
	}
	var res userapi.PerformDeviceCreationResponse
	err := m.userAPI.PerformDeviceCreation(ctx, &userapi.PerformDeviceCreationRequest{
		Localpart:         user.localpart,
		ServerName:        m.cfg.Matrix.ServerName,
		AccessToken:       base64.RawURLEncoding.EncodeToString(b),
		DeviceID:          &deviceID,
		DeviceDisplayName: displayName,
	}, &res)
	return err == nil, err
}

func (m *masAPI) deleteDevice(req *http.Request) util.JSONResponse {
	var body deviceRequest
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	if !validDeviceID(body.DeviceID) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid device_id")}
	}
	var res userapi.PerformDeviceDeletionResponse
	if err := m.userAPI.PerformDeviceDeletion(req.Context(), &userapi.PerformDeviceDeletionRequest{
		UserID: user.userID, DeviceIDs: []string{body.DeviceID},
	}, &res); err != nil {
		return internalError(req, "PerformDeviceDeletion", err)
	}
	return util.JSONResponse{Code: http.StatusNoContent}
}

func (m *masAPI) updateDeviceDisplayName(req *http.Request) util.JSONResponse {
	var body struct {
		deviceRequest
		DisplayName string `json:"display_name"`
	}
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	var res userapi.PerformDeviceUpdateResponse
	if err := m.userAPI.PerformDeviceUpdate(req.Context(), &userapi.PerformDeviceUpdateRequest{
		RequestingUserID: user.userID, DeviceID: body.DeviceID, DisplayName: &body.DisplayName,
	}, &res); err != nil {
		return internalError(req, "PerformDeviceUpdate", err)
	}
	if !res.DeviceExists {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound(fmt.Sprintf("Unknown device %q", body.DeviceID))}
	}
	return emptyOK()
}

// syncDevices makes the devices of a user exactly the given set.
func (m *masAPI) syncDevices(req *http.Request) util.JSONResponse {
	var body struct {
		localpartRequest
		Devices []string `json:"devices"`
	}
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	target := map[string]bool{}
	for _, id := range body.Devices {
		if !validDeviceID(id) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid device ID in devices")}
		}
		target[id] = true
	}

	m.deviceMu.Lock()
	defer m.deviceMu.Unlock()
	var existing userapi.QueryDevicesResponse
	if err := m.userAPI.QueryDevices(req.Context(), &userapi.QueryDevicesRequest{UserID: user.userID}, &existing); err != nil {
		return internalError(req, "QueryDevices", err)
	}
	have := map[string]bool{}
	var extra []string
	for _, d := range existing.Devices {
		have[d.ID] = true
		if !target[d.ID] {
			extra = append(extra, d.ID)
		}
	}
	if len(extra) > 0 {
		var res userapi.PerformDeviceDeletionResponse
		if err := m.userAPI.PerformDeviceDeletion(req.Context(), &userapi.PerformDeviceDeletionRequest{
			UserID: user.userID, DeviceIDs: extra,
		}, &res); err != nil {
			return internalError(req, "PerformDeviceDeletion", err)
		}
	}
	for id := range target {
		if have[id] {
			continue
		}
		if _, err := m.createDevice(req, user, id, nil); err != nil {
			return internalError(req, "creating a device", err)
		}
	}
	return emptyOK()
}

func (m *masAPI) allowCrossSigningReset(req *http.Request) util.JSONResponse {
	var body localpartRequest
	if resErr := clientutil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	user, resErr := m.existingUser(req, body.Localpart)
	if resErr != nil {
		return *resErr
	}
	allowCrossSigningReset(user.userID)
	return emptyOK()
}
