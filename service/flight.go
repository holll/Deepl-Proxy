package service

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Flight coalesces calls without making one request the owner of shared work.
// Each waiter cancels independently; work is canceled when all waiters leave,
// the application stops, or the 60s total budget expires. At most 128 shared
// workers run at once; saturation is rejected rather than queued in goroutines.
const flightTimeout = 60 * time.Second
const maxFlights = 128

type Flight[T any] struct {
	mu        sync.Mutex
	m         map[string]*flightCall[T]
	lifecycle context.Context
	slots     chan struct{}
}

type flightCall[T any] struct {
	done       chan struct{}
	val        T
	err        error
	panicValue any
	cancel     context.CancelFunc
	waiters    int
}

func NewFlight[T any](lifecycle ...context.Context) *Flight[T] {
	ctx := context.Background()
	if len(lifecycle) > 0 {
		ctx = lifecycle[0]
	}
	return &Flight[T]{m: make(map[string]*flightCall[T]), lifecycle: ctx, slots: make(chan struct{}, maxFlights)}
}

func (f *Flight[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	f.mu.Lock()
	if f.m == nil {
		f.m = make(map[string]*flightCall[T])
	}
	if f.lifecycle == nil {
		f.lifecycle = context.Background()
	}
	if f.slots == nil {
		f.slots = make(chan struct{}, maxFlights)
	}
	if err := f.lifecycle.Err(); err != nil {
		f.mu.Unlock()
		return zero, false, err
	}
	c, shared := f.m[key]
	if !shared {
		select {
		case f.slots <- struct{}{}:
		default:
			f.mu.Unlock()
			return zero, false, fmt.Errorf("flight: capacity exhausted")
		}
		workCtx, cancel := context.WithTimeout(f.lifecycle, flightTimeout)
		c = &flightCall[T]{done: make(chan struct{}), cancel: cancel}
		f.m[key] = c
		go f.run(key, c, workCtx, fn)
	}
	c.waiters++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		c.waiters--
		if c.waiters == 0 {
			c.cancel()
			if f.m[key] == c {
				delete(f.m, key)
			}
		}
	}()
	select {
	case <-ctx.Done():
		return zero, shared, ctx.Err()
	case <-c.done:
		if !shared && c.panicValue != nil {
			panic(c.panicValue)
		}
		return c.val, shared, c.err
	}
}

func (f *Flight[T]) run(key string, c *flightCall[T], ctx context.Context, fn func(context.Context) (T, error)) {
	defer func() {
		if r := recover(); r != nil {
			c.panicValue = r
			c.err = fmt.Errorf("flight: panic in shared call: %v", r)
		}
		c.cancel()
		f.mu.Lock()
		if f.m[key] == c {
			delete(f.m, key)
		}
		<-f.slots
		close(c.done)
		f.mu.Unlock()
	}()
	c.val, c.err = fn(ctx)
}
