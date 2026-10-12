package traectl

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- transport ----

// TestTransportDisablesH2 必须用**空的 TLSNextProto 映射**真正禁掉 h2。
//
// 这是本文件最重要的一条断言。踩过的坑：
//   - 设 `ForceAttemptHTTP2: false` **不管用** —— 它只对自定义 Dial 生效，
//     默认 TLS 栈仍会经 ALPN 协商出 h2。半死的 h2 流被复用后表现为
//     `http2: timeout awaiting response headers`，排查时完全看不出是 h2 的问题。
//   - 留 `TLSNextProto` 为 nil **也是错的** —— nil 的含义是"让标准库注入
//     默认 h2 映射"，恰好是反效果。
//
// 唯一正确的写法是 `make(map[string]func(string, *tls.Conn) http.RoundTripper)`。
func TestTransportDisablesH2(t *testing.T) {
	tr := newTransport()
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto 为 nil —— 标准库会注入默认 h2 映射，h2 没被禁掉")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto 应为空映射，实际 %d 项", len(tr.TLSNextProto))
	}
}

// TestTransportTimeoutsSet 四道闸都必须设上。
//
// 每一项都对应一个"默认值会导致卡死"的场景，缺任何一项都白搭。
func TestTransportTimeoutsSet(t *testing.T) {
	tr := newTransport()
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout || tlsHandshakeTimeout <= 0 {
		t.Errorf("TLSHandshakeTimeout 未设或为 0（握手挂起时无兜底）: %v", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout || responseHeaderTimeout <= 0 {
		t.Errorf("ResponseHeaderTimeout 未设: %v", tr.ResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout != idleConnTimeout || idleConnTimeout <= 0 {
		t.Errorf("IdleConnTimeout 未设: %v", tr.IdleConnTimeout)
	}
	if tr.DialContext == nil {
		t.Error("DialContext 未设（半开连接会卡到系统重传窗口）")
	}
	if tr.MaxIdleConns <= 0 || tr.MaxIdleConnsPerHost <= 0 {
		t.Error("连接池容量未设")
	}
}

// TestDialerHasTimeoutAndKeepAlive 拨号器必须有建连超时与短 keepalive。
//
// 默认 keepalive 是 **2 小时**才发第一个探测 —— 在 NAT 黑洞里，
// 2 小时足够一条死连接被反复复用。
func TestDialerHasTimeoutAndKeepAlive(t *testing.T) {
	d := newDialer()
	if d.Timeout != dialTimeout || d.Timeout <= 0 {
		t.Errorf("建连超时未设: %v", d.Timeout)
	}
	if d.KeepAlive != dialKeepAlive || d.KeepAlive <= 0 {
		t.Errorf("keepalive 未设: %v", d.KeepAlive)
	}
	if dialKeepAlive > time.Minute {
		t.Errorf("keepalive 周期 %v 太长（默认 2h 的问题是死连接被反复复用）", dialKeepAlive)
	}
}

// TestResponseHeaderTimeoutBelowOverall 响应头超时必须明显小于整体超时。
//
// 否则整体超时先生效，响应头超时就形同虚设。
func TestResponseHeaderTimeoutBelowOverall(t *testing.T) {
	if responseHeaderTimeout >= 5*time.Minute {
		t.Errorf("响应头超时 %v 不该接近整体超时（长回复靠流中空闲监控，不靠它）",
			responseHeaderTimeout)
	}
}

// plainRoundTripper 是**未实现** closeIdler 的 RoundTripper（模拟测试注入的替身）。
type plainRoundTripper struct{}

func (plainRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// countingRoundTripper 记录 CloseIdleConnections 是否被调用。
type countingRoundTripper struct {
	mu     sync.Mutex
	closed int
}

func (c *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (c *countingRoundTripper) CloseIdleConnections() {
	c.mu.Lock()
	c.closed++
	c.mu.Unlock()
}

// TestCloseIdleOnFailureSafeOnNil nil 与非实现者都不该 panic。
func TestCloseIdleOnFailureSafeOnNil(t *testing.T) {
	closeIdleOnFailure(nil)                 // 不该 panic
	closeIdleOnFailure(plainRoundTripper{}) // 未实现 closeIdler → 静默跳过
}

// TestCloseIdleOnFailureCallsThrough 实现了 closeIdler 的必须真的被调用。
//
// 为什么重要：失败的连接可能仍留在空闲池里（还没到 IdleConnTimeout），
// 下一个请求会继续捡到它 —— 于是"一次抖动"变成"连续几次失败"。
func TestCloseIdleOnFailureCallsThrough(t *testing.T) {
	rt := &countingRoundTripper{}
	closeIdleOnFailure(rt)
	if rt.closed != 1 {
		t.Errorf("CloseIdleConnections 调用次数 = %d want 1", rt.closed)
	}
	// 真实 Transport 也实现了该接口
	closeIdleOnFailure(newTransport())
}

// ---- idle ----

// TestIdleTickClamping 监控周期 = idle/4，钳在 [10ms, 1s]。
func TestIdleTickClamping(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want time.Duration
	}{
		{4 * time.Second, time.Second},                 // 1s（正好等于上钳）
		{100 * time.Second, time.Second},               // 25s → 上钳到 1s
		{40 * time.Millisecond, 10 * time.Millisecond}, // 10ms（正好等于下钳）
		{4 * time.Millisecond, 10 * time.Millisecond},  // 1ms → 下钳到 10ms
		{400 * time.Millisecond, 100 * time.Millisecond},
	}
	for _, c := range cases {
		if got := idleTick(c.idle); got != c.want {
			t.Errorf("idleTick(%v) = %v want %v", c.idle, got, c.want)
		}
	}
	// 下钳保证小 idle 也能被及时发现；上钳避免大 idle 下空转
	if d := idleTick(time.Hour); d != time.Second {
		t.Errorf("大 idle 下周期应上钳到 1s，实际 %v", d)
	}
}

// TestIdleMonitoringCancelsOnSilence 静默超过阈值时必须取消 context。
func TestIdleMonitoringCancelsOnSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 一个永远不返回数据的 body
	pr, pw := io.Pipe()
	defer pw.Close()
	body := monitorBody(pr, 60*time.Millisecond, cancel)
	defer body.Close()

	select {
	case <-ctx.Done():
		// 期望：静默超时后 context 被取消
	case <-time.After(2 * time.Second):
		t.Fatal("静默超时后 context 未被取消 —— 流会永远挂着")
	}
}

// TestIdleMonitoringKeepsAliveWhileDataFlows 只要还在吐数据就不该被掐。
//
// 这条是"别误杀长回复"的回归：推理模型思考几分钟是常态，
// 用整体超时去管它会把正常请求杀掉。
func TestIdleMonitoringKeepsAliveWhileDataFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	body := monitorBody(pr, 80*time.Millisecond, cancel)
	defer body.Close()

	// 持续吐数据（每 20ms 一个字节，远密于 80ms 的空闲阈值）
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := pw.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// 并发读，让 lastRead 被刷新
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		for {
			if _, err := body.Read(buf); err != nil {
				return
			}
		}
	}()

	// ⚠️ 必须在**数据仍在流动期间**检查 —— 写入停止后再查，
	// 那 80ms 静默被取消是**正确行为**，测出来的会是假失败。
	for i := 0; i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Fatalf("第 %d 次检查：数据持续流动时不该被掐（会误杀正常的长回复）", i)
		default:
		}
	}
	// 收尾：先关 pipe 让阻塞中的 reader 返回，再等 goroutine 退出。
	// （顺序反了会死等 —— reader 阻塞在没有数据的 Read 上，wg.Wait 永远不返回。）
	close(stop)
	pw.Close()
	_ = body.Close()
	wg.Wait()
}

// TestMonitorBodyDisabled idle<=0 时原样返回（零开销）。
func TestMonitorBodyDisabled(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	got := monitorBody(pr, 0, func() {})
	if got != io.ReadCloser(pr) {
		t.Error("idle<=0 时应原样返回底流")
	}
	// cancel 为 nil 时也要原样返回（避免空指针）
	if got := monitorBody(pr, time.Second, nil); got != io.ReadCloser(pr) {
		t.Error("cancel 为 nil 时应原样返回底流")
	}
}

// TestIdleBodyCloseStopsGoroutine Close 必须停掉后台 goroutine 且可重复调用。
func TestIdleBodyCloseStopsGoroutine(t *testing.T) {
	pr, pw := io.Pipe()
	canceled := false
	var mu sync.Mutex
	body := monitorBody(pr, time.Second, func() { mu.Lock(); canceled = true; mu.Unlock() })

	if err := body.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Logf("Close 返回: %v", err)
	}
	// 重复 Close 不该 panic
	_ = body.Close()

	mu.Lock()
	got := canceled
	mu.Unlock()
	if !got {
		t.Error("Close 应取消 context")
	}
	pw.Close()
}

// ---- cachekey ----

// TestCacheKeyAccountIsolation 是本文件的安全核心：**不同账号必须得到不同的 key**。
//
// 若两个账号共用同一个 cache key，上游会命中**另一个账号**的前缀缓存 ——
// 那等于把对方的对话内容喂给了你的请求。
func TestCacheKeyAccountIsolation(t *testing.T) {
	conv := "same-conversation-id"
	k1 := BuildPromptCacheKey("acct-AAAA-1111", conv)
	k2 := BuildPromptCacheKey("acct-BBBB-2222", conv)
	if k1 == k2 {
		t.Fatalf("不同账号在相同会话下得到了相同的 cache key（会跨账号泄露对话）: %q", k1)
	}
	// uid8 段必须不同
	if k1[:12] == k2[:12] {
		t.Errorf("uid8 隔离段相同: %q vs %q", k1[:12], k2[:12])
	}
}

// TestCacheKeyStableForSameAccountAndConversation 同账号同会话必须稳定
// （否则每次都算新前缀，缓存永远不命中）。
func TestCacheKeyStableForSameAccountAndConversation(t *testing.T) {
	a := BuildPromptCacheKey("acct-1", "conv-x")
	b := BuildPromptCacheKey("acct-1", "conv-x")
	if a != b {
		t.Errorf("同账号同会话应稳定: %q vs %q", a, b)
	}
}

// TestCacheKeyDiffersByConversation 同账号不同会话必须不同。
func TestCacheKeyDiffersByConversation(t *testing.T) {
	a := BuildPromptCacheKey("acct-1", "conv-a")
	b := BuildPromptCacheKey("acct-1", "conv-b")
	if a == b {
		t.Error("不同会话应有不同的 key（否则会复用到别的对话的前缀）")
	}
}

// TestCacheKeyFormat 格式 `t2a-<uid8>-<16字节hex>`。
func TestCacheKeyFormat(t *testing.T) {
	k := BuildPromptCacheKey("916348159159136", "conv")
	parts := strings.Split(k, "-")
	if len(parts) != 3 {
		t.Fatalf("格式不对（应为 3 段）: %q", k)
	}
	if parts[0] != "t2a" {
		t.Errorf("前缀 = %q want t2a", parts[0])
	}
	if parts[1] != "91634815" {
		t.Errorf("uid8 = %q want 91634815", parts[1])
	}
	if len(parts[2]) != 32 {
		t.Errorf("会话段应为 16 字节 hex（32 字符），实际 %d", len(parts[2]))
	}
}

// TestCacheKeyEmptyUIDStillIsolatedByConversation uid 为空时仍要生成 key，
// 但此时隔离只剩会话段（调用方应保证传真实 uid）。
func TestCacheKeyEmptyUID(t *testing.T) {
	k := BuildPromptCacheKey("", "conv")
	if k == "" {
		t.Fatal("uid 为空也该生成 key")
	}
	if !strings.HasPrefix(k, "t2a--") {
		t.Errorf("空 uid 应产生固定隔离段: %q", k)
	}
	// 空会话与有会话应不同
	if BuildPromptCacheKey("u", "") == BuildPromptCacheKey("u", "x") {
		t.Error("空会话与有会话应得到不同的 key")
	}
}
