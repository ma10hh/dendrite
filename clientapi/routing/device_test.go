package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/element-hq/dendrite/clientapi/auth"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

type deviceAPI struct {
	userapi.ClientUserAPI
	exists   bool
	created  []userapi.PerformDeviceCreationRequest
	deleted  []userapi.PerformDeviceDeletionRequest
	nextCode int
}

func (d *deviceAPI) PerformDeviceUpdate(_ context.Context, _ *userapi.PerformDeviceUpdateRequest, res *userapi.PerformDeviceUpdateResponse) error {
	res.DeviceExists = d.exists
	return nil
}

func (d *deviceAPI) PerformDeviceCreation(_ context.Context, req *userapi.PerformDeviceCreationRequest, _ *userapi.PerformDeviceCreationResponse) error {
	d.created = append(d.created, *req)
	return nil
}

func (d *deviceAPI) PerformDeviceDeletion(_ context.Context, req *userapi.PerformDeviceDeletionRequest, _ *userapi.PerformDeviceDeletionResponse) error {
	d.deleted = append(d.deleted, *req)
	return nil
}

func TestAppserviceCreatesDevices(t *testing.T) {
	bridge := &userapi.Device{ID: "AS_Device", UserID: "@ghost:example.com", AppserviceID: "bridge"}
	user := &userapi.Device{ID: "D", UserID: "@alice:example.com"}
	put := func(api *deviceAPI, dev *userapi.Device, body string) (code int) {
		req := httptest.NewRequest(http.MethodPut, "/devices/NEWDEV", strings.NewReader(body))
		return UpdateDeviceByID(req, api, dev, "NEWDEV").Code
	}

	api := &deviceAPI{}
	if code := put(api, bridge, `{"display_name": "Bridge device"}`); code != http.StatusCreated {
		t.Fatalf("appservice creating a device: got %d, want 201", code)
	}
	if len(api.created) != 1 {
		t.Fatalf("created %d devices, want 1", len(api.created))
	}
	c := api.created[0]
	if c.Localpart != "ghost" || string(c.ServerName) != "example.com" || *c.DeviceID != "NEWDEV" || *c.DeviceDisplayName != "Bridge device" || c.AccessToken == "" {
		t.Errorf("unexpected device creation %+v", c)
	}

	// Users cannot create devices this way, and existing devices are only updated.
	api = &deviceAPI{}
	if code := put(api, user, `{}`); code != http.StatusNotFound || len(api.created) != 0 {
		t.Errorf("user creating a device: got %d with %d created, want 404 and none", code, len(api.created))
	}
	api = &deviceAPI{exists: true}
	if code := put(api, bridge, `{}`); code != http.StatusOK || len(api.created) != 0 {
		t.Errorf("updating a device: got %d with %d created, want 200 and none", code, len(api.created))
	}
}

func TestAppserviceDeletesDevicesWithoutUIA(t *testing.T) {
	bridge := &userapi.Device{ID: "AS_Device", UserID: "@ghost:example.com", AppserviceID: "bridge"}
	// A nil user interactive auth panics if it is used, so these also prove it is skipped.
	var uia *auth.UserInteractive

	api := &deviceAPI{}
	req := httptest.NewRequest(http.MethodDelete, "/devices/OLD", strings.NewReader(`{}`))
	if res := DeleteDeviceById(req, uia, api, bridge, "OLD"); res.Code != http.StatusOK {
		t.Fatalf("delete one: got %d %v", res.Code, res.JSON)
	}
	if len(api.deleted) != 1 || api.deleted[0].UserID != "@ghost:example.com" || len(api.deleted[0].DeviceIDs) != 1 || api.deleted[0].DeviceIDs[0] != "OLD" {
		t.Errorf("unexpected deletion %+v", api.deleted)
	}

	api = &deviceAPI{}
	req = httptest.NewRequest(http.MethodPost, "/delete_devices", strings.NewReader(`{"devices": ["A", "B"]}`))
	if res := DeleteDevices(req, uia, api, bridge); res.Code != http.StatusOK {
		t.Fatalf("delete many: got %d %v", res.Code, res.JSON)
	}
	if len(api.deleted) != 1 || len(api.deleted[0].DeviceIDs) != 2 {
		t.Errorf("unexpected deletion %+v", api.deleted)
	}
}
