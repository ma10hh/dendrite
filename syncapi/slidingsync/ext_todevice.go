// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"fmt"
	"strconv"

	"github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/syncapi/types"
)

// toDevice returns the device's pending to-device messages.
//
// The client's `since` is the only cursor: it is both where reading starts and an acknowledgement that
// everything up to it was received, which is when those messages are deleted.
func (h *Handler) toDevice(r *extensionRun, ext *ExtensionsResponse, ignored map[string]interface{}) (func(), error) {
	since := types.StreamPosition(r.cfg.since)
	to := r.now.SendToDevicePosition
	limit := defaultToDeviceLimit
	if l := r.cfg.toDevice.Limit; l != nil {
		limit = min(*l, defaultToDeviceLimit)
	}

	out := &ToDeviceResponse{NextBatch: strconv.FormatInt(int64(since), 10)}
	ext.ToDevice = out
	ack := func() {
		if since == 0 {
			return
		}
		if err := h.db.CleanSendToDeviceUpdates(r.ctx, r.device.UserID, r.device.ID, since); err != nil {
			logrus.WithError(err).Warn("failed to delete acknowledged to-device messages")
		}
	}
	if since >= to || limit == 0 {
		return ack, nil
	}

	next, events, err := r.snapshot.SendToDeviceUpdatesForSync(r.ctx, r.device.UserID, r.device.ID, since, to)
	if err != nil {
		return nil, fmt.Errorf("loading to-device messages: %w", err)
	}
	if len(events) > limit {
		events = events[:limit]
		next = events[limit-1].ID
	}
	out.NextBatch = strconv.FormatInt(int64(next), 10)
	for _, event := range events {
		// Messages from ignored users are dropped, but still acknowledged by moving next_batch past them.
		if _, isIgnored := ignored[event.Sender]; !isIgnored {
			out.Events = append(out.Events, event.SendToDeviceEvent)
		}
	}
	return ack, nil
}
