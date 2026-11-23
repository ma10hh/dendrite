package process

import (
	"context"
	"sort"
	"sync"

	"github.com/getsentry/sentry-go"
	"github.com/sirupsen/logrus"
)

type ProcessContext struct {
	mu        sync.RWMutex
	wg        sync.WaitGroup                         // used to wait for components to shutdown
	ctx       context.Context                        // cancelled when Stop is called
	shutdown  context.CancelFunc                     // shut down Dendrite
	degraded  map[string]struct{}                    // reasons why the process is degraded
	readiness map[string]func(context.Context) error // checks for /monitor/ready
}

func NewProcessContext() *ProcessContext {
	ctx, shutdown := context.WithCancel(context.Background())
	return &ProcessContext{
		ctx:      ctx,
		shutdown: shutdown,
		wg:       sync.WaitGroup{},
		degraded: map[string]struct{}{},
	}
}

func (b *ProcessContext) Context() context.Context {
	return context.WithValue(b.ctx, "scope", "process") // nolint:staticcheck
}

func (b *ProcessContext) ComponentStarted() {
	b.wg.Add(1)
}

func (b *ProcessContext) ComponentFinished() {
	b.wg.Done()
}

func (b *ProcessContext) ShutdownDendrite() {
	b.shutdown()
}

func (b *ProcessContext) WaitForShutdown() <-chan struct{} {
	return b.ctx.Done()
}

func (b *ProcessContext) WaitForComponentsToFinish() {
	b.wg.Wait()
}

func (b *ProcessContext) Degraded(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.degraded[err.Error()]; !ok {
		logrus.WithError(err).Warn("Dendrite has entered a degraded state")
		sentry.CaptureException(err)
		b.degraded[err.Error()] = struct{}{}
	}
}

func (b *ProcessContext) IsDegraded() (bool, []string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.degraded) == 0 {
		return false, nil
	}
	reasons := make([]string, 0, len(b.degraded))
	for reason := range b.degraded {
		reasons = append(reasons, reason)
	}
	return true, reasons
}

// RegisterReadiness adds a check that must pass before the process counts as ready to serve (for example a database
// that answers). The check should return quickly and honour the context.
func (b *ProcessContext) RegisterReadiness(name string, check func(context.Context) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readiness == nil {
		b.readiness = map[string]func(context.Context) error{}
	}
	b.readiness[name] = check
}

// Readiness runs the registered checks at the same time and returns the failures as "name: error", and whether the
// process is degraded for another reason.
func (b *ProcessContext) Readiness(ctx context.Context) (failures []string) {
	b.mu.RLock()
	checks := make(map[string]func(context.Context) error, len(b.readiness))
	for name, check := range b.readiness {
		checks[name] = check
	}
	b.mu.RUnlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, check := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := check(ctx); err != nil {
				mu.Lock()
				failures = append(failures, name+": "+err.Error())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(failures)
	return failures
}
