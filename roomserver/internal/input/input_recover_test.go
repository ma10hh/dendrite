package input

import (
	"context"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"

	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/test"
)

// An Inputer that has nothing to work with panics on the first event: the worker must get an error, not a crash.
func TestProcessRoomEventRecoversFromPanics(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	r := &Inputer{}
	err := r.processRoomEventRecovering(context.Background(), "", &api.InputRoomEvent{
		Kind: api.KindNew, Event: room.Events()[0],
	}, sentry.CurrentHub())
	if err == nil || !strings.Contains(err.Error(), "panic while processing the event") {
		t.Fatalf("got %v, want the error of a recovered panic", err)
	}
}
