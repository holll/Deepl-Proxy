package service

import (
	"context"
	"fmt"
	"sync"
)

// Flight 把同一 key 的并发调用合并成一次（singleflight 的 ctx 感知版本）。
//
// 用途是缓存击穿：某个 key 刚失效或还没被翻译过时，N 个并发请求会同时
// 判定为 miss，然后各自打一次上游 —— 既浪费额度，也会在冷启动或热点
// 新文本上把上游打成尖峰。合并后只有第一个请求真的调用上游，其余等待复用。
//
// 与 golang.org/x/sync/singleflight 的区别：
//   - 等待方会响应自己的 ctx：客户端断开时可以立刻返回，不必陪跑。
//   - leader 的 fn 收到 leader 自己的 ctx，客户端断开仍会取消上游调用
//     （不为了合并而放弃取消语义）。
//
// 代价与语义边界：如果 leader 的 ctx 被取消，等待方会拿到同一个错误，
// 也就是这一批请求一起失败。这里选择"宁可一起失败也不并发重试"——
// 上游已经出问题或客户端已经离开时，让 N 个等待方各自重试只会放大故障。
type Flight[T any] struct {
	mu sync.Mutex
	m  map[string]*flightCall[T]
}

type flightCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// NewFlight 创建一个 Flight。零值不可用，因为 m 需要初始化。
func NewFlight[T any]() *Flight[T] {
	return &Flight[T]{m: make(map[string]*flightCall[T])}
}

// Do 执行 fn，同一 key 的并发调用只会真正执行一次。
//
// 返回 value、shared 和 error：shared 为 true 表示本次结果是复用了别人
// 的调用而非自己执行。等待方在 ctx 提前结束时返回 ctx.Err()。
func (f *Flight[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = make(map[string]*flightCall[T])
	}
	if c, ok := f.m[key]; ok {
		f.mu.Unlock()
		// 等待：要么等到结果，要么等到自己的 ctx 结束。
		select {
		case <-c.done:
			return c.val, true, c.err
		case <-ctx.Done():
			var zero T
			return zero, true, ctx.Err()
		}
	}
	c := &flightCall[T]{done: make(chan struct{})}
	f.m[key] = c
	f.mu.Unlock()

	panicValue := f.run(key, c, ctx, fn)
	if panicValue != nil {
		// fn 里的 panic 不应该被这里吞掉，交回给上层（gin.Recovery）处理，
		// 但必须先把等待方放走，否则它们会永久挂在这个 key 上。
		panic(panicValue)
	}

	return c.val, false, c.err
}

// run 执行 fn 并把结果写进 call，返回非 nil 表示 fn 发生了 panic。
func (f *Flight[T]) run(key string, c *flightCall[T], ctx context.Context, fn func(context.Context) (T, error)) (panicValue any) {
	finished := false
	finish := func() {
		if finished {
			return
		}
		finished = true
		f.mu.Lock()
		// 只删自己这条，避免把后来者注册的新 call 误删。
		if cur, ok := f.m[key]; ok && cur == c {
			delete(f.m, key)
		}
		f.mu.Unlock()
		close(c.done)
	}

	defer func() {
		if r := recover(); r != nil {
			// 失败时不返回部分结果，等待方只看 err。
			c.err = fmt.Errorf("flight: panic in shared call: %v", r)
			finish()
			panicValue = r
			return
		}
		finish()
	}()

	c.val, c.err = fn(ctx)
	return nil
}
