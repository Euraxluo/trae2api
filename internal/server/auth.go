package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"trae2api/internal/usermgr"
)

// 本文件实现**用户密码认证**（注册 / 登录 / 登出 / 自助改密 / 自助换 Key /
// 开放注册开关），是控制台「登录 / 注册 / 改密」三个模态的后端。
//
// ## 与控制台密码闸（webauth.go）的关系
//
// 两者是**两套独立的东西**，各有各的 cookie，互不覆盖：
//
//	trae2api_console  —— 控制台**页面**的门禁（config.web_password，挡"随手打开 UI 的人"）
//	trae2api_session  —— **用户身份**的登录态（用户名 + 密码，回答"我是谁"）
//
// ## 为什么没有直接用 webAuth.ValidSession
//
// 任务描述是"会话复用现有 webAuth 机制"。这里复用的是**机制与参数**：
// 同样的 crypto/rand 32 字节 token、同样的 HttpOnly + SameSite=Lax + Path=/ cookie、
// 同样的 12 小时 TTL（直接复用 webSessionTTL 常量）、同一个按 IP 的 loginLimiter。
//
// 但**不能**把 webAuth 的会话表当作登录态判据，有两个硬原因：
//
//  1. `ValidSession` 在密码闸**未启用**时恒返回 true（`if !w.Enabled() { return true }`）。
//     而"没设 web_password"是最常见的部署形态 —— 拿它当登录判据，等于
//     免密部署下**任何请求**（哪怕一个 cookie 都没有）都算"已登录"。
//  2. 它只回答"这个 token 有效吗"，**不回答"这个 token 是谁"**。
//     而 /me/password 与 /me/api-key/reset 必须知道身份才能操作。
//
// 所以用户登录态需要一张 token → 用户 ID 的表，这张表 webAuth 里没有，
// 也不该有（页面门禁与用户身份是两件事，混在一起以后必然互相拖累）。

// sessionCookie 是**用户登录态**的 cookie 名。
//
// 与 webAuthCookie（trae2api_console）并存，两者互不覆盖：
// 退出用户登录不该把控制台页面门禁也退掉，反之亦然。
const sessionCookie = "trae2api_session"

// ---- 用户登录态表 ----

// userSession 是一条用户登录态。
type userSession struct {
	userID  string
	expires time.Time
}

// userSessionStore 是 token → (用户 ID, 过期时刻) 的表。
//
// 为什么不维护"用户 → token"的反向索引：会话数是个位数，遍历足够便宜；
// 而两套索引必须严格同步维护，是 bug 的温床（webauth 里也因此只留一张表）。
type userSessionStore struct {
	mu       sync.Mutex
	sessions map[string]userSession
}

func newUserSessionStore() *userSessionStore {
	return &userSessionStore{sessions: map[string]userSession{}}
}

// loginSessions 是**进程级**的用户登录态表。
//
// 为什么是包级变量而不是 Server 的字段：Server 的字段定义在 server.go，
// 而本次改动的边界只允许动 server.go 的**路由注册部分**（见任务边界）。
// 语义上也说得通：会话本来就是进程级状态，一个进程里也只有一个 Server
// （webAuth 同样是每进程一份）。token 是 256 位随机值，跨 Server 实例碰撞的概率可忽略。
var loginSessions = newUserSessionStore()

// new 签发一个新的会话 token。
func (st *userSessionStore) new(userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败是严重问题，绝不降级到弱随机（与 webauth.NewSession 一致）
		return "", fmt.Errorf("生成会话 token: %w", err)
	}
	token := hex.EncodeToString(b)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reapLocked()
	st.sessions[token] = userSession{userID: userID, expires: time.Now().Add(webSessionTTL)}
	return token, nil
}

// userID 解析 token 对应的用户 ID；无效或已过期返回 false。
func (st *userSessionStore) userID(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[token]
	if !ok {
		return "", false
	}
	if time.Now().After(s.expires) {
		delete(st.sessions, token)
		return "", false
	}
	return s.userID, true
}

// drop 注销单个 token。
func (st *userSessionStore) drop(token string) {
	if token == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, token)
}

// dropUser 作废某用户的**全部**会话，返回作废条数（诊断用）。
//
// 改密后必须调它：改密最常见的动机就是"怀疑有人拿到了我的会话"，
// 此时让旧会话继续有效等于没改。
func (st *userSessionStore) dropUser(userID string) int {
	if userID == "" {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for t, s := range st.sessions {
		if s.userID == userID {
			delete(st.sessions, t)
			n++
		}
	}
	return n
}

// count 返回有效会话数（诊断用）。
func (st *userSessionStore) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reapLocked()
	return len(st.sessions)
}

// reapLocked 惰性清理过期会话（不另起 goroutine，与 webauth 一致）。
func (st *userSessionStore) reapLocked() {
	now := time.Now()
	for t, s := range st.sessions {
		if now.After(s.expires) {
			delete(st.sessions, t)
		}
	}
}

// ---- cookie 与身份解析 ----

// setSessionCookie 下发登录态 cookie。
//
// 属性与控制台门禁 cookie 保持一致：HttpOnly（JS 读不到，降低 XSS 窃取风险）、
// SameSite=Lax（挡掉跨站 POST 携带，CSRF 的基本防线）、Path=/。
// 不设 Secure：本地自托管默认跑 http，设了会让 cookie 在 http 下直接失效。
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(webSessionTTL.Seconds()),
	})
}

// clearSessionCookie 让浏览器丢掉登录态 cookie。
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// sessionUser 从 cookie 解析出当前登录用户；无有效登录态时返回 nil。
//
// 除了"token 有没有"，还要再查一次用户本身，因为会话表里只有 ID：
//   - 用户**已被删除** → 会话成了孤儿，必须失效（否则删掉的人还能改 Key）
//   - 用户**已被禁用** → 立即失去登录态（管理员禁用账号应当立刻生效）
//
// 这两种情况顺手把 cookie 清掉，免得客户端拿着一个永远无效的 cookie 反复重试。
func (s *Server) sessionUser(w http.ResponseWriter, r *http.Request) *usermgr.User {
	if s.users == nil {
		return nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	id, ok := loginSessions.userID(c.Value)
	if !ok {
		return nil
	}
	u, ok := s.users.ByID(id)
	if !ok || u.Disabled {
		loginSessions.drop(c.Value)
		clearSessionCookie(w)
		return nil
	}
	return u
}

// requireUsers 在"未启用用户体系"时写出 503 并返回 false。
//
// 用 503 而不是 500：这是"功能没启用"（配置层），不是"服务出错"。
// 生产路径上 s.users 一定非 nil（New 里无条件创建），这是给直接构造的
// Server（测试、嵌入式用法）兜底，避免一个 nil 指针把请求打成 panic。
func (s *Server) requireUsers(w http.ResponseWriter) bool {
	if s.users != nil {
		return true
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "users_disabled",
		"未启用用户体系（没有用户文件）")
	return false
}

// ---- 登录限流（与控制台登录共用同一套状态）----

// loginAllowed 在尝试登录**之前**检查该 IP 是否已被锁定。
func (s *Server) loginAllowed(w http.ResponseWriter, r *http.Request) bool {
	ip := clientIP(r, s.trustProxyHeaders())
	// loginLimit 为 nil 时 Allow 内部会返回"允许"（见 security.go），无需额外判空
	if ok, remain := s.loginLimit.Allow(ip, time.Now()); !ok {
		s.logf("WARN: 登录已被限流（IP %s，还需 %.0f 秒）", ip, remain.Seconds())
		writeOpenAIError(w, http.StatusTooManyRequests, "too_many_attempts",
			fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(remain.Minutes())+1))
		return false
	}
	return true
}

// noteLoginFailure 记一次登录失败；返回 true 表示"刚触发锁定"（已写出 429）。
func (s *Server) noteLoginFailure(w http.ResponseWriter, r *http.Request, reason string) bool {
	ip := clientIP(r, s.trustProxyHeaders())
	locked, lockFor := s.loginLimit.NoteFailure(ip, time.Now())
	s.logf("WARN: 登录失败（%s）来自 %s", reason, ip)
	if locked {
		s.logf("WARN: 登录连续失败达阈值，已锁定 IP %s（%s）", ip, lockFor)
		writeOpenAIError(w, http.StatusTooManyRequests, "too_many_attempts",
			fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(lockFor.Minutes())+1))
		return true
	}
	return false
}

// failLogin 统一处理登录失败：计入按 IP 限流 + 回**不区分原因**的 401。
//
// ## 为什么响应不区分"用户名不存在 / 密码错 / 没设密码"
//
// 区分它们等于免费送出一个**用户名枚举**接口：攻击者先批量试一遍用户名，
// 拿到"存在"的清单，再对这些账号集中爆破 —— 攻击面从"所有人"缩到"真实用户"。
// 所以对外永远只有一句话：`用户名或密码错误`。
//
// 真正的原因只写进**服务端日志**（运维需要排查"为什么用户登不进来"）。
// 另外"用户名不存在"这条路径会先付出一次等价的 PBKDF2 代价（BurnPasswordTime），
// 否则**响应快慢**又会把答案泄露出去（见 usermgr.BurnPasswordTime）。
func (s *Server) failLogin(w http.ResponseWriter, r *http.Request, reason string) {
	if s.noteLoginFailure(w, r, reason) {
		return // 已回 429
	}
	writeOpenAIError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
}

// ---- 端点 ----

// handleSystemInfo GET /admin/system
//
// ⚠️ 这个端点是**公开的**（故意不挂 withAuth）。
//
// 为什么必须公开：登录/注册弹窗要在**还没有任何凭证**的时候就知道要不要显示
// "注册"入口。它透出的只有版本、服务名与开放注册开关 ——
// 版本与服务名 /healthz 早就公开了，开放注册开关也必须公开（否则页面无法渲染）。
// 真正的门控在 /auth/register 里（服务端二次判定，**不靠前端隐藏按钮**）。
func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"allow_register": s.allowRegister(),
		"version":        Version,
		"service":        ServiceName,
	})
}

// allowRegister 读取开放注册开关。
//
// fail-closed：读不到 / 未启用用户体系时一律 false。
// "注册被误关"只是不方便，"注册被误开"是任何人都能给自己开一个账号。
func (s *Server) allowRegister() bool {
	return s.users != nil && s.users.AllowRegister()
}

// handleSetAllowRegister POST /admin/system/allow_register   （需管理员）
//
// body: {"enabled":true}  →  200 {"allow_register":true}
func (s *Server) handleSetAllowRegister(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if !s.requireUsers(w) {
		return
	}
	// 用 *bool 而不是 bool：必须能区分"没传 enabled"与"传了 enabled:false"。
	// 若用 bool，一个漏传字段的请求会被静默当成"关闭注册" —— 那是个
	// 由拼写错误触发的安全配置变更，不该发生。
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`缺少 enabled 字段（true = 开放注册 / false = 关闭注册）`)
		return
	}
	if err := s.users.SetAllowRegister(*req.Enabled); err != nil {
		// 内存已生效、落盘失败：如实返回状态 + 警告，而不是谎报 500（设置其实变了）
		s.logf("WARN: 开放注册设置落盘失败: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"allow_register": s.users.AllowRegister(),
			"warning":        "设置已在内存中生效，但落盘失败（重启后会恢复为上一次成功保存的值）：" + err.Error(),
		})
		return
	}
	if *req.Enabled {
		s.logf("INFO: 自助注册已**开启**（任何访问者都能注册账号）")
	} else {
		s.logf("INFO: 自助注册已关闭")
	}
	writeJSON(w, http.StatusOK, map[string]any{"allow_register": s.users.AllowRegister()})
}

// handleRegister POST /auth/register   （公开，受 allow_register 门控）
//
// body: {"username":"alice","password":"至少6位"}
// 200 : {"id","username","role","api_key"}
// 403 : register_disabled
// 409 : 用户名已存在
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	// 门控**必须在服务端**：前端把"注册"按钮藏起来只是 UI，不是访问控制。
	if !s.allowRegister() {
		writeOpenAIError(w, http.StatusForbidden, "register_disabled",
			"系统当前已关闭自助注册，请联系管理员开通")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	username := strings.TrimSpace(req.Username)
	if err := validateUsername(username); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_username", err.Error())
		return
	}
	// 密码先校验（CreateWithPassword 内部也会再校验一次）：
	// 让"密码太短"这种最常见的错误在**创建任何东西之前**就被拒掉。
	if err := usermgr.ValidatePassword(req.Password); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}
	// 角色固定 user。**首个用户必须是管理员**这条防自锁规则由 usermgr 把关，
	// 自助注册不给它开例外（详见 usermgr.create 的注释）。
	u, err := s.users.CreateWithPassword(username, usermgr.RoleUser, req.Password, "自助注册")
	if err != nil && u == nil {
		writeOpenAIError(w, registerErrStatus(err), registerErrCode(err), err.Error())
		return
	}
	s.logf("INFO: 新用户已自助注册：%s（%s）", u.Username, u.ID)
	resp := map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role, "api_key": u.APIKey,
		"warning": "API Key 仅本次返回明文，请立即保存；忘记可在用户页重置。",
	}
	if err != nil {
		// 内存已生效、落盘失败：与 users.go 的 createUser 同一处理（不谎报失败）
		resp["warning"] = "账号已在内存中创建，但落盘失败（重启后会丢失）：" + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// registerErrStatus 把 usermgr 的错误映射成 HTTP 状态码。
func registerErrStatus(err error) int {
	switch {
	case errors.Is(err, usermgr.ErrUserExists):
		return http.StatusConflict // 契约要求 409
	case strings.Contains(err.Error(), "落盘失败"):
		return http.StatusInternalServerError
	default:
		// 含"首个用户必须是管理员"（请求本身没错，是系统状态不允许）
		return http.StatusBadRequest
	}
}

// registerErrCode 给错误一个机器可读的 code（前端据此定位提示文案）。
func registerErrCode(err error) string {
	switch {
	case errors.Is(err, usermgr.ErrUserExists):
		return "username_exists"
	case strings.Contains(err.Error(), "首个用户必须是管理员"):
		return "first_user_must_be_admin"
	case strings.Contains(err.Error(), "落盘失败"):
		return "persist_failed"
	default:
		return "register_failed"
	}
}

// validateUsername 校验注册用户名。
//
// 规则：3–32 个**字符**，且不含空白 / 控制字符 / `:` `/` `\`。
//
// 为什么禁 `:`：会话粘性键是 `owner + ":" + sessionID` 拼出来的（见 sessionKey），
// 用户名里带 `:` 会让不同用户的键有几率撞在一起（会话串台）。
// 为什么禁 `/` `\`：用户名会出现在 URL 路径与日志里，带路径分隔符容易引发
// 转义/拼接类问题，收益为零。
//
// ⚠️ 这条规则只作用于**自助注册**；管理员用 /admin/users 建用户不受它限制
// （那是既有行为，本次不改，避免影响已有用户）。
func validateUsername(name string) error {
	n := len([]rune(name))
	if n < 3 || n > 32 {
		return fmt.Errorf("用户名长度须在 3–32 个字符之间（当前 %d）", n)
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == ':' || r == '/' || r == '\\' {
			return fmt.Errorf("用户名不能包含空白、控制字符或 : / \\")
		}
	}
	return nil
}

// handleLogin POST /auth/login   （公开）
//
// body: {"username":"alice","password":"..."}
// 200 : {"id","username","role","api_key"} + Set-Cookie: trae2api_session
// 401 : 用户名或密码错误（**不区分**两者）
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 先查限流：被锁定的 IP 不该有机会消耗 PBKDF2 的 CPU
	if !s.loginAllowed(w, r) {
		return
	}
	username := strings.TrimSpace(req.Username)
	u, ok := s.users.ByUsername(username)
	if !ok {
		// 用户不存在也要付出**等价**的派生代价，否则响应耗时就是用户名枚举的信道
		usermgr.BurnPasswordTime(req.Password)
		s.failLogin(w, r, "用户名不存在")
		return
	}
	if !s.users.CheckPassword(u.ID, req.Password) {
		// 没设密码的老用户也走这里（VerifyPassword 对空哈希返回 false）——
		// 他们只能用 API Key，不会被"默认密码"悄悄放进来。
		s.failLogin(w, r, "密码错误或该用户未设密码")
		return
	}
	if u.Disabled {
		s.failLogin(w, r, "账号已被禁用")
		return
	}

	s.loginLimit.NoteSuccess(clientIP(r, s.trustProxyHeaders()))
	token, err := loginSessions.new(u.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "session_failed", err.Error())
		return
	}
	setSessionCookie(w, token)
	s.logf("INFO: 用户 %s 登录成功（来自 %s）", u.Username, r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role, "api_key": u.APIKey,
	})
}

// handleLogout POST /auth/logout   （公开，幂等）
//
// 没有登录态也返回 200：登出是幂等操作，"我已经不在登录态了"本来就是期望结果。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		loginSessions.drop(c.Value)
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMePassword POST /me/password   （需登录）
//
// body: {"old_password":"...","new_password":"..."}
// 200 : {"ok":true}
// 401 : 原密码错误 / 未登录
//
// 改密成功后**作废该用户的全部会话**（含当前这个），用户需用新密码重新登录。
// 这是刻意的：改密最常见的动机就是"怀疑会话被别人拿到了"，
// 此时留着旧会话（哪怕是当前这个）等于没改。
func (s *Server) handleMePassword(w http.ResponseWriter, r *http.Request) {
	u := s.sessionUser(w, r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "not_logged_in", "请先登录")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.loginAllowed(w, r) {
		return
	}
	if !s.users.CheckPassword(u.ID, req.OldPassword) {
		// 与登录失败共用同一套限流状态：否则这个端点会变成一个**不限速**的
		// 密码爆破入口（拿到一个有效会话后可以无限次试原密码）。
		if s.noteLoginFailure(w, r, "自助改密时原密码错误") {
			return
		}
		writeOpenAIError(w, http.StatusUnauthorized, "wrong_password", "原密码不正确")
		return
	}
	if err := s.users.SetPassword(u.ID, req.NewPassword); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "落盘失败") {
			status = http.StatusInternalServerError
		}
		writeOpenAIError(w, status, "invalid_password", err.Error())
		return
	}
	s.loginLimit.NoteSuccess(clientIP(r, s.trustProxyHeaders()))
	n := loginSessions.dropUser(u.ID)
	clearSessionCookie(w)
	s.logf("INFO: 用户 %s 已自助改密，作废会话 %d 个（含当前会话）", u.Username, n)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"hint": "密码已更新；所有旧会话（含当前这个）已作废，请用新密码重新登录。",
	})
}

// handleMeAPIKeyReset POST /me/api-key/reset   （需登录）
//
// 200 : {"api_key":"sk-trae-新值","warning":"..."}
//
// 与管理员端点 /admin/users/{id}/reset-key 的区别只是**授权来源**：
// 这里凭自己的登录态换自己的 Key，不需要管理员。
func (s *Server) handleMeAPIKeyReset(w http.ResponseWriter, r *http.Request) {
	u := s.sessionUser(w, r)
	if u == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "not_logged_in", "请先登录")
		return
	}
	nu, err := s.users.ResetKey(u.ID)
	if nu == nil {
		// 会话有效但用户没了（并发删除）：按登录态失效处理
		loginSessions.dropUser(u.ID)
		clearSessionCookie(w)
		writeOpenAIError(w, http.StatusUnauthorized, "not_logged_in", "用户已不存在，请重新登录")
		return
	}
	if err != nil {
		// 内存已生效、落盘失败：Key 已经变了，必须把它给用户（否则新 Key 丢失）
		s.logf("WARN: %v", err)
	}
	s.logf("INFO: 用户 %s 自助重置了 API Key", u.Username)
	resp := map[string]any{
		"api_key": nu.APIKey,
		"warning": "旧 API Key 已立即失效；新 Key 仅本次返回明文，请立即保存。",
	}
	if err != nil {
		resp["warning"] = "旧 API Key 已立即失效；新 Key 仅本次返回明文。" +
			"注意：落盘失败（重启后会恢复成旧 Key）：" + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUserResetPassword POST /admin/users/{id}/reset-password   （需管理员）
//
// body: {"password":"..."}  →  200 {"id","username"}
//
// 这是"老用户平滑过渡"的入口：升级前创建的账号没有密码，
// 管理员在这里给他们设一个，他们才能开始用密码登录。
func (s *Server) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if !s.requireUsers(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少用户 id")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, ok := s.users.ByID(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if err := s.users.SetPassword(id, req.Password); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "落盘失败") {
			status = http.StatusInternalServerError
		}
		writeOpenAIError(w, status, "set_password_failed", err.Error())
		return
	}
	// 管理员代改密码同样作废该用户的旧会话：
	// "我怀疑这个账号被盗了，帮他改个密码"是最常见的动机，
	// 若旧会话还活着，这个操作等于没做。
	n := loginSessions.dropUser(id)
	s.logf("INFO: 管理员重置了用户 %s 的密码，作废会话 %d 个", u.Username, n)
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "username": u.Username})
}
