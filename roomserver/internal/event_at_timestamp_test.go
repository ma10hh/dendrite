package internal

import (
	"context"
	"errors"
	"testing"

	"github.com/element-hq/dendrite/roomserver/api"
)

func TestQueryEventAtTimestamp(t *testing.T) {
	r := &RoomserverInternalAPI{}
	if _, _, err := r.QueryEventAtTimestamp(context.Background(), "!r:x", 1, true); !errors.Is(err, api.ErrNoEventAtTimestamp) {
		t.Fatalf("without a provider: got %v, want ErrNoEventAtTimestamp", err)
	}
	r.SetEventAtTimestamp(func(_ context.Context, roomID string, ts int64, forward bool) (string, int64, error) {
		if roomID != "!r:x" || ts != 5 || !forward {
			t.Errorf("arguments not passed on: %q %d %v", roomID, ts, forward)
		}
		return "$e", 7, nil
	})
	id, ts, err := r.QueryEventAtTimestamp(context.Background(), "!r:x", 5, true)
	if err != nil || id != "$e" || ts != 7 {
		t.Errorf("got %q %d %v", id, ts, err)
	}
}
