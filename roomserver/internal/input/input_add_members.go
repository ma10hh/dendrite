// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/state"
	"github.com/element-hq/dendrite/roomserver/storage/shared"
	"github.com/element-hq/dendrite/roomserver/types"
)

// addMembers adds the memberships that a room joined with partial state was missing to its state. No event is
// involved: the memberships were already stored as outliers, and the state of the room, as well as the state before
// each of its latest events (which new events start from), gets the ones it does not have. A membership that the state
// does have is left alone, because the events that came after the join are more recent than what was fetched.
func (r *Inputer) addMembers(ctx context.Context, input *api.InputRoomEvent) (err error) {
	roomInfo, err := r.DB.RoomInfo(ctx, input.Event.RoomID().String())
	if err != nil {
		return fmt.Errorf("r.DB.RoomInfo: %w", err)
	}
	if roomInfo == nil {
		return fmt.Errorf("room %s does not exist", input.Event.RoomID().String())
	}
	partial, err := r.DB.PartialStateOfRoom(ctx, roomInfo.RoomNID)
	if err != nil {
		return fmt.Errorf("r.DB.PartialStateOfRoom: %w", err)
	}
	if partial == nil {
		return nil // the state is whole already
	}

	// Rejected events are left out by the lookup.
	entries, err := r.DB.StateEntriesForEventIDs(ctx, input.MemberEventIDs, true)
	if err != nil {
		return fmt.Errorf("r.DB.StateEntriesForEventIDs: %w", err)
	}
	var members []types.StateEntry
	for _, entry := range types.DeduplicateStateEntries(entries) {
		if entry.EventTypeNID == types.MRoomMemberNID {
			members = append(members, entry)
		}
	}

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	newCurrent, added, err := r.snapshotWithMembers(ctx, updater, roomInfo, updater.CurrentStateSnapshotNID(), members)
	if err != nil {
		return fmt.Errorf("current state: %w", err)
	}

	latest := updater.LatestEvents()
	for i := range latest {
		before, _, serr := r.snapshotWithMembers(ctx, updater, roomInfo, latest[i].BeforeStateSnapshotNID, members)
		if serr != nil {
			return fmt.Errorf("state before %s: %w", latest[i].EventID, serr)
		}
		if before != latest[i].BeforeStateSnapshotNID {
			if err = updater.SetState(ctx, latest[i].EventNID, before); err != nil {
				return fmt.Errorf("updater.SetState: %w", err)
			}
			latest[i].BeforeStateSnapshotNID = before
		}
	}

	var updates []api.OutputEvent
	if len(added) > 0 {
		lastSent, nerr := r.DB.EventNIDs(ctx, []string{updater.LastEventIDSent()})
		if nerr != nil || len(lastSent) != 1 {
			return fmt.Errorf("the last event sent is not known: %v", nerr)
		}
		if err = updater.SetLatestEvents(roomInfo.RoomNID, latest, lastSent[updater.LastEventIDSent()].EventNID, newCurrent); err != nil {
			return fmt.Errorf("updater.SetLatestEvents: %w", err)
		}
		if updates, err = r.updateMemberships(ctx, updater, nil, added); err != nil {
			return fmt.Errorf("r.updateMemberships: %w", err)
		}

		nids := make([]types.EventNID, len(added))
		for i := range added {
			nids[i] = added[i].EventNID
		}
		events, eerr := updater.Events(ctx, roomInfo.RoomVersion, nids)
		if eerr != nil {
			return fmt.Errorf("updater.Events: %w", eerr)
		}
		out := &api.OutputMembersAdded{
			RoomID:      input.Event.RoomID().String(),
			JoinEventID: partial.JoinEventID,
			Servers:     partial.Servers,
		}
		for _, ev := range events {
			out.Members = append(out.Members, &types.HeaderedEvent{PDU: ev.PDU})
		}
		updates = append(updates, api.OutputEvent{Type: api.OutputTypeMembersAdded, MembersAdded: out})
	} else {
		// Nothing was missing, but the consumers still have to drop the servers that stood in for the members.
		updates = append(updates, api.OutputEvent{
			Type: api.OutputTypeMembersAdded,
			MembersAdded: &api.OutputMembersAdded{
				RoomID: input.Event.RoomID().String(), JoinEventID: partial.JoinEventID, Servers: partial.Servers,
			},
		})
	}
	if err = r.OutputProducer.ProduceRoomEvents(input.Event.RoomID().String(), updates); err != nil {
		return fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
	}
	succeeded = true
	return nil
}

// snapshotWithMembers returns the snapshot that has the memberships of the snapshot and the ones of members that it
// has no membership of the user for, and the entries that were added. If there are none, it is the snapshot itself.
func (r *Inputer) snapshotWithMembers(
	ctx context.Context, updater *shared.RoomUpdater, roomInfo *types.RoomInfo,
	snapshot types.StateSnapshotNID, members []types.StateEntry,
) (types.StateSnapshotNID, []types.StateEntry, error) {
	resolver := state.NewStateResolution(updater, roomInfo, r.Queryer)
	have, err := resolver.LoadStateAtSnapshot(ctx, snapshot)
	if err != nil {
		return 0, nil, err
	}
	known := make(map[types.StateKeyTuple]struct{}, len(have))
	for _, entry := range have {
		if entry.EventTypeNID == types.MRoomMemberNID {
			known[entry.StateKeyTuple] = struct{}{}
		}
	}
	var missing []types.StateEntry
	for _, entry := range members {
		if _, ok := known[entry.StateKeyTuple]; !ok {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return snapshot, nil, nil
	}

	lists, err := updater.StateBlockNIDs(ctx, []types.StateSnapshotNID{snapshot})
	if err != nil {
		return 0, nil, err
	}
	blocks := lists[0].StateBlockNIDs
	var added types.StateSnapshotNID
	// The memberships go into a block of their own on top of the blocks of the snapshot, unless there are too many.
	if len(blocks) < maxStateBlocks {
		added, err = updater.AddState(ctx, roomInfo.RoomNID, blocks, missing)
	} else {
		added, err = updater.AddState(ctx, roomInfo.RoomNID, nil, append(have, missing...))
	}
	return added, missing, err
}

// maxStateBlocks is the number of blocks of a snapshot above which it is written out in full, as in the state package.
const maxStateBlocks = 64
