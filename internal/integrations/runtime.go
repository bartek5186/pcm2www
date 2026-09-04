package integrations

import (
	"context"
	"errors"
	"sync"

	"gorm.io/gorm"
)

type runtimeContextKey struct{}

// Runtime contains dependencies and readiness signals shared by integrations
// started as one Syncer generation.
type Runtime struct {
	DB             *gorm.DB
	wooCacheReady  chan struct{}
	wooCacheReadyO sync.Once
}

func NewRuntime(gdb *gorm.DB, waitsForWooCache bool) *Runtime {
	r := &Runtime{DB: gdb, wooCacheReady: make(chan struct{})}
	if !waitsForWooCache {
		r.MarkWooCacheReady()
	}
	return r
}

func WithRuntime(ctx context.Context, runtime *Runtime) context.Context {
	return context.WithValue(ctx, runtimeContextKey{}, runtime)
}

func RuntimeFromContext(ctx context.Context) (*Runtime, error) {
	runtime, _ := ctx.Value(runtimeContextKey{}).(*Runtime)
	if runtime == nil {
		return nil, errors.New("missing integration runtime in context")
	}
	return runtime, nil
}

func DBFromContext(ctx context.Context) (*gorm.DB, error) {
	runtime, err := RuntimeFromContext(ctx)
	if err != nil || runtime.DB == nil {
		return nil, errors.New("missing *gorm.DB in integration runtime")
	}
	return runtime.DB, nil
}

func (r *Runtime) MarkWooCacheReady() {
	if r == nil {
		return
	}
	r.wooCacheReadyO.Do(func() { close(r.wooCacheReady) })
}

func (r *Runtime) WooCacheReady() <-chan struct{} {
	if r == nil {
		ready := make(chan struct{})
		close(ready)
		return ready
	}
	return r.wooCacheReady
}

func (r *Runtime) IsWooCacheReady() bool {
	select {
	case <-r.WooCacheReady():
		return true
	default:
		return false
	}
}
