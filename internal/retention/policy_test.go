package retention

import (
	"testing"
	"time"

	"github.com/element-hq/dendrite/setup/config"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{"empty", `{}`, false},
		{"max only", `{"max_lifetime": 86400000}`, false},
		{"both", `{"min_lifetime": 1000, "max_lifetime": 2000}`, false},
		{"equal", `{"min_lifetime": 1000, "max_lifetime": 1000}`, false},
		{"min above max", `{"min_lifetime": 3000, "max_lifetime": 2000}`, true},
		{"string", `{"max_lifetime": "1000"}`, true},
		{"fraction", `{"max_lifetime": 1.5}`, true},
		{"negative", `{"max_lifetime": -1}`, true},
		{"not an object", `[]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate([]byte(tc.content)); (err != nil) != tc.wantErr {
				t.Errorf("Validate(%s) = %v, want error: %v", tc.content, err, tc.wantErr)
			}
		})
	}
}

func TestFromContent(t *testing.T) {
	got := FromContent([]byte(`{"min_lifetime": 1000, "max_lifetime": 9223372036854775807}`))
	if got.MinLifetime != time.Second {
		t.Errorf("min = %v", got.MinLifetime)
	}
	if got.MaxLifetime <= 0 {
		t.Errorf("a huge lifetime must not overflow, got %v", got.MaxLifetime)
	}
	if got := FromContent([]byte(`nonsense`)); got != (Policy{}) {
		t.Errorf("invalid content gives no limits, got %+v", got)
	}
}

func TestEffective(t *testing.T) {
	day := 24 * time.Hour
	cfg := &config.MessageRetention{
		DefaultPolicy:      config.RetentionPolicy{MaxLifetime: 30 * day},
		AllowedLifetimeMin: day,
		AllowedLifetimeMax: 365 * day,
	}
	for _, tc := range []struct {
		name string
		room *Policy
		want Policy
	}{
		{"no event uses the default", nil, Policy{MaxLifetime: 30 * day}},
		{"inside the range", &Policy{MaxLifetime: 7 * day}, Policy{MaxLifetime: 7 * day}},
		{"too short is raised", &Policy{MinLifetime: time.Hour, MaxLifetime: time.Hour}, Policy{MinLifetime: day, MaxLifetime: day}},
		{"too long is lowered", &Policy{MaxLifetime: 1000 * day}, Policy{MaxLifetime: 365 * day}},
		{"an event without lifetimes keeps everything", &Policy{}, Policy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(cfg, tc.room); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
