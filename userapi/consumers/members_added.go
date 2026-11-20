// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	rsapi "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/userapi/api"
)

// onMembersAdded tells the users of this server who share the room that the users the room was missing have changed
// their device lists. The sync API takes device_lists.changed from the key changes, and these users were not known
// to share the room with anybody here until now, so what a user of this server is told about the room when it joins
// would have been left out for them. A client that is told fetches their keys.
func (s *OutputRoomEventConsumer) onMembersAdded(msg rsapi.OutputMembersAdded) bool {
	if s.keyChanges == nil {
		return true
	}
	var changes []api.DeviceMessage
	for _, ev := range msg.Members {
		if ev.StateKey() == nil || gjson.GetBytes(ev.Content(), "membership").String() != spec.Join {
			continue
		}
		user, err := spec.NewUserID(*ev.StateKey(), true)
		if err != nil || s.cfg.Matrix.IsLocalServerName(user.Domain()) {
			continue
		}
		changes = append(changes, api.DeviceMessage{
			Type:       api.TypeDeviceKeyUpdate,
			DeviceKeys: &api.DeviceKeys{UserID: user.String()},
		})
	}
	if len(changes) == 0 {
		return true
	}
	if err := s.keyChanges.ProduceKeyChanges(changes); err != nil {
		logrus.WithError(err).WithField("room_id", msg.RoomID).Error("failed to report the device lists of the members that were added")
		return false
	}
	return true
}
