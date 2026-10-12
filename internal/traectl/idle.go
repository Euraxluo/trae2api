package traectl

import (
	"context"
	"io"
	"sync"
	"time"
)

// 本文件实现 **SSE 流的流中空闲监控**。
//
// 要解决的问题：`ResponseHeaderTimeout` 只管到"响应头到达"为止。
// 头一到，上游就可以**永远不吐数据**而不触发任何超时 ——
// 连接占着、账号租约占着、客户端干等。
//
// 所以需要一个**流中**的看门狗：只要还在吐数据就续命，静默超过阈值才断流。
//
// 为什么不能简单用一个整体 `Client.Timeout`：那会误杀正常的长回复
//（推理模型思考几分钟是常态）。必须按"静默时长"而不是"总时长"判断。

// idleMonitoringBody 包在 SSE body 外层，做流中空闲监控。
type idleMonitoringBody struct {
	rc       io.ReadCloser
	mu       sync.Mutex
	lastRead time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		// 读到数据就续命。注意判 n>0 而不是 err==nil：
		// 一次 Read 可能同时返回数据与 io.EOF，那也算"刚吐过"。
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// Close 停掉后台 goroutine、取消请求 context、关闭底流，保证无泄漏。
func (b *idleMonitoringBody) Close() error {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody 给 body 包上流中空闲监控。
//
// idle <= 0 时**原样返回**（禁用监控，零开销）。
//
// 计时从返回 body 之后才开始 —— 首字节阶段由 Transport.ResponseHeaderTimeout
// 负责，这里不抢跑（否则"上游正在冷启动"会被误判成"流中空闲"）。
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 || cancel == nil {
		return rc
	}
	b := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}
	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					// 静默超时：取消请求 context，中断阻塞中的 Read。
					// 用 cancel 而不是直接 Close：让上层看到的是
					// context.Canceled（可归类为"流中空闲超时"），
					// 而不是一个含糊的 "use of closed connection"。
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。
//
// 为什么是 idle/4：这样从"真的开始静默"到"被发现"最多多等 idle/4，
// 对总阈值的失真不超过 25%。
//
// 上钳 1s：大 idle（如 5 分钟）下 1/4 是 75s，空转周期太长且没必要。
// 下钳 10ms：小 idle（如 30ms，测试用）下 1/4 是 7.5ms，
// 定时器精度与调度开销不划算。
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
