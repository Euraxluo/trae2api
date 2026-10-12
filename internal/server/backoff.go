package server

import (
	"context"
	"math/rand"
	"time"
)

// 本文件实现换号轮转之间的**指数退避 + 抖动**。
//
// 为什么要退避：连续失败往往意味着上游在频控。立刻重试只会让请求更密，
// 而频控通常**按密度判罚** —— 越是密集重试，越容易被加重处罚。
//
// 为什么必须加抖动：多个并发请求若"齐步走"，会在同一时刻一起重试，
// 以固定周期再次聚成一股洪峰。±25% 的随机抖动把它们的相位打散。
const (
	// backoffBase 第一次退避的基准。
	backoffBase = 500 * time.Millisecond
	// backoffCap 单次退避上限。
	//
	// 取 8s 的理由：再长就超过多数客户端的耐心阈值了（它们会先超时断开），
	// 而断开后我们做的退避毫无意义。真正的"长时间不健康"应该由账号冷却
	// 与熔断处理，不是靠在这里睡。
	backoffCap = 8 * time.Second
	// backoffJitter 抖动比例（±25%）。
	backoffJitter = 0.25
)

// backoffAfter 返回第 n 次失败后应等待的时长（n 从 0 开始）。
//
// 用**逐次翻倍**而不是位移运算：base 变成可配置值之后，位移需要同步维护
// 一个"移位上限"，很容易改漏；逐次翻倍只需在循环里比较封顶值。
func backoffAfter(n int) time.Duration {
	if n <= 0 {
		return jitterDur(backoffBase)
	}
	d := backoffBase
	for k := 0; k < n && d < backoffCap; k++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	return jitterDur(d)
}

// jitterDur 给时长叠加 ±backoffJitter 的随机抖动。
//
// 下界钳到 0：极端情况下（base 很小 + 负抖动）不能让时长变成负数，
// 否则 time.After 会立即触发，退避形同虚设。
func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := float64(d) * backoffJitter
	delta := (rand.Float64()*2 - 1) * span
	out := time.Duration(float64(d) + delta)
	if out < 0 {
		return 0
	}
	return out
}

// sleepCtx 是可取消的退避等待。
//
// 为什么不用 time.Sleep：客户端可能已经断开、或者网关正在优雅停机。
// 那种情况下继续睡满退避毫无意义，只会拖慢停机、白占一个 goroutine。
// 返回 false 表示 ctx 已取消（调用方应立即收工）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
