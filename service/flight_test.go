package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightCoalescesConcurrentCalls(t *testing.T) {
	f := NewFlight[string]()

	const waiters = 8
	var calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]string, waiters)
	shareds := make([]bool, waiters)
	errs := make([]error, waiters)

	for i := range waiters {
		wg.Go(func() {
			v, shared, err := f.Do(context.Background(), "k", func(context.Context) (string, error) {
				if calls.Add(1) == 1 {
					close(entered)
				}
				<-release // 卡住 leader，确保其余调用都走进等待分支
				return "result", nil
			})
			results[i], shareds[i], errs[i] = v, shared, err
		})
	}

	// 等 leader 真正进入 fn，再给其余 goroutine 时间注册为等待方。
	// leader 在 close(release) 之前一直阻塞，这段时间内到达的调用必然复用结果。
	<-entered
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn should run exactly once, ran %d times", got)
	}

	leaders := 0
	for i := range waiters {
		if errs[i] != nil {
			t.Fatalf("waiter %d errored: %v", i, errs[i])
		}
		if results[i] != "result" {
			t.Fatalf("waiter %d got %q, want the shared result", i, results[i])
		}
		if !shareds[i] {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("exactly one caller should be the leader, got %d", leaders)
	}
}

// pending 由测试用于观察当前在飞的 key 数量（测试辅助，不属于公共 API）。
func (f *Flight[T]) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}

func TestFlightDifferentKeysDoNotCoalesce(t *testing.T) {
	f := NewFlight[int]()

	var calls atomic.Int32
	var wg sync.WaitGroup
	for _, key := range []string{"a", "b", "c"} {
		wg.Go(func() {
			_, _, _ = f.Do(context.Background(), key, func(context.Context) (int, error) {
				calls.Add(1)
				return 1, nil
			})
		})
	}
	wg.Wait()

	if got := calls.Load(); got != 3 {
		t.Fatalf("distinct keys must not coalesce, fn ran %d times", got)
	}
}

func TestFlightWaiterRespectsItsOwnContext(t *testing.T) {
	f := NewFlight[string]()
	release := make(chan struct{})
	entered := make(chan struct{})

	var leaderErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _, leaderErr = f.Do(context.Background(), "k", func(context.Context) (string, error) {
			close(entered)
			<-release
			return "late", nil
		})
	})
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	v, shared, err := f.Do(ctx, "k", func(context.Context) (string, error) {
		t.Error("waiter must not execute fn")
		return "", nil
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter should get its own ctx error, got %v", err)
	}
	if !shared {
		t.Fatal("waiter result should be marked shared")
	}
	if v != "" {
		t.Fatalf("waiter should get a zero value, got %q", v)
	}
	if elapsed > time.Second {
		t.Fatalf("waiter blocked on the leader instead of honouring ctx (%v)", elapsed)
	}

	close(release)
	wg.Wait()
	if leaderErr != nil {
		t.Fatalf("leader errored: %v", leaderErr)
	}
}

func TestFlightPropagatesErrorToWaiters(t *testing.T) {
	f := NewFlight[string]()
	boom := errors.New("upstream down")
	release := make(chan struct{})
	entered := make(chan struct{})

	var wg sync.WaitGroup
	var waiterErr error
	wg.Go(func() {
		_, _, _ = f.Do(context.Background(), "k", func(context.Context) (string, error) {
			close(entered)
			<-release
			return "", boom
		})
	})
	<-entered
	wg.Go(func() {
		_, _, waiterErr = f.Do(context.Background(), "k", func(context.Context) (string, error) {
			t.Error("waiter must not execute fn")
			return "", nil
		})
	})
	time.Sleep(100 * time.Millisecond) // 让等待方进入 wait 分支
	close(release)
	wg.Wait()

	if !errors.Is(waiterErr, boom) {
		t.Fatalf("waiter should see the leader's error, got %v", waiterErr)
	}
}

// key 必须在本轮结束后被移除，否则同一 key 的后续调用会被永久合并成旧结果。
func TestFlightForgetsKeyAfterCompletion(t *testing.T) {
	f := NewFlight[int]()
	var calls atomic.Int32
	run := func() {
		_, _, _ = f.Do(context.Background(), "k", func(context.Context) (int, error) {
			return int(calls.Add(1)), nil
		})
	}

	run()
	run()

	if got := calls.Load(); got != 2 {
		t.Fatalf("sequential calls must each run fn, got %d runs", got)
	}
	if n := f.pending(); n != 0 {
		t.Fatalf("in-flight map should be empty, has %d", n)
	}
}

// fn panic 时等待方必须被放行（拿到错误），leader 的 panic 继续向上抛。
func TestFlightPanicReleasesWaiters(t *testing.T) {
	f := NewFlight[string]()
	entered := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("leader panic should propagate")
			}
		}()
		_, _, _ = f.Do(context.Background(), "k", func(context.Context) (string, error) {
			close(entered)
			<-release
			panic("boom")
		})
	})
	<-entered

	var waiterErr error
	wg.Go(func() {
		_, _, waiterErr = f.Do(context.Background(), "k", func(context.Context) (string, error) {
			return "", nil
		})
	})
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if waiterErr == nil {
		t.Fatal("waiter should receive an error when the leader panics")
	}
	if n := f.pending(); n != 0 {
		t.Fatalf("panic must not leak an in-flight entry, has %d", n)
	}
}

func waitForWaiters(t *testing.T, f *Flight[string], n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		c := f.m["k"]
		ready := c != nil && c.waiters == n
		f.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("waiters did not register")
}

func TestFlightLeaderCancellationDoesNotCancelOtherWaiter(t *testing.T) {
	f := NewFlight[string]()
	leader, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := f.Do(leader, "k", func(ctx context.Context) (string, error) {
			close(entered)
			select {
			case <-release:
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		leaderDone <- err
	}()
	<-entered
	result := make(chan string, 1)
	go func() {
		v, _, err := f.Do(context.Background(), "k", func(context.Context) (string, error) { return "wrong", nil })
		if err != nil {
			result <- err.Error()
		} else {
			result <- v
		}
	}()
	waitForWaiters(t, f, 2)
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v", err)
	}
	close(release)
	if got := <-result; got != "ok" {
		t.Fatalf("remaining waiter: %q", got)
	}
}

func TestFlightAllWaitersCancelWork(t *testing.T) {
	f := NewFlight[string]()
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = f.Do(ctx, "k", func(work context.Context) (string, error) {
			close(entered)
			<-work.Done()
			close(stopped)
			return "", work.Err()
		})
	}()
	<-entered
	cancel()
	<-done
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("orphan shared work")
	}
}

func TestFlightLifecycleCancellation(t *testing.T) {
	root, stop := context.WithCancel(context.Background())
	defer stop()
	f := NewFlight[string](root)
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := f.Do(context.Background(), "k", func(ctx context.Context) (string, error) { close(entered); <-ctx.Done(); return "", ctx.Err() })
		done <- err
	}()
	<-entered
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle not canceled")
	}
}

func TestFlightCapacityIsBounded(t *testing.T) {
	f := NewFlight[string]()
	f.slots = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = f.Do(ctx, "k", func(work context.Context) (string, error) { close(entered); <-work.Done(); return "", work.Err() })
	}()
	<-entered
	ran := false
	_, _, err := f.Do(context.Background(), "other", func(context.Context) (string, error) { ran = true; return "", nil })
	if err == nil || ran {
		t.Fatal("capacity should reject additional workers")
	}
	cancel()
	<-done
}
