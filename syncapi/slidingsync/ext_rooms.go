// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/syncapi/streams"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// typingSource tells who is typing in a room.
type typingSource interface {
	GetTypingUsersIfUpdatedAfter(roomID string, position int64) (users []string, updated bool)
}

// accountData returns the user's account data: global data since the connection last received it, and the
// room data of the rooms in this response since each of those rooms last received it.
func (h *Handler) accountData(r *extensionRun, ext *ExtensionsResponse) (func(), error) {
	out := &AccountDataResponse{}
	ext.AccountData = out

	state := &r.conn.ext
	to := r.now.AccountDataPosition
	scoped := r.scopedRooms(r.cfg.accountData)

	// One query per distinct starting position, which is usually just one.
	roomsByFrom := map[types.StreamPosition][]string{state.accountDataPos: nil}
	for _, roomID := range scoped {
		from := r.update.sent[roomID].accountDataPos
		roomsByFrom[from] = append(roomsByFrom[from], roomID)
	}
	filter := synctypes.EventFilter{Limit: math.MaxInt32}
	for from, roomIDs := range roomsByFrom {
		if to <= from {
			continue
		}
		changes, _, err := r.snapshot.GetAccountDataInRange(r.ctx, r.device.UserID, types.Range{From: from, To: to}, &filter)
		if err != nil {
			return nil, fmt.Errorf("loading account data: %w", err)
		}
		if from == state.accountDataPos {
			out.Global = append(out.Global, h.accountDataEvents(r.ctx, r.device.UserID, "", changes[""])...)
		}
		for _, roomID := range roomIDs {
			if events := h.accountDataEvents(r.ctx, r.device.UserID, roomID, changes[roomID]); len(events) > 0 {
				if out.Rooms == nil {
					out.Rooms = map[string][]RoomEvent{}
				}
				out.Rooms[roomID] = events
			}
		}
	}

	for _, roomID := range scoped {
		r.update.sent[roomID].accountDataPos = max(to, r.update.sent[roomID].accountDataPos)
	}
	return func() { state.accountDataPos = max(to, state.accountDataPos) }, nil
}

// accountDataEvents loads the content of the changed account data of a room, or of the user if roomID is empty.
func (h *Handler) accountDataEvents(ctx context.Context, userID, roomID string, dataTypes []string) []RoomEvent {
	sort.Strings(dataTypes)
	var events []RoomEvent
	for _, dataType := range dataTypes {
		var res userapi.QueryAccountDataResponse
		if err := h.userAPI.QueryAccountData(ctx, &userapi.QueryAccountDataRequest{UserID: userID, RoomID: roomID, DataType: dataType}, &res); err != nil {
			logrus.WithError(err).WithField("data_type", dataType).Warn("failed to load account data")
			continue
		}
		content, ok := res.GlobalAccountData[dataType]
		if roomID != "" {
			content, ok = res.RoomAccountData[roomID][dataType]
		}
		if ok {
			events = append(events, RoomEvent{Type: dataType, Content: content})
		}
	}
	return events
}

// receipts returns read receipts for the rooms in this response. A room sent in full gets all its receipts;
// any other room the ones since it last received them. Other users' private receipts are never sent.
func (h *Handler) receipts(r *extensionRun, ext *ExtensionsResponse, ignored map[string]interface{}) (func(), error) {
	out := &RoomEventsResponse{}
	ext.Receipts = out

	to := r.now.ReceiptPosition
	scoped := r.scopedRooms(r.cfg.receipts)
	roomsByFrom := map[types.StreamPosition][]string{}
	for _, roomID := range scoped {
		var from types.StreamPosition
		if !r.isInitial(roomID) {
			from = r.update.sent[roomID].receiptPos
		}
		roomsByFrom[from] = append(roomsByFrom[from], roomID)
	}

	for from, roomIDs := range roomsByFrom {
		_, receipts, err := r.snapshot.RoomReceiptsAfter(r.ctx, roomIDs, from)
		if err != nil {
			return nil, fmt.Errorf("loading receipts: %w", err)
		}
		content := map[string]map[string]streams.ReceiptMRead{}
		for _, receipt := range receipts {
			if _, isIgnored := ignored[receipt.UserID]; isIgnored {
				continue
			}
			if receipt.Type == "m.read.private" && receipt.UserID != r.device.UserID {
				continue
			}
			if content[receipt.RoomID] == nil {
				content[receipt.RoomID] = map[string]streams.ReceiptMRead{}
			}
			read, ok := content[receipt.RoomID][receipt.EventID]
			if !ok {
				read = streams.ReceiptMRead{User: map[string]streams.ReceiptTS{}}
			}
			read.User[receipt.UserID] = streams.ReceiptTS{TS: receipt.Timestamp}
			content[receipt.RoomID][receipt.EventID] = read
		}
		for roomID, byEvent := range content {
			raw, err := json.Marshal(byEvent)
			if err != nil {
				return nil, fmt.Errorf("encoding receipts of %s: %w", roomID, err)
			}
			if out.Rooms == nil {
				out.Rooms = map[string]RoomEvent{}
			}
			out.Rooms[roomID] = RoomEvent{Type: spec.MReceipt, Content: raw}
		}
	}

	for _, roomID := range scoped {
		r.update.sent[roomID].receiptPos = max(to, r.update.sent[roomID].receiptPos)
	}
	return func() {}, nil
}

// typingUsers returns who is typing in the rooms in this response. Typing is short-lived, so only a room sent in
// full gets its current state; other rooms get what changed since the last response that carried typing.
func (h *Handler) typingUsers(r *extensionRun, ext *ExtensionsResponse, ignored map[string]interface{}) (func(), error) {
	out := &RoomEventsResponse{}
	ext.Typing = out

	state := &r.conn.ext
	to := r.now.TypingPosition
	if h.typing != nil {
		for _, roomID := range r.scopedRooms(r.cfg.typing) {
			initial := r.isInitial(roomID)
			from := state.typingPos
			if initial {
				from = 0
			}
			users, updated := h.typing.GetTypingUsersIfUpdatedAfter(roomID, int64(from))
			if !updated {
				continue
			}
			typing := make([]string, 0, len(users))
			for _, user := range users {
				if _, isIgnored := ignored[user]; !isIgnored {
					typing = append(typing, user)
				}
			}
			if initial && len(typing) == 0 {
				continue
			}
			sort.Strings(typing)
			content, err := json.Marshal(map[string]any{"user_ids": typing})
			if err != nil {
				return nil, fmt.Errorf("encoding typing users of %s: %w", roomID, err)
			}
			if out.Rooms == nil {
				out.Rooms = map[string]RoomEvent{}
			}
			out.Rooms[roomID] = RoomEvent{Type: spec.MTyping, Content: content}
		}
	}
	return func() { state.typingPos = max(to, state.typingPos) }, nil
}
