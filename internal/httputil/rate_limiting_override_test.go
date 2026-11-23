package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

func TestRateLimitOverride(t *testing.T) {
	l := NewRateLimits(&config.RateLimiting{Enabled: true, Threshold: 2, CooloffMS: 60000})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	allowed := func(d *userapi.Device, n int) int {
		count := 0
		for i := 0; i < n; i++ {
			if l.Limit(req, d) == nil {
				count++
			}
		}
		return count
	}

	if got := allowed(&userapi.Device{UserID: "@plain:x", ID: "A"}, 10); got != 2 {
		t.Errorf("the limit of the server lets %d through, want 2", got)
	}
	if got := allowed(&userapi.Device{UserID: "@bursty:x", ID: "A", RateLimit: &userapi.RateLimitOverride{MessagesPerSecond: 1, BurstCount: 5}}, 10); got != 5 {
		t.Errorf("an override with a burst of 5 lets %d through", got)
	}
	if got := allowed(&userapi.Device{UserID: "@free:x", ID: "A", RateLimit: &userapi.RateLimitOverride{}}, 50); got != 50 {
		t.Errorf("an override of 0 and 0 means no limit, %d went through", got)
	}
	// Changing the override starts again with new slots.
	if got := allowed(&userapi.Device{UserID: "@bursty:x", ID: "A", RateLimit: &userapi.RateLimitOverride{MessagesPerSecond: 1, BurstCount: 7}}, 10); got != 7 {
		t.Errorf("a changed override lets %d through, want 7", got)
	}
	if got := allowed(&userapi.Device{UserID: "@admin:x", ID: "A", AccountType: userapi.AccountTypeAdmin, RateLimit: &userapi.RateLimitOverride{MessagesPerSecond: 1, BurstCount: 1}}, 10); got != 10 {
		t.Errorf("administrators are never limited, %d went through", got)
	}
	res := l.Limit(req, &userapi.Device{UserID: "@bursty:x", ID: "A", RateLimit: &userapi.RateLimitOverride{MessagesPerSecond: 0.5, BurstCount: 1}})
	if res != nil {
		t.Fatalf("the first request of a new limit passes")
	}
	res = l.Limit(req, &userapi.Device{UserID: "@bursty:x", ID: "A", RateLimit: &userapi.RateLimitOverride{MessagesPerSecond: 0.5, BurstCount: 1}})
	if res == nil || res.Code != http.StatusTooManyRequests {
		t.Fatalf("the second is refused, got %+v", res)
	}
}
