package consumers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rsapi "github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/element-hq/dendrite/userapi/producers"
)

type fakeKeyChangeDB struct{ next int64 }

func (f *fakeKeyChangeDB) StoreKeyChange(context.Context, string) (int64, error) {
	f.next++
	return f.next, nil
}

type fakePublisher struct{ published []api.DeviceMessage }

func (f *fakePublisher) PublishMsg(m *nats.Msg, _ ...nats.PubOpt) (*nats.PubAck, error) {
	var msg api.DeviceMessage
	if err := json.Unmarshal(m.Data, &msg); err != nil {
		return nil, err
	}
	f.published = append(f.published, msg)
	return &nats.PubAck{}, nil
}

func TestMembersAddedReportsRemoteMembers(t *testing.T) {
	creator := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:r", test.PrivateKeyB))
	joined := test.NewUser(t, test.WithSigningServer("second.example", "ed25519:s", test.PrivateKeyA))
	left := test.NewUser(t, test.WithSigningServer("third.example", "ed25519:t", test.PrivateKeyA))
	local := test.NewUser(t)
	room := test.NewRoom(t, creator)
	member := func(u *test.User, membership string) *rstypes.HeaderedEvent {
		return room.CreateAndInsert(t, u, spec.MRoomMember, map[string]string{"membership": membership}, test.WithStateKey(u.ID))
	}
	msg := rsapi.OutputMembersAdded{
		RoomID:  room.ID,
		Members: []*rstypes.HeaderedEvent{member(joined, "join"), member(left, "leave"), member(local, "join")},
	}

	publisher := &fakePublisher{}
	global := &config.Global{}
	global.ServerName = "test"
	s := &OutputRoomEventConsumer{
		cfg:        &config.UserAPI{Matrix: global},
		keyChanges: &producers.KeyChange{Topic: "keys", JetStream: publisher, DB: &fakeKeyChangeDB{}},
	}
	require.True(t, s.onMembersAdded(msg))

	require.Len(t, publisher.published, 1, "only the member of another server who is joined has a change to report")
	assert.Equal(t, joined.ID, publisher.published[0].UserID)
	assert.Equal(t, api.TypeDeviceKeyUpdate, publisher.published[0].Type)
	assert.NotZero(t, publisher.published[0].DeviceChangeID)
}
