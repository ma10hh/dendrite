// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"fmt"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/syncapi/internal"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
	keytypes "github.com/element-hq/dendrite/userapi/types"
)

// e2ee returns the end-to-end encryption details the device needs: one-time key counts, unused fallback key
// types, and which users' device lists changed. Key counts are sent with every response; device list
// changes are relative to the connection's previous e2ee response and are not sent on the first.
func (h *Handler) e2ee(r *extensionRun, ext *ExtensionsResponse) (func(), error) {
	out := &E2EEResponse{OneTimeKeysCount: map[string]int{}, UnusedFallbackKeyTypes: []string{}}
	ext.E2EE = out

	var keys types.Response
	if err := internal.DeviceOTKCounts(r.ctx, h.userAPI, r.device.UserID, r.device.ID, &keys); err != nil {
		// Keys are not worth failing the whole sync for: report none and try again next time.
		logrus.WithError(err).Warn("failed to load one-time key counts")
	}
	for algorithm, count := range keys.DeviceListsOTKCount {
		out.OneTimeKeysCount[algorithm] = count
	}
	if keys.DeviceListsUnusedFallbackAlgorithms != nil {
		out.UnusedFallbackKeyTypes = keys.DeviceListsUnusedFallbackAlgorithms
	}

	state := &r.conn.ext
	newDeviceListPos := r.now.DeviceListPosition
	if state.e2eeStarted {
		changed, left, pos, err := h.deviceListChanges(r, state.deviceListPos, state.pduPos)
		if err != nil {
			return nil, err
		}
		newDeviceListPos = pos
		if len(changed) > 0 || len(left) > 0 {
			out.DeviceLists = &DeviceListsResponse{Changed: changed, Left: left}
		}
	}

	pduPos := r.now.PDUPosition
	return func() {
		state.e2eeStarted = true
		state.deviceListPos, state.pduPos = newDeviceListPos, pduPos
	}, nil
}

// deviceListChanges returns the users whose device lists changed, or who stopped sharing a room with the user,
// since the given positions, and the key change position to continue from.
func (h *Handler) deviceListChanges(r *extensionRun, fromDeviceList, fromPDU types.StreamPosition) (changed, left []string, next types.StreamPosition, err error) {
	userID := r.device.UserID
	next = r.now.DeviceListPosition

	joinedRooms, leftRooms, joinedUsers, leftUsers, err := h.membershipChanges(r, fromPDU)
	if err != nil {
		return nil, nil, next, err
	}
	if len(joinedRooms) > 0 || len(leftRooms) > 0 {
		// Users in newly joined rooms are new to us, and users who were only in newly left rooms are gone.
		if changed, left, err = internal.TrackChangedUsers(r.ctx, h.rsAPI, userID, joinedRooms, leftRooms); err != nil {
			return nil, nil, next, fmt.Errorf("tracking changed users: %w", err)
		}
	}

	offset, toOffset := keytypes.OffsetOldest, keytypes.OffsetNewest
	if fromDeviceList > 0 {
		offset = int64(fromDeviceList)
	}
	if to := r.now.DeviceListPosition; to > 0 && to > fromDeviceList {
		toOffset = int64(to)
	}
	var keyChanges userapi.QueryKeyChangesResponse
	_ = h.userAPI.QueryKeyChanges(r.ctx, &userapi.QueryKeyChangesRequest{Offset: offset, ToOffset: toOffset}, &keyChanges)
	if keyChanges.Error != nil {
		// Membership tracking may still have found something useful, so do not fail.
		util.GetLogger(r.ctx).WithError(keyChanges.Error).Error("QueryKeyChanges failed")
		return changed, left, next, nil
	}
	if keyChanges.Offset > 0 {
		next = types.StreamPosition(keyChanges.Offset)
	}

	candidates := util.UniqueStrings(append(append(append([]string{}, keyChanges.UserIDs...), joinedUsers...), leftUsers...))
	shared, err := h.sharedUserCounts(r, candidates)
	if err != nil {
		return nil, nil, next, err
	}

	seen := make(map[string]bool, len(changed))
	for _, id := range changed {
		seen[id] = true
	}
	for _, id := range append(append([]string{}, keyChanges.UserIDs...), joinedUsers...) {
		if !seen[id] && shared[id] > 0 {
			changed = append(changed, id)
			seen[id] = true
		}
	}
	for _, id := range leftUsers {
		if shared[id] == 0 {
			left = append(left, id)
		}
	}
	return changed, util.UniqueStrings(left), next, nil
}

// membershipChanges finds the rooms the user joined or left since the position, and the users who joined or
// left rooms the user is in.
func (h *Handler) membershipChanges(r *extensionRun, from types.StreamPosition) (joinedRooms, leftRooms, joinedUsers, leftUsers []string, err error) {
	if r.now.PDUPosition <= from {
		return nil, nil, nil, nil, nil
	}
	filter := synctypes.DefaultStateFilter()
	filter.Types = &[]string{spec.MRoomMember}
	deltas, _, err := r.snapshot.GetStateDeltas(r.ctx, r.device, types.Range{From: from, To: r.now.PDUPosition}, r.device.UserID, &filter, h.rsAPI)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("loading membership changes: %w", err)
	}
	for _, delta := range deltas {
		if delta.NewlyJoined {
			joinedRooms = append(joinedRooms, delta.RoomID)
		}
		if delta.Membership == spec.Leave || delta.Membership == spec.Ban {
			leftRooms = append(leftRooms, delta.RoomID)
		}
		for _, ev := range delta.StateEvents {
			if ev.Type() != spec.MRoomMember || ev.StateKey() == nil {
				continue
			}
			target, err := h.rsAPI.QueryUserIDForSender(r.ctx, ev.RoomID(), spec.SenderID(*ev.StateKey()))
			if err != nil || target == nil || target.String() == r.device.UserID {
				continue
			}
			switch membership, _ := ev.Membership(); membership {
			case spec.Join:
				joinedUsers = append(joinedUsers, target.String())
			case spec.Leave, spec.Ban:
				leftUsers = append(leftUsers, target.String())
			}
		}
	}
	return joinedRooms, leftRooms, joinedUsers, leftUsers, nil
}

// sharedUserCounts returns, for each user, the number of rooms they share with the requester. The requester
// always counts as sharing a room with themselves, so that their own device changes are reported.
func (h *Handler) sharedUserCounts(r *extensionRun, userIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(userIDs))
	for _, id := range userIDs {
		counts[id] = 0
	}
	counts[r.device.UserID] = 1
	if len(userIDs) == 0 {
		return counts, nil
	}
	shared, err := r.snapshot.SharedUsers(r.ctx, r.device.UserID, userIDs)
	if err != nil {
		return nil, fmt.Errorf("loading shared users: %w", err)
	}
	for _, id := range shared {
		counts[id]++
	}
	return counts, nil
}
