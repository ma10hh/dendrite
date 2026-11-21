// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package eventutil

import (
	"fmt"
	"runtime/debug"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// CheckUntrustedEvent rejects events from other servers that gomatrixserverlib parses but cannot be used: its check
// of a room ID accepts "!:", and then every call of RoomID() on the event panics, as does the verification of its
// signatures. An event from another server goes through this after it is parsed and before anything else is done
// with it.
func CheckUntrustedEvent(event gomatrixserverlib.PDU) error {
	roomID := gjson.GetBytes(event.JSON(), "room_id")
	// The create event of a room version with room IDs made from its event has none.
	if !roomID.Exists() && event.Type() == spec.MRoomCreate {
		return nil
	}
	if roomID.Type != gjson.String {
		return fmt.Errorf("the event has no room ID")
	}
	if _, err := spec.NewRoomID(roomID.String()); err != nil {
		return fmt.Errorf("the room ID %q is invalid: %w", roomID.String(), err)
	}
	return nil
}

// LogPanic is deferred at the top of a goroutine that handles data from other servers: a panic there would stop the
// whole process, so it is logged with its stack, and the goroutine ends. It must be deferred directly.
func LogPanic(what string) {
	if p := recover(); p != nil {
		logrus.WithField("stack", string(debug.Stack())).Errorf("Recovered from a panic in %s: %v", what, p)
	}
}
