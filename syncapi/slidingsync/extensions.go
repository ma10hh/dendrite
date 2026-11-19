// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/matrix-org/gomatrixserverlib"
)

// defaultToDeviceLimit is both the default and the cap for the number of to-device messages in a response.
const defaultToDeviceLimit = 100

// extensionToggle is the part of an extension's configuration every extension has.
type extensionToggle struct {
	Enabled *bool `json:"enabled"`
}

func (t extensionToggle) on() bool { return t.Enabled != nil && *t.Enabled }

type toDeviceConfig struct {
	extensionToggle
	Limit *int   `json:"limit"`
	Since string `json:"since"`
}

// roomExtensionConfig scopes an extension that returns data per room. Both fields default to `["*"]`.
type roomExtensionConfig struct {
	extensionToggle
	Lists *[]string `json:"lists"`
	Rooms *[]string `json:"rooms"`
}

// extensionConfig is the validated `extensions` object of a request. None of it is sticky.
type extensionConfig struct {
	toDevice    toDeviceConfig
	e2ee        extensionToggle
	accountData roomExtensionConfig
	receipts    roomExtensionConfig
	typing      roomExtensionConfig

	// since is toDevice.Since as a position; 0 if the client sent none.
	since int64
}

// parseExtensions reads the extensions of a request. Unknown extensions are ignored.
func parseExtensions(raw map[string]json.RawMessage) (extensionConfig, error) {
	var cfg extensionConfig
	targets := map[string]any{
		"to_device":    &cfg.toDevice,
		"e2ee":         &cfg.e2ee,
		"account_data": &cfg.accountData,
		"receipts":     &cfg.receipts,
		"typing":       &cfg.typing,
	}
	for name, target := range targets {
		data, ok := raw[name]
		if !ok {
			continue
		}
		if err := json.Unmarshal(data, target); err != nil {
			return cfg, fmt.Errorf("invalid %s extension: %w", name, err)
		}
	}
	if limit := cfg.toDevice.Limit; limit != nil && *limit < 0 {
		return cfg, fmt.Errorf("to_device limit must not be negative")
	}
	for name, scoped := range map[string]roomExtensionConfig{"account_data": cfg.accountData, "receipts": cfg.receipts, "typing": cfg.typing} {
		if (scoped.Lists != nil && len(*scoped.Lists) > maxExtensionScope) || (scoped.Rooms != nil && len(*scoped.Rooms) > maxExtensionScope) {
			return cfg, fmt.Errorf("%s names more than %d lists or rooms", name, maxExtensionScope)
		}
	}
	if cfg.toDevice.Since != "" {
		since, err := strconv.ParseInt(cfg.toDevice.Since, 10, 64)
		if err != nil || since < 0 {
			return cfg, fmt.Errorf("to_device since is not a valid token")
		}
		cfg.since = since
	}
	return cfg, nil
}

// ExtensionsResponse holds the data of the extensions the request enabled. An enabled extension is present
// even when it has nothing to say.
type ExtensionsResponse struct {
	ToDevice    *ToDeviceResponse    `json:"to_device,omitempty"`
	E2EE        *E2EEResponse        `json:"e2ee,omitempty"`
	AccountData *AccountDataResponse `json:"account_data,omitempty"`
	Receipts    *RoomEventsResponse  `json:"receipts,omitempty"`
	Typing      *RoomEventsResponse  `json:"typing,omitempty"`
}

type ToDeviceResponse struct {
	NextBatch string                                `json:"next_batch"`
	Events    []gomatrixserverlib.SendToDeviceEvent `json:"events,omitempty"`
}

type E2EEResponse struct {
	DeviceLists *DeviceListsResponse `json:"device_lists,omitempty"`
	// OneTimeKeysCount and UnusedFallbackKeyTypes are sent with every response, as Synapse does.
	OneTimeKeysCount       map[string]int `json:"device_one_time_keys_count"`
	UnusedFallbackKeyTypes []string       `json:"device_unused_fallback_key_types"`
}

type DeviceListsResponse struct {
	Changed []string `json:"changed,omitempty"`
	Left    []string `json:"left,omitempty"`
}

type AccountDataResponse struct {
	Global []RoomEvent            `json:"global,omitempty"`
	Rooms  map[string][]RoomEvent `json:"rooms,omitempty"`
}

// RoomEvent is an ephemeral event, such as a receipt or the typing users, which has no ID or sender.
type RoomEvent struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

type RoomEventsResponse struct {
	Rooms map[string]RoomEvent `json:"rooms,omitempty"`
}

// hasData reports whether the extensions have something a waiting client should be woken for.
// As in Synapse, one-time key counts alone do not count: they are sent with the next response.
func (e *ExtensionsResponse) hasData() bool {
	switch {
	case e == nil:
		return false
	case e.ToDevice != nil && len(e.ToDevice.Events) > 0:
		return true
	case e.E2EE != nil && e.E2EE.DeviceLists != nil:
		return true
	case e.AccountData != nil && (len(e.AccountData.Global) > 0 || len(e.AccountData.Rooms) > 0):
		return true
	case e.Receipts != nil && len(e.Receipts.Rooms) > 0:
		return true
	case e.Typing != nil && len(e.Typing.Rooms) > 0:
		return true
	}
	return false
}
