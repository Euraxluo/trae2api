package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 本文件实现**控制台 Web 密码闸**（config 里的 `web_password`）。
//
// ## 为什么必须有这个文件
//
// `web_password` 这个配置项**早就声明了**，注释写着"控制台密码；空 = 免密"，
// 但**代码里从来没读过它**。那是最糟的一种情况：用户设了密码、以为控制台受保护，
// 实际上一点作用都没有 —— 一个"看起来是安全控制"的死配置，比没有这个配置危险得多。
//
// ## 定位：它保护什么、不保护什么
//
// - **保护**：控制台**页面本身**（挡掉随手打开 http://host:port/ 的人）。
// - **不保护**：API 端点。那些由 API Key / RBAC 负责（见 withAuth）。
//
// 两者是**正交**的：密码闸挡的是"浏览器访问 UI"，API Key 挡的是"程序访问接口"。
// 所以即使设了密码，用 curl 调 /v1/chat/completions 仍然只需要 API Key。
//
// ## 会话怎么维持
//
// 登录成功后发一个**随机 token** 存进程内（带 TTL），放进 HttpOnly Cookie。
// 不做签名 Cookie：那需要引入 HMAC 与密钥管理，而这里的威胁模型只是
// "挡住随手访问"，进程内 token 足够且更简单（重启即失效，符合预期）。
type webAuth struct {
	// password 为空的闸是"关闭"状态（免密）。
	password string

	mu       sync.Mutex
	sessions map[string]time.Time // token → 过期时刻
}

// webSessionTTL 控制台登录态有效期。
//
// 取 12 小时：覆盖一个工作日的使用，又不至于让一次登录永久有效。
const webSessionTTL = 12 * time.Hour

// MinPasswordLen 是控制台密码的最短长度。
//
// 设 6 位而不是更长：这是**本地自托管工具**的门禁，过长的强制要求会让用户
// 干脆不设密码（那就完全没有保护了）。真正的防线是"设了密码 + 登录限流"，
// 不是密码强度本身。
const MinPasswordLen = 6

func newWebAuth(password string) *webAuth {
	return &webAuth{password: strings.TrimSpace(password), sessions: map[string]time.Time{}}
}

// ValidatePassword 校验密码是否符合要求（供配置校验与将来的改密端点用）。
//
// 空串是合法的（= 免密）；非空则必须 >= MinPasswordLen。
func ValidatePassword(pw string) error {
	pw = strings.TrimSpace(pw)
	if pw == "" {
		return nil // 免密
	}
	// 按**字符数**而不是字节数：中文密码按字节算会莫名其妙地"够长"
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("控制台密码至少需要 %d 位（当前 %d 位）；留空则恢复单机免密",
			MinPasswordLen, len([]rune(pw)))
	}
	return nil
}

// Enabled 报告密码闸是否生效（未配置密码时为 false = 免密）。
func (w *webAuth) Enabled() bool {
	return w != nil && w.password != ""
}

// Check 用常数时间比较密码。
//
// ⚠️ 必须用 subtle.ConstantTimeCompare 而不是 `==`：
// 字符串比较会在第一个不同的字节处提前返回，攻击者可以通过测量响应时间
// 逐字节猜出密码（时序侧信道）。这是"看起来能用但实际有洞"的典型。
func (w *webAuth) Check(input string) bool {
	if !w.Enabled() {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(input), []byte(w.password)) == 1
}

// NewSession 登录成功后签发一个会话 token。
func (w *webAuth) NewSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败是严重问题，不该静默降级到弱随机
		panic("webauth: crypto/rand 不可用: " + err.Error())
	}
	token := hex.EncodeToString(b)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reapLocked()
	w.sessions[token] = time.Now().Add(webSessionTTL)
	return token
}

// ValidSession 校验会话 token。
func (w *webAuth) ValidSession(token string) bool {
	if !w.Enabled() {
		return true
	}
	if token == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	until, ok := w.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(w.sessions, token)
		return false
	}
	return true
}

// DropSession 注销。
func (w *webAuth) DropSession(token string) {
	if w == nil || token == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sessions, token)
}

// reapLocked 惰性清理过期会话（不另起 goroutine）。
func (w *webAuth) reapLocked() {
	now := time.Now()
	for t, until := range w.sessions {
		if now.After(until) {
			delete(w.sessions, t)
		}
	}
}

// SessionCount 返回有效会话数（诊断用）。
func (w *webAuth) SessionCount() int {
	if !w.Enabled() {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reapLocked()
	return len(w.sessions)
}

// ---- HTTP 层 ----

const webAuthCookie = "trae2api_console"

// consoleAuthed 判断当前请求是否有权看控制台页面。
//
// 未配置密码 → 恒 true（免密）。
func (s *Server) consoleAuthed(r *http.Request) bool {
	if s.webAuth == nil || !s.webAuth.Enabled() {
		return true
	}
	c, err := r.Cookie(webAuthCookie)
	if err != nil {
		return false
	}
	return s.webAuth.ValidSession(c.Value)
}

// trustProxyHeaders 安全地读取配置（cfg 为 nil 时按 false 处理）。
//
// 为什么容错：cfg 为 nil 是"构造不完整"，不该让**登录处理**崩掉 ——
// 那会把一个配置问题放大成"控制台完全打不开"。默认 false 也是更安全的取值。
func (s *Server) trustProxyHeaders() bool {
	return s.cfg != nil && s.cfg.TrustProxyHeaders
}

// handleConsoleLogin POST /login —— 校验密码并签发会话。
func (s *Server) handleConsoleLogin(w http.ResponseWriter, r *http.Request) {
	if s.webAuth == nil || !s.webAuth.Enabled() {
		http.Redirect(w, r, "/console", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	// 防暴力破解：按来源 IP 限流（见 security.go 的 loginLimiter）。
	ip := clientIP(r, s.trustProxyHeaders())
	now := time.Now()
	if ok, remain := s.loginLimit.Allow(ip, now); !ok {
		s.logf("WARN: 控制台登录已被限流（IP %s，还需 %.0f 秒）", ip, remain.Seconds())
		writeLoginHTML(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(remain.Minutes())+1))
		return
	}

	pw := r.FormValue("password")
	if !s.webAuth.Check(pw) {
		locked, lockFor := s.loginLimit.NoteFailure(ip, now)
		if locked {
			s.logf("WARN: 控制台登录连续失败达阈值，已锁定 IP %s（%s）", ip, lockFor)
			writeLoginHTML(w, http.StatusTooManyRequests,
				fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(lockFor.Minutes())))
			return
		}
		s.logf("WARN: 控制台登录失败（密码错误）来自 %s", ip)
		// 不区分"密码错误"与"密码为空"，避免给攻击者额外信息
		writeLoginHTML(w, http.StatusUnauthorized, "密码不正确")
		return
	}
	s.loginLimit.NoteSuccess(ip)
	token := s.webAuth.NewSession()
	http.SetCookie(w, &http.Cookie{
		Name:     webAuthCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true, // 禁止 JS 读取，降低 XSS 窃取风险
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(webSessionTTL.Seconds()),
	})
	s.logf("INFO: 控制台登录成功，来自 %s", r.RemoteAddr)
	http.Redirect(w, r, "/console", http.StatusFound)
}

// handleConsoleLogout POST /logout
func (s *Server) handleConsoleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(webAuthCookie); err == nil {
		s.webAuth.DropSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: webAuthCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	http.Redirect(w, r, "/console", http.StatusFound)
}

// writeLoginHTML 渲染登录页。
func writeLoginHTML(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	msg := ""
	if errMsg != "" {
		msg = `<p class="err">` + htmlEscape(errMsg) + `</p>`
	}
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Trae2Api 控制台登录</title><style>
*{box-sizing:border-box}
body{font:15px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;background:#0f1115;color:#e6e8ee;
display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;padding:20px}
.box{width:100%;max-width:380px;padding:32px;background:#171a21;border:1px solid #2a2f3a;border-radius:12px}
h1{font-size:18px;margin:0 0 6px}
p.sub{color:#6b7280;font-size:13px;margin:0 0 20px}
input{width:100%;padding:10px 12px;background:#0b0d12;color:#e6e8ee;border:1px solid #2a2f3a;
border-radius:8px;font-size:15px;margin-bottom:14px}
input:focus{outline:none;border-color:#4da3ff}
button{width:100%;padding:10px;background:#2b6cb0;color:#fff;border:0;border-radius:8px;
font-size:15px;cursor:pointer}
button:hover{background:#3182ce}
.err{color:#fc8181;font-size:13px;margin:0 0 12px}
</style></head><body>
<form class="box" method="POST" action="/login">
  <h1>Trae2Api 控制台</h1>
  <p class="sub">需要密码才能访问</p>
  ` + msg + `
  <input type="password" name="password" placeholder="控制台密码" autofocus autocomplete="current-password">
  <button type="submit">登录</button>
</form></body></html>`))
}

// htmlEscape 是给登录页用的最小转义（只用于把固定文案与错误提示放进 HTML）。
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
