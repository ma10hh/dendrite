// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"sort"

	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// extensionRun is the context shared by the extensions while one response is built.
type extensionRun struct {
	ctx      context.Context
	snapshot storage.DatabaseTransaction
	device   *userapi.Device
	conn     *Connection
	cfg      extensionConfig
	plan     plan
	update   *update
	// now is the notifier position taken before the response was started. Everything up to it is covered.
	now  types.StreamingToken
	resp *Response
}

// buildExtensions fills in the extensions the request enabled. It returns what to do to the connection once the
// response is on its way: move the extensions' positions on.
func (h *Handler) buildExtensions(r *extensionRun) (commit func(), err error) {
	cfg := r.cfg
	if !cfg.toDevice.on() && !cfg.e2ee.on() && !cfg.accountData.on() && !cfg.receipts.on() && !cfg.typing.on() {
		return func() {}, nil
	}
	ignored, err := ignoredUsers(r.ctx, r.snapshot, r.device.UserID)
	if err != nil {
		return nil, err
	}

	ext := &ExtensionsResponse{}
	var commits []func()
	steps := []struct {
		enabled bool
		run     func() (func(), error)
	}{
		{cfg.toDevice.on(), func() (func(), error) { return h.toDevice(r, ext, ignored) }},
		{cfg.e2ee.on(), func() (func(), error) { return h.e2ee(r, ext) }},
		{cfg.accountData.on(), func() (func(), error) { return h.accountData(r, ext) }},
		{cfg.receipts.on(), func() (func(), error) { return h.receipts(r, ext, ignored) }},
		{cfg.typing.on(), func() (func(), error) { return h.typingUsers(r, ext, ignored) }},
	}
	for _, step := range steps {
		if !step.enabled {
			continue
		}
		stepCommit, err := step.run()
		if err != nil {
			return nil, err
		}
		commits = append(commits, stepCommit)
	}

	r.resp.Extensions = ext
	return func() {
		for _, c := range commits {
			c()
		}
	}, nil
}

// scopedRooms returns the joined rooms in this response that a room-based extension applies to.
//
// As in Synapse, only rooms that are in the response qualify. `rooms` and `lists` both default to `["*"]`:
// `*` in `rooms` is every room in the response, `*` in `lists` every list, and a named list stands for the
// rooms in its window.
func (r *extensionRun) scopedRooms(cfg roomExtensionConfig) []string {
	inResponse := make(map[string]struct{}, len(r.update.rooms))
	for roomID := range r.update.rooms {
		if sent := r.update.sent[roomID]; sent != nil && sent.kind == kindJoined {
			inResponse[roomID] = struct{}{}
		}
	}

	rooms, lists := []string{"*"}, []string{"*"}
	if cfg.Rooms != nil {
		rooms = *cfg.Rooms
	}
	if cfg.Lists != nil {
		lists = *cfg.Lists
	}

	wanted := map[string]struct{}{}
	for _, roomID := range rooms {
		if roomID == "*" {
			for id := range inResponse {
				wanted[id] = struct{}{}
			}
			continue
		}
		wanted[roomID] = struct{}{}
	}
	for _, name := range lists {
		for listName, roomIDs := range r.plan.byList {
			if name != "*" && name != listName {
				continue
			}
			for _, roomID := range roomIDs {
				wanted[roomID] = struct{}{}
			}
		}
	}

	var scoped []string
	for roomID := range wanted {
		if _, ok := inResponse[roomID]; ok {
			scoped = append(scoped, roomID)
		}
	}
	sort.Strings(scoped)
	return scoped
}

// isInitial reports whether the room is sent in full, or with a larger timeline, in this response.
// Extensions send such rooms' data again from the beginning.
func (r *extensionRun) isInitial(roomID string) bool {
	room := r.update.rooms[roomID]
	return room.Initial || room.ExpandedTimeline
}
