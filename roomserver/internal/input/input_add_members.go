// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

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
func (r *Inputer) addMembers(ctx context.Context, input *api.InputRoomEvent) error {
	roomNID, err := r.addMembersInTransaction(ctx, input)
	if err != nil || roomNID == 0 {
		return err
	}
	// The notes go once the changes are stored, so a failure before that is tried again.
	if err = r.DB.ForgetPartialStateEvents(ctx, roomNID); err != nil {
		logrus.WithError(err).Warn("failed to forget the events of the partial state")
	}
	return nil
}

func (r *Inputer) addMembersInTransaction(ctx context.Context, input *api.InputRoomEvent) (roomNID types.RoomNID, err error) {
	roomInfo, err := r.DB.RoomInfo(ctx, input.Event.RoomID().String())
	if err != nil {
		return 0, fmt.Errorf("r.DB.RoomInfo: %w", err)
	}
	if roomInfo == nil {
		return 0, fmt.Errorf("room %s does not exist", input.Event.RoomID().String())
	}
	partial, err := r.DB.PartialStateOfRoom(ctx, roomInfo.RoomNID)
	if err != nil {
		return 0, fmt.Errorf("r.DB.PartialStateOfRoom: %w", err)
	}
	if partial == nil {
		return 0, nil // the state is whole already
	}

	// Rejected events are left out by the lookup.
	entries, err := r.DB.StateEntriesForEventIDs(ctx, input.MemberEventIDs, true)
	if err != nil {
		return 0, fmt.Errorf("r.DB.StateEntriesForEventIDs: %w", err)
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
		return 0, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	newCurrent, added, err := r.snapshotWithMembers(ctx, updater, roomInfo, updater.CurrentStateSnapshotNID(), members)
	if err != nil {
		return 0, fmt.Errorf("current state: %w", err)
	}

	latest := updater.LatestEvents()
	for i := range latest {
		before, _, serr := r.snapshotWithMembers(ctx, updater, roomInfo, latest[i].BeforeStateSnapshotNID, members)
		if serr != nil {
			return 0, fmt.Errorf("state before %s: %w", latest[i].EventID, serr)
		}
		if before != latest[i].BeforeStateSnapshotNID {
			if err = updater.SetState(ctx, latest[i].EventNID, before); err != nil {
				return 0, fmt.Errorf("updater.SetState: %w", err)
			}
			latest[i].BeforeStateSnapshotNID = before
		}
	}

	recorded, err := updater.PartialStateEvents(ctx, roomInfo.RoomNID)
	if err != nil {
		return 0, fmt.Errorf("updater.PartialStateEvents: %w", err)
	}
	// The join event has the state the other server gave, without the memberships, as the state before it. Servers to
	// backfill from are taken from it, so it gets them like the events after it.
	if joinNIDs, jerr := r.DB.EventNIDs(ctx, []string{partial.JoinEventID}); jerr == nil {
		if join, ok := joinNIDs[partial.JoinEventID]; ok && join.EventNID != 0 {
			known := false
			for _, e := range recorded {
				known = known || e.EventNID == join.EventNID
			}
			if !known {
				recorded = append([]types.PartialStateEvent{{EventNID: join.EventNID}}, recorded...)
			}
		}
	}
	if err = r.recheckPartialStateEvents(ctx, updater, roomInfo, members, recorded); err != nil {
		return 0, fmt.Errorf("rechecking the events of the partial state: %w", err)
	}

	var updates []api.OutputEvent
	if len(added) > 0 {
		lastSent, nerr := r.DB.EventNIDs(ctx, []string{updater.LastEventIDSent()})
		if nerr != nil || len(lastSent) != 1 {
			return 0, fmt.Errorf("the last event sent is not known: %v", nerr)
		}
		if err = updater.SetLatestEvents(roomInfo.RoomNID, latest, lastSent[updater.LastEventIDSent()].EventNID, newCurrent); err != nil {
			return 0, fmt.Errorf("updater.SetLatestEvents: %w", err)
		}
		if updates, err = r.updateMemberships(ctx, updater, nil, added); err != nil {
			return 0, fmt.Errorf("r.updateMemberships: %w", err)
		}

		nids := make([]types.EventNID, len(added))
		for i := range added {
			nids[i] = added[i].EventNID
		}
		events, eerr := updater.Events(ctx, roomInfo.RoomVersion, nids)
		if eerr != nil {
			return 0, fmt.Errorf("updater.Events: %w", eerr)
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
		return 0, fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
	}
	succeeded = true
	return roomInfo.RoomNID, nil
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

// allowedWithAuthMemberships checks the event against the state before it, plus the memberships among its auth events
// that the state does not have: the ones that a room with partial state is missing.
func (r *Inputer) allowedWithAuthMemberships(
	ctx context.Context, roomInfo *types.RoomInfo, event gomatrixserverlib.PDU,
	stateBefore []gomatrixserverlib.PDU, userIDForSender spec.UserIDForSender,
) bool {
	have := make(map[string]struct{}, len(stateBefore))
	for _, ev := range stateBefore {
		if ev.Type() == spec.MRoomMember && ev.StateKey() != nil {
			have[*ev.StateKey()] = struct{}{}
		}
	}
	authEvents, err := r.DB.EventsFromIDs(ctx, roomInfo, event.AuthEventIDs())
	if err != nil {
		return false
	}
	augmented := append([]gomatrixserverlib.PDU{}, stateBefore...)
	for _, auth := range authEvents {
		if auth.PDU == nil || auth.PDU.Type() != spec.MRoomMember || auth.PDU.StateKey() == nil {
			continue
		}
		if rejected, rerr := r.DB.IsEventRejected(ctx, roomInfo.RoomNID, auth.EventID()); rerr != nil || rejected {
			continue
		}
		if _, ok := have[*auth.PDU.StateKey()]; !ok {
			augmented = append(augmented, auth.PDU)
		}
	}
	provider, err := gomatrixserverlib.NewAuthEvents(gomatrixserverlib.ToPDUs(augmented))
	if err != nil {
		return false
	}
	return gomatrixserverlib.Allowed(event, provider, userIDForSender) == nil
}

// recheckPartialStateEvents gives the state before each event that was stored while the room had partial state the
// memberships it lacked, and checks the events that were accepted without a check against the state before them.
// An event that fails is marked as rejected: it stays in the timeline clients have seen it in, but is no longer part of
// the state, and is not sent on.
func (r *Inputer) recheckPartialStateEvents(
	ctx context.Context, updater *shared.RoomUpdater, roomInfo *types.RoomInfo, members []types.StateEntry,
	recorded []types.PartialStateEvent,
) error {
	if len(recorded) == 0 {
		return nil
	}
	nids := make([]types.EventNID, len(recorded))
	recheck := make(map[types.EventNID]bool, len(recorded))
	for i, e := range recorded {
		nids[i] = e.EventNID
		recheck[e.EventNID] = e.Recheck
	}
	events, err := updater.Events(ctx, roomInfo.RoomVersion, nids)
	if err != nil {
		return fmt.Errorf("updater.Events: %w", err)
	}
	ids := make([]string, len(events))
	for i := range events {
		ids[i] = events[i].EventID()
	}
	states, err := updater.StateAtEventIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("updater.StateAtEventIDs: %w", err)
	}
	stateOf := make(map[types.EventNID]types.StateAtEvent, len(states))
	for _, s := range states {
		stateOf[s.EventNID] = s
	}

	resolver := state.NewStateResolution(updater, roomInfo, r.Queryer)
	userIDForSender := func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return r.Queryer.QueryUserIDForSender(ctx, roomID, senderID)
	}
	filled := map[types.StateSnapshotNID]types.StateSnapshotNID{}
	for _, ev := range events {
		at, ok := stateOf[ev.EventNID]
		if !ok || at.BeforeStateSnapshotNID == 0 {
			continue
		}
		before, done := filled[at.BeforeStateSnapshotNID]
		if !done {
			if before, _, err = r.snapshotWithMembers(ctx, updater, roomInfo, at.BeforeStateSnapshotNID, members); err != nil {
				return fmt.Errorf("state before %s: %w", ev.EventID(), err)
			}
			filled[at.BeforeStateSnapshotNID] = before
		}
		if before != at.BeforeStateSnapshotNID {
			if err = updater.SetState(ctx, ev.EventNID, before); err != nil {
				return fmt.Errorf("updater.SetState: %w", err)
			}
		}
		if !recheck[ev.EventNID] {
			continue
		}

		tuples := gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.PDU{ev.PDU}).Tuples()
		entries, err := resolver.LoadStateAtSnapshotForStringTuples(ctx, before, tuples)
		if err != nil {
			return fmt.Errorf("state needed for %s: %w", ev.EventID(), err)
		}
		entryNIDs := make([]types.EventNID, len(entries))
		for i := range entries {
			entryNIDs[i] = entries[i].EventNID
		}
		stateEvents, err := updater.Events(ctx, roomInfo.RoomVersion, entryNIDs)
		if err != nil {
			return fmt.Errorf("updater.Events: %w", err)
		}
		pdus := make([]gomatrixserverlib.PDU, len(stateEvents))
		for i := range stateEvents {
			pdus[i] = stateEvents[i].PDU
		}
		provider, err := gomatrixserverlib.NewAuthEvents(pdus)
		if err != nil {
			return err
		}
		if aerr := gomatrixserverlib.Allowed(ev.PDU, provider, userIDForSender); aerr != nil {
			logrus.WithError(aerr).WithFields(logrus.Fields{
				"room_id": ev.RoomID().String(), "event_id": ev.EventID(), "state_event": ev.StateKey() != nil,
			}).Warn("an event accepted while the room had partial state is not allowed by the full state, rejecting it")
			if err = updater.SetEventRejected(ctx, ev.EventNID, true); err != nil {
				return fmt.Errorf("updater.SetEventRejected: %w", err)
			}
		}
	}
	return nil
}
