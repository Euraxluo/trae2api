package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLoginLimiterLocksAfterThreshold 达阈后锁定。
func TestLoginLimiterLocksAfterThreshold(t *testing.T) {
	l := newLoginLimiter()
	ip := "1.2.3.4"
	now := time.Now()

	for i := 1; i <= l.maxFail; i++ {
		ok, _ := l.Allow(ip, now)
		if !ok {
			t.Fatalf("第 %d 次不该已被锁", i)
		}
		locked, lockFor := l.NoteFailure(ip, now)
		wantLocked := i >= l.maxFail
		if locked != wantLocked {
			t.Fatalf("第 %d 次锁定 = %v want %v", i, locked, wantLocked)
		}
		if locked && lockFor != l.lockFor {
			t.Errorf("锁定时长 = %v want %v", lockFor, l.lockFor)
		}
	}
	// 锁定后不该允许
	ok, remain := l.Allow(ip, now)
	if ok {
		t.Fatal("达阈后应被锁定")
	}
	if remain <= 0 || remain > l.lockFor {
		t.Errorf("剩余锁定时间不合理: %v", remain)
	}
	// 锁定期过后应恢复
	later := now.Add(l.lockFor + time.Second)
	if ok, _ := l.Allow(ip, later); !ok {
		t.Error("锁定到期后应恢复")
	}
}

// TestLoginLimiterPerIP 限流**按 IP 独立**。
//
// 为什么重要：全局计数会被一个攻击者用来**锁死所有人**（拒绝服务）。
// 按 IP 只影响攻击者自己。
func TestLoginLimiterPerIP(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	// 把 A 打满
	for i := 0; i < l.maxFail; i++ {
		l.NoteFailure("10.0.0.1", now)
	}
	if ok, _ := l.Allow("10.0.0.1", now); ok {
		t.Error("A 应被锁")
	}
	// B 不受影响
	if ok, _ := l.Allow("10.0.0.2", now); !ok {
		t.Error("B 不该被 A 的失败牵连（按 IP 隔离）")
	}
}

// TestLoginLimiterWindowSlides 窗口滑动：久远的失败不该计入。
func TestLoginLimiterWindowSlides(t *testing.T) {
	l := newLoginLimiter()
	ip := "1.2.3.4"
	base := time.Now()
	// 在窗口内失败 4 次
	for i := 0; i < l.maxFail-1; i++ {
		l.NoteFailure(ip, base)
	}
	// 过了窗口再来一次：前面 4 次已出窗 → 不该触发锁定
	later := base.Add(l.window + time.Second)
	if locked, _ := l.NoteFailure(ip, later); locked {
		t.Error("窗口外的失败不该计入（窗口应滑动）")
	}
}

// TestLoginLimiterSuccessClears 登录成功应清掉失败记录。
//
// 否则"先打错两次再打对"的那两次会一直留着，之后正常使用中再错几次就被锁 ——
// 那是误伤。
func TestLoginLimiterSuccessClears(t *testing.T) {
	l := newLoginLimiter()
	ip := "1.2.3.4"
	now := time.Now()
	for i := 0; i < l.maxFail-1; i++ {
		l.NoteFailure(ip, now)
	}
	l.NoteSuccess(ip)
	// 清空后再失败 maxFail-1 次仍不该锁
	for i := 0; i < l.maxFail-1; i++ {
		if locked, _ := l.NoteFailure(ip, now); locked {
			t.Fatal("成功后的计数应已清零")
		}
	}
}

// TestLoginLimiterNilSafe nil 限流器不该 panic（Server 构造不完整时）。
func TestLoginLimiterNilSafe(t *testing.T) {
	var l *loginLimiter
	if ok, _ := l.Allow("x", time.Now()); !ok {
		t.Error("nil 限流器应放行")
	}
	if locked, _ := l.NoteFailure("x", time.Now()); locked {
		t.Error("nil 限流器不该锁定")
	}
	l.NoteSuccess("x") // 不该 panic
	if l.Status() != nil {
		t.Error("nil 的 Status 应为 nil")
	}
}

// TestClientIPProxyHeaderNotTrustedByDefault 默认**不采信**代理头。
//
// ⚠️ 这是安全关键：若默认采信 X-Forwarded-For，攻击者每次换一个假 IP
// 就能完全绕过限流（等于没有限流）。
func TestClientIPProxyHeaderNotTrustedByDefault(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "192.168.1.100:54321"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	r.Header.Set("X-Real-IP", "2.2.2.2")

	if got := clientIP(r, false); got != "192.168.1.100" {
		t.Errorf("默认应使用 RemoteAddr，得到 %q（伪造头可绕过限流）", got)
	}
	// 显式信任时才采信，且取 XFF 的**第一段**（最接近原始客户端）
	if got := clientIP(r, true); got != "1.1.1.1" {
		t.Errorf("信任代理时应取 XFF 第一段，得到 %q", got)
	}
}

// TestClientIPForwardedForChain 多级代理时取第一段。
func TestClientIPForwardedForChain(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.5, 10.0.0.6")
	if got := clientIP(r, true); got != "203.0.113.9" {
		t.Errorf("应取第一段，得到 %q", got)
	}
}

// TestSecurityHeadersPresent 全站安全头必须都设上。
func TestSecurityHeadersPresent(t *testing.T) {
	w := httptest.NewRecorder()
	securityHeaders(w)
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q want %q", k, got, v)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors") {
		t.Errorf("CSP 应含 frame-ancestors（防点击劫持）: %q", csp)
	}
}

// TestSecurityHeadersAppliedToAllRoutes 安全头必须加在**所有**路由上。
//
// 逐个 handler 里设必然会漏 —— 我自己就漏过：只有导出端点设了 nosniff。
func TestSecurityHeadersAppliedToAllRoutes(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := withSecurityHeaders(inner)
	for _, path := range []string{"/", "/status", "/v1/models", "/admin/users", "/healthz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s 缺少安全头", path)
		}
		if w.Code != http.StatusTeapot {
			t.Errorf("%s 状态码被改动: %d", path, w.Code)
		}
	}
}

// TestConsoleLoginRateLimited 登录被限流时应返回 429 而不是 401。
//
// 用 429 而不是 401：让用户能区分"密码错了"与"我被锁了"，
// 而不是反复重试一个永远不会成功的密码。
func TestConsoleLoginRateLimited(t *testing.T) {
	wa := newWebAuth("correct-pw")
	lim := newLoginLimiter()
	s := &Server{webAuth: wa, loginLimit: lim}

	// 先把该 IP 打满
	for i := 0; i < lim.maxFail; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=wrong"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "9.9.9.9:1234"
		w := httptest.NewRecorder()
		s.handleConsoleLogin(w, req)
	}

	// 现在即使密码**正确**也该被限流挡住
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=correct-pw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "9.9.9.9:1234"
	w := httptest.NewRecorder()
	s.handleConsoleLogin(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("被限流时应 429，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "次数过多") {
		t.Error("应提示被限流及何时可重试")
	}
	// 不该签发会话
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookie && c.Value != "" {
			t.Error("被限流时不该签发会话")
		}
	}
}
