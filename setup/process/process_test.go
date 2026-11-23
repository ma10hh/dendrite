package process

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDegradedDoesNotPanic(t *testing.T) {
	p := NewProcessContext()
	p.Degraded(errors.New("no NATS"))
	if degraded, reasons := p.IsDegraded(); !degraded || len(reasons) != 1 {
		t.Errorf("degraded=%v reasons=%v", degraded, reasons)
	}
}

func TestReadiness(t *testing.T) {
	p := NewProcessContext()
	if f := p.Readiness(context.Background()); len(f) != 0 {
		t.Fatalf("no checks, no failures, got %v", f)
	}
	p.RegisterReadiness("ok", func(context.Context) error { return nil })
	p.RegisterReadiness("b", func(context.Context) error { return errors.New("down") })
	p.RegisterReadiness("a", func(context.Context) error { return errors.New("slow") })
	f := p.Readiness(context.Background())
	if len(f) != 2 || f[0] != "a: slow" || f[1] != "b: down" {
		t.Errorf("failures = %v, want the two failing ones, sorted", f)
	}

	// The checks run at the same time: two that wait 200ms take about that long, not twice.
	q := NewProcessContext()
	for _, n := range []string{"x", "y"} {
		q.RegisterReadiness(n, func(ctx context.Context) error {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
			}
			return nil
		})
	}
	start := time.Now()
	q.Readiness(context.Background())
	if d := time.Since(start); d > 350*time.Millisecond {
		t.Errorf("took %v, the checks should run together", d)
	}
}
