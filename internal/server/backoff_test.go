package server

import (
	"context"
	"testing"
	"time"
)

// TestBackoffGrowsExponentially 逐次翻倍。
func TestBackoffGrowsExponentially(t *testing.T) {
	// 抖动是 ±25%，所以断言区间而不是精确值
	inRange := func(d time.Duration, base time.Duration) bool {
		lo := time.Duration(float64(base) * 0.75)
		hi := time.Duration(float64(base) * 1.25)
		return d >= lo-1 && d <= hi+1
	}
	for n, want := range map[int]time.Duration{
		0:  backoffBase,     // 500ms
		1:  backoffBase * 2, // 1s
		2:  backoffBase * 4, // 2s
		3:  backoffBase * 8, // 4s
		4:  backoffCap,      // 8s（正好封顶）
		5:  backoffCap,      // 封顶
		20: backoffCap,      // 远超大值仍封顶
	} {
		got := backoffAfter(n)
		if !inRange(got, want) {
			t.Errorf("backoffAfter(%d) = %v，期望在 %v ±25%% 内", n, got, want)
		}
	}
}

// TestBackoffNeverExceedsCap 任何 n 都不能超过封顶。
func TestBackoffNeverExceedsCap(t *testing.T) {
	max := time.Duration(float64(backoffCap) * 1.25)
	for n := 0; n < 64; n++ {
		if d := backoffAfter(n); d > max {
			t.Fatalf("backoffAfter(%d) = %v 超过封顶上限 %v", n, d, max)
		}
	}
}

// TestBackoffJitterActuallyVaries 抖动必须真的在变。
//
// 为什么重要：多个并发请求若"齐步走"，会在同一时刻一起重试，
// 以固定周期再次聚成一股洪峰 —— 而频控通常按密度判罚。
func TestBackoffJitterActuallyVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[backoffAfter(3)] = true
	}
	if len(seen) < 5 {
		t.Errorf("50 次采样只得到 %d 个不同值 —— 抖动没生效（齐步走会被频控加重处罚）",
			len(seen))
	}
}

// TestBackoffNegativeN 负数 n 不该 panic，按首次退避处理。
func TestBackoffNegativeN(t *testing.T) {
	if d := backoffAfter(-5); d <= 0 {
		t.Errorf("负数 n 应回落到首次退避，得到 %v", d)
	}
}

// TestJitterDurNonNegative 抖动不能产生负时长。
//
// 负时长会让 time.After 立即触发，退避形同虚设。
func TestJitterDurNonNegative(t *testing.T) {
	for i := 0; i < 200; i++ {
		if d := jitterDur(time.Nanosecond); d < 0 {
			t.Fatalf("jitterDur 产生了负时长: %v", d)
		}
	}
	if d := jitterDur(0); d != 0 {
		t.Errorf("jitterDur(0) = %v want 0", d)
	}
	if d := jitterDur(-time.Second); d != 0 {
		t.Errorf("jitterDur(负数) = %v want 0", d)
	}
}

// TestSleepCtxReturnsAfterDuration 正常等待后返回 true。
func TestSleepCtxReturnsAfterDuration(t *testing.T) {
	start := time.Now()
	if !sleepCtx(context.Background(), 60*time.Millisecond) {
		t.Fatal("未被取消时应返回 true")
	}
	if e := time.Since(start); e < 50*time.Millisecond {
		t.Errorf("等待时长不足: %v", e)
	}
}

// TestSleepCtxCancellable 取消后必须立刻返回 false。
//
// 为什么重要：客户端可能已断开、或网关正在优雅停机 ——
// 那种情况下继续睡满退避毫无意义，只会拖慢停机、白占一个 goroutine。
func TestSleepCtxCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepCtx(ctx, 10*time.Second) {
		t.Fatal("被取消时应返回 false")
	}
	if e := time.Since(start); e > 2*time.Second {
		t.Errorf("取消后返回过慢（%v）—— 说明没用 select 监听 ctx", e)
	}
}

// TestSleepCtxZeroDuration 零时长立即返回。
func TestSleepCtxZeroDuration(t *testing.T) {
	if !sleepCtx(context.Background(), 0) {
		t.Error("零时长应立即返回 true")
	}
	// 已取消的 ctx + 零时长 → false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, 0) {
		t.Error("已取消的 ctx 即便零时长也应返回 false")
	}
}
