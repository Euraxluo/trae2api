package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 本文件实现 **控制台登录的防暴力破解** 与 **全站安全响应头**。
//
// 这两件事在参考实现里都有，而我原先完全没有 —— 而它们不是"功能多少"的问题，
// 是**真问题**：
//   - 没有限流：`web_password` 可以被无限次尝试，弱密码几分钟内就被爆破
//   - 没有安全头：浏览器会按内容嗅探类型（可能把 JSON 当 HTML 执行）、
//     页面可被任意站点 iframe 嵌入（点击劫持）

// loginLimiter 是**按来源 IP** 的登录失败限流。
//
// 策略（对齐参考实现）：同一 IP 在 window 内失败达到 maxFail 次 → 锁定 lockFor。
//
// 为什么按 IP 而不是全局：全局计数会被一个攻击者用来**锁死所有人**
// （拒绝服务）；按 IP 只影响攻击者自己。
//
// ⚠️ 局限（必须写清楚）：这是**进程内**状态，且按 IP 计数。
//   - 多实例部署时各算各的（需要 Redis 才能全局一致）
//   - 攻击者换 IP 即可绕过；在反向代理后面若没配好真实 IP，所有请求会算作同一个 IP
//     （见 clientIP 的说明）
//
// 它挡的是"低成本脚本爆破"，不是有组织的攻击 —— 后者要靠强密码与网络层防护。
type loginLimiter struct {
	window  time.Duration
	maxFail int
	lockFor time.Duration

	mu       sync.Mutex
	fails    map[string][]time.Time // IP → 失败时刻
	lockedTo map[string]time.Time   // IP → 解锁时刻
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		window:   5 * time.Minute,
		maxFail:  5,
		lockFor:  5 * time.Minute,
		fails:    map[string][]time.Time{},
		lockedTo: map[string]time.Time{},
	}
}

// Allow 报告该 IP 当前是否允许尝试登录。
//
// 返回 (允许, 剩余锁定时长)。剩余时长用于给用户一个明确的提示
// （"请 X 分钟后再试"），而不是一个含糊的 401。
func (l *loginLimiter) Allow(ip string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reapLocked(now)

	if until, ok := l.lockedTo[ip]; ok {
		if now.Before(until) {
			return false, time.Until(until)
		}
		delete(l.lockedTo, ip)
	}
	return true, 0
}

// NoteFailure 记一次登录失败；达到阈值则锁定。
//
// 返回 (是否刚触发锁定, 锁定时长)。
func (l *loginLimiter) NoteFailure(ip string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reapLocked(now)

	// 已在锁定期内不该再记（它本来就不该走到这里）
	if until, ok := l.lockedTo[ip]; ok && now.Before(until) {
		return false, 0
	}

	cutoff := now.Add(-l.window)
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.fails[ip] = kept

	if len(kept) >= l.maxFail {
		l.lockedTo[ip] = now.Add(l.lockFor)
		delete(l.fails, ip) // 锁定时清计数：解锁后重新累计
		return true, l.lockFor
	}
	return false, 0
}

// NoteSuccess 登录成功时清掉该 IP 的失败记录。
//
// 为什么要清：用户可能只是先打错了两次再打对。不清的话那两次会一直留着，
// 之后正常使用中再错几次就被锁 —— 那是误伤。
func (l *loginLimiter) NoteSuccess(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
	delete(l.lockedTo, ip)
}

// reapLocked 惰性清理过期条目（不另起 goroutine）。
func (l *loginLimiter) reapLocked(now time.Time) {
	cutoff := now.Add(-l.window)
	for ip, ts := range l.fails {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.fails, ip)
		} else {
			l.fails[ip] = kept
		}
	}
	for ip, until := range l.lockedTo {
		if now.After(until) {
			delete(l.lockedTo, ip)
		}
	}
}

// Status 返回限流状态（诊断用）。
func (l *loginLimiter) Status() map[string]any {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.reapLocked(now)
	return map[string]any{
		"window_seconds": int(l.window.Seconds()),
		"max_failures":   l.maxFail,
		"lock_seconds":   int(l.lockFor.Seconds()),
		"tracked_ips":    len(l.fails),
		"locked_ips":     len(l.lockedTo),
	}
}

// clientIP 尽力取出**真实**来源 IP。
//
// 代理头（X-Forwarded-For / X-Real-IP）**只在可信代理后**才该采信 ——
// 否则攻击者伪造一个头就能绕过限流（每次换一个假 IP，等于没有限流）。
//
// 所以这里的策略是保守的：默认用 RemoteAddr；只有当配置显式声明
// "我在反向代理后面"（cfg.TrustProxyHeaders）时才采信代理头。
// 宁可把代理后的所有请求算作同一个 IP（限流偏严），也不要被伪造头绕过（等于没有）。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// X-Forwarded-For 形如 "client, proxy1, proxy2"，第一个是原始客户端
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// securityHeaders 是给**所有响应**加的安全头。
//
// 逐个说明为什么需要（它们都不是"凑数"）：
//
//	X-Content-Type-Options: nosniff
//	  禁止浏览器按内容嗅探类型。没有它，一个被当作 text/plain 返回的 JSON
//	  可能被当成 HTML 执行（存储型 XSS 的常见入口）。
//
//	X-Frame-Options: SAMEORIGIN
//	  禁止别的站点用 iframe 嵌入控制台。没有它，攻击者可以叠一层透明 iframe
//	  诱导用户点击（点击劫持）—— 对"一键解除冷却""删除账号"这类按钮很危险。
//
//	Referrer-Policy: no-referrer
//	  控制台 URL 里可能带 session_id 之类的参数，不该随外链泄漏给第三方。
//
//	Cache-Control: no-store（仅控制台与管理接口）
//	  避免共享机器上退出后仍能从浏览器缓存看到控制台内容。
func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "no-referrer")
	// 控制台是自包含的（无外部资源），可以用较严的 CSP：
	// 禁掉外部脚本/样式，只允许内联（页面本来就是内联的）。
	// 注意允许 data: 图片（Playground 的粘贴截图与模型图标）。
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data: https:; "+
			"style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; "+
			"connect-src 'self'; frame-ancestors 'self'")
}

// withSecurityHeaders 包一层，给所有响应加安全头。
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		next.ServeHTTP(w, r)
	})
}
