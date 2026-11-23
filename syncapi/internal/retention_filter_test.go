package internal

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
)

type fakeState struct {
	policy *types.HeaderedEvent
	err    error
}

func (f fakeState) GetStateEvent(context.Context, string, string, string) (*types.HeaderedEvent, error) {
	return f.policy, f.err
}

func TestHideExpiredEvents(t *testing.T) {
	now := time.Now()
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	policy := room.CreateAndInsert(t, alice, "m.room.retention", map[string]any{"max_lifetime": (24 * time.Hour).Milliseconds()}, test.WithStateKey(""))
	oldMsg := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "old"}, test.WithTimestamp(now.Add(-48*time.Hour)))
	oldTopic := room.CreateAndInsert(t, alice, "m.room.topic", map[string]any{"topic": "old"}, test.WithStateKey(""), test.WithTimestamp(now.Add(-48*time.Hour)))
	newMsg := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "new"}, test.WithTimestamp(now.Add(-time.Hour)))
	events := []*types.HeaderedEvent{oldMsg, oldTopic, newMsg}

	defer SetRetention(nil)
	ids := func(evs []*types.HeaderedEvent) (out []string) {
		for _, e := range evs {
			out = append(out, e.EventID())
		}
		return
	}

	// Retention off: nothing is hidden, and nothing is read.
	SetRetention(nil)
	got, err := hideExpiredEvents(context.Background(), fakeState{err: assert.AnError}, events, now)
	require.NoError(t, err)
	assert.Equal(t, ids(events), ids(got))
	SetRetention(&config.MessageRetention{})
	got, err = hideExpiredEvents(context.Background(), fakeState{err: assert.AnError}, events, now)
	require.NoError(t, err)
	assert.Equal(t, ids(events), ids(got))

	// The policy of the room: the old message goes, the old state event and the new message stay.
	SetRetention(&config.MessageRetention{Enabled: true})
	got, err = hideExpiredEvents(context.Background(), fakeState{policy: policy}, events, now)
	require.NoError(t, err)
	assert.Equal(t, []string{oldTopic.EventID(), newMsg.EventID()}, ids(got))

	// No policy event and no default: nothing is hidden.
	got, err = hideExpiredEvents(context.Background(), fakeState{}, events, now)
	require.NoError(t, err)
	assert.Equal(t, ids(events), ids(got))

	// The default policy applies to a room without one, and the limits of the server move the lifetime of a room.
	SetRetention(&config.MessageRetention{Enabled: true, DefaultPolicy: config.RetentionPolicy{MaxLifetime: 30 * time.Minute}})
	got, err = hideExpiredEvents(context.Background(), fakeState{}, events, now)
	require.NoError(t, err)
	assert.Equal(t, []string{oldTopic.EventID()}, ids(got))
	SetRetention(&config.MessageRetention{Enabled: true, AllowedLifetimeMin: 72 * time.Hour})
	got, err = hideExpiredEvents(context.Background(), fakeState{policy: policy}, events, now)
	require.NoError(t, err)
	assert.Equal(t, ids(events), ids(got), "a day is raised to three days")

	// A failing read is an error, not a reason to show or hide anything.
	_, err = hideExpiredEvents(context.Background(), fakeState{err: assert.AnError}, events, now)
	assert.Error(t, err)
}
