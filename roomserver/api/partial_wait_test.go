package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type partialThenWhole struct{ calls, partialFor int }

func (p *partialThenWhole) QueryMembershipForUser(_ context.Context, _ *QueryMembershipForUserRequest, res *QueryMembershipForUserResponse) error {
	p.calls++
	res.RoomExists = true
	res.PartialState = p.calls <= p.partialFor
	return nil
}

func TestQueryMembershipWhenStateIsWhole(t *testing.T) {
	t.Run("whole at once", func(t *testing.T) {
		p := &partialThenWhole{}
		res := QueryMembershipForUserResponse{}
		ok, err := QueryMembershipWhenStateIsWhole(context.Background(), p, &QueryMembershipForUserRequest{}, &res)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, 1, p.calls)
	})
	t.Run("waits until it is whole", func(t *testing.T) {
		p := &partialThenWhole{partialFor: 2}
		res := QueryMembershipForUserResponse{}
		ok, err := QueryMembershipWhenStateIsWhole(context.Background(), p, &QueryMembershipForUserRequest{}, &res)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, 3, p.calls)
		assert.False(t, res.PartialState)
	})
	t.Run("stops when the request is cancelled", func(t *testing.T) {
		p := &partialThenWhole{partialFor: 1 << 30}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := QueryMembershipForUserResponse{}
		ok, err := QueryMembershipWhenStateIsWhole(ctx, p, &QueryMembershipForUserRequest{}, &res)
		assert.False(t, ok)
		assert.Error(t, err)
	})
}
