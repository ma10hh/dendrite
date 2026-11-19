// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import "github.com/element-hq/dendrite/syncapi/types"

type roomKind int

const (
	kindJoined roomKind = iota
	kindInvited
	kindKnocked
	kindLeft
)

func (e roomEntry) kind() roomKind {
	switch {
	case e.invited:
		return kindInvited
	case e.knocked:
		return kindKnocked
	case e.left:
		return kindLeft
	}
	return kindJoined
}

// roomSent records what a connection was last told about a room, so that later responses only carry changes.
//
// Rooms that are not joined are sent once; they are sent again when the event that put the user in that
// state (token) changes. Joined rooms keep enough to tell what changed since.
type roomSent struct {
	kind roomKind
	// token identifies the membership event behind an invite, knock or leave.
	token string

	// pos is the position of the room's latest event at the time it was last sent: everything up to it was handled.
	pos types.StreamPosition
	// timelineLimit is the timeline limit the room was last sent with.
	timelineLimit int
	// state holds the event ID last sent for each required_state entry.
	state map[stateID]string

	name                      string
	avatar                    string
	isDM                      bool
	joined, invited           int
	notifications, highlights int
	heroes                    []string
	bump                      int64

	// accountDataPos and receiptPos are the positions up to which the room's account data and receipts were sent.
	// Zero means never, which sends everything.
	accountDataPos, receiptPos types.StreamPosition
}

// extensionState is what the extensions remember about the connection. Positions only move once a response
// that carried the extension is on its way, so a dropped response loses nothing.
type extensionState struct {
	// e2eeStarted is set once a response carried the e2ee extension; device list changes are relative to it.
	e2eeStarted bool
	// deviceListPos and pduPos are the key change and room event positions device list changes are computed from.
	deviceListPos, pduPos types.StreamPosition
	// accountDataPos is the position up to which global account data was sent.
	accountDataPos types.StreamPosition
	// typingPos is the typing position of the last response that carried the typing extension.
	typingPos types.StreamPosition
}
