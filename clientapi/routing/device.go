// Copyright 2024 New Vector Ltd.
// Copyright 2017 Paul Tötterman <paul.totterman@iki.fi>
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/element-hq/dendrite/clientapi/auth"
	"github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"
)

// https://matrix.org/docs/spec/client_server/r0.6.1#get-matrix-client-r0-devices
type deviceJSON struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name"`
	LastSeenIP  string `json:"last_seen_ip"`
	LastSeenTS  int64  `json:"last_seen_ts"`
}

type devicesJSON struct {
	Devices []deviceJSON `json:"devices"`
}

type deviceUpdateJSON struct {
	DisplayName *string `json:"display_name"`
}

type devicesDeleteJSON struct {
	Devices []string `json:"devices"`
}

// GetDeviceByID handles /devices/{deviceID}
func GetDeviceByID(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device,
	deviceID string,
) util.JSONResponse {
	var queryRes api.QueryDevicesResponse
	err := userAPI.QueryDevices(req.Context(), &api.QueryDevicesRequest{
		UserID: device.UserID,
	}, &queryRes)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryDevices failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	var targetDevice *api.Device
	for _, device := range queryRes.Devices {
		if device.ID == deviceID {
			targetDevice = &device
			break
		}
	}
	if targetDevice == nil {
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound("Unknown device"),
		}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: deviceJSON{
			DeviceID:    targetDevice.ID,
			DisplayName: targetDevice.DisplayName,
			LastSeenIP:  stripIPPort(targetDevice.LastSeenIP),
			LastSeenTS:  targetDevice.LastSeenTS,
		},
	}
}

// GetDevicesByLocalpart handles /devices
func GetDevicesByLocalpart(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device,
) util.JSONResponse {
	var queryRes api.QueryDevicesResponse
	err := userAPI.QueryDevices(req.Context(), &api.QueryDevicesRequest{
		UserID: device.UserID,
	}, &queryRes)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryDevices failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	res := devicesJSON{}

	for _, dev := range queryRes.Devices {
		res.Devices = append(res.Devices, deviceJSON{
			DeviceID:    dev.ID,
			DisplayName: dev.DisplayName,
			LastSeenIP:  stripIPPort(dev.LastSeenIP),
			LastSeenTS:  dev.LastSeenTS,
		})
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: res,
	}
}

// deviceCreationMu keeps two creations of the same device from replacing each other: an appservice and MAS can both
// create the device of a user, and the second creation would revoke the token of the first.
var deviceCreationMu sync.Mutex

// createAppserviceDevice creates a device for the user an appservice acts as (MSC4190).
func createAppserviceDevice(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device, deviceID string, displayName *string,
) util.JSONResponse {
	localpart, domain, err := gomatrixserverlib.SplitID('@', device.UserID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("The appservice does not act as a user")}
	}
	deviceCreationMu.Lock()
	defer deviceCreationMu.Unlock()
	var existing api.QueryDevicesResponse
	if err = userAPI.QueryDevices(req.Context(), &api.QueryDevicesRequest{UserID: device.UserID}, &existing); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryDevices failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	for _, d := range existing.Devices {
		if d.ID == deviceID {
			return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
		}
	}
	// Devices need an access token in the database. Nobody knows this one: appservices use their own token.
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	var res api.PerformDeviceCreationResponse
	if err = userAPI.PerformDeviceCreation(req.Context(), &api.PerformDeviceCreationRequest{
		Localpart:         localpart,
		ServerName:        domain,
		AccessToken:       base64.RawURLEncoding.EncodeToString(token),
		DeviceID:          &deviceID,
		DeviceDisplayName: displayName,
	}, &res); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("PerformDeviceCreation failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusCreated, JSON: struct{}{}}
}

// UpdateDeviceByID handles PUT on /devices/{deviceID}
func UpdateDeviceByID(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device,
	deviceID string,
) util.JSONResponse {

	defer req.Body.Close() // nolint: errcheck

	payload := deviceUpdateJSON{}

	if resErr := httputil.UnmarshalJSONRequest(req, &payload); resErr != nil {
		return *resErr
	}

	var performRes api.PerformDeviceUpdateResponse
	err := userAPI.PerformDeviceUpdate(req.Context(), &api.PerformDeviceUpdateRequest{
		RequestingUserID: device.UserID,
		DeviceID:         deviceID,
		DisplayName:      payload.DisplayName,
	}, &performRes)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("PerformDeviceUpdate failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	if !performRes.DeviceExists {
		// Appservices create the devices of their users here instead of logging them in (MSC4190).
		if device.AppserviceID != "" {
			return createAppserviceDevice(req, userAPI, device, deviceID, payload.DisplayName)
		}
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.Forbidden("device does not exist"),
		}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}

// DeleteDeviceById handles DELETE requests to /devices/{deviceId}
func DeleteDeviceById(
	req *http.Request, userInteractiveAuth *auth.UserInteractive, userAPI api.ClientUserAPI, device *api.Device,
	deviceID string,
) util.JSONResponse {
	var (
		deleteOK  bool
		sessionID string
	)
	defer func() {
		if deleteOK {
			sessions.deleteSession(sessionID)
		}
	}()
	ctx := req.Context()
	defer req.Body.Close() // nolint:errcheck
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("The request body could not be read: " + err.Error()),
		}
	}

	// check that we know this session, and it matches with the device to delete
	s := gjson.GetBytes(bodyBytes, "auth.session").Str
	if dev, ok := sessions.getDeviceToDelete(s); ok {
		if dev != deviceID {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("session and device mismatch"),
			}
		}
	}

	if s != "" {
		sessionID = s
	}

	// Appservices act for registered users and need no re-authentication (MSC4190).
	if device.AppserviceID == "" {
		login, errRes := userInteractiveAuth.Verify(ctx, bodyBytes, device)
		if errRes != nil {
			switch data := errRes.JSON.(type) {
			case auth.Challenge:
				sessions.addDeviceToDelete(data.Session, deviceID)
			default:
			}
			return *errRes
		}

		localpart, _, err := gomatrixserverlib.SplitID('@', device.UserID)
		if err != nil {
			util.GetLogger(ctx).WithError(err).Error("gomatrixserverlib.SplitID failed")
			return util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}

		// make sure that the access token being used matches the login creds used for user interactive auth, else
		// 1 compromised access token could be used to logout all devices.
		if login.Username() != localpart && login.Username() != device.UserID {
			return util.JSONResponse{
				Code: 403,
				JSON: spec.Forbidden("Cannot delete another user's device"),
			}
		}
	}

	var res api.PerformDeviceDeletionResponse
	if err := userAPI.PerformDeviceDeletion(ctx, &api.PerformDeviceDeletionRequest{
		UserID:    device.UserID,
		DeviceIDs: []string{deviceID},
	}, &res); err != nil {
		util.GetLogger(ctx).WithError(err).Error("userAPI.PerformDeviceDeletion failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	deleteOK = true

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}

// DeleteDevices handles POST requests to /delete_devices
func DeleteDevices(
	req *http.Request, userInteractiveAuth *auth.UserInteractive, userAPI api.ClientUserAPI, device *api.Device,
) util.JSONResponse {
	ctx := req.Context()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("The request body could not be read: " + err.Error()),
		}
	}
	defer req.Body.Close() // nolint:errcheck

	// initiate UIA. Appservices act for registered users and need no re-authentication (MSC4190).
	if device.AppserviceID == "" {
		login, errRes := userInteractiveAuth.Verify(ctx, bodyBytes, device)
		if errRes != nil {
			return *errRes
		}

		if login.Username() != device.UserID {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("unable to delete devices for other user"),
			}
		}
	}

	payload := devicesDeleteJSON{}
	if err = json.Unmarshal(bodyBytes, &payload); err != nil {
		util.GetLogger(ctx).WithError(err).Error("unable to unmarshal device deletion request")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// An empty list deletes nothing. The user API reads an empty list as "all the devices", which is for the other
	// callers, and an appservice that sends {} must not log its user out everywhere.
	if payload.Devices == nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("devices must be a list of device IDs")}
	}
	if len(payload.Devices) == 0 {
		return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
	}

	var res api.PerformDeviceDeletionResponse
	if err := userAPI.PerformDeviceDeletion(ctx, &api.PerformDeviceDeletionRequest{
		UserID:    device.UserID,
		DeviceIDs: payload.Devices,
	}, &res); err != nil {
		util.GetLogger(ctx).WithError(err).Error("userAPI.PerformDeviceDeletion failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}

// stripIPPort converts strings like "[::1]:12345" to "::1"
func stripIPPort(addr string) string {
	ip := net.ParseIP(addr)
	if ip != nil {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	ip = net.ParseIP(host)
	if ip != nil {
		return host
	}
	return ""
}
