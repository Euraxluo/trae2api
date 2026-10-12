package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"trae2api/internal/traectl"
)

// 本文件实现 OAuth 一键登录的 HTTP 层。
//
// 设计要点（其中第一条来自对 workbuddy2api 的复盘）：
//
//  1. **轮询接口恒返 200**。用 state 字段（waiting/success/error）表达状态，
//     绝不因为一次内部错误返回 5xx —— 前端轮询会因为一次 5xx 而中断，
//     用户就看不到"到底卡在哪一步"了。
//  2. 会话在**成功的那一刻**就落盘成便携快照并注册进池，不依赖前端来"确认保存"。
//     前端可能被关掉，但凭据不该因此丢失。
//  3. 登录会生成**新的设备密钥对**，服务端把它与账号绑定。所以登录产物必须
//     整份保存（含私钥），不能只存 token。

// loginManager 管理进行中的登录会话。
type loginManager struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

type loginSession struct {
	sess    *traectl.LoginSession
	product string
	started time.Time
	saved   bool
	saveErr string
	savedAs string
	userID  string
}

func newLoginManager() *loginManager {
	return &loginManager{sessions: map[string]*loginSession{}}
}

// reap 清理已结束且超过保留期的会话（惰性调用，不另起 goroutine）。
func (m *loginManager) reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, s := range m.sessions {
		st := s.sess.Status()
		state := st["state"]
		if (state == "success" || state == "error") && s.started.Before(cutoff) {
			s.sess.Close()
			delete(m.sessions, id)
		}
	}
}

func (m *loginManager) get(id string) *loginSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// handleLoginStart POST /admin/login/start
//
// body: {"product":"trae-cn"}（可选，默认 trae-cn）
// 返回：{"session_id":"...","url":"https://www.trae.cn/authorization?...","product":"..."}
//
// 调用方拿到 url 后让用户打开；然后轮询 /admin/login/status。
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Product     string `json:"product"`
		OpenBrowser *bool  `json:"open_browser"`
	}
	// body 可为空（用默认产品）
	if r.Body != nil {
		_ = decodeJSON(r, &req)
	}
	key := strings.TrimSpace(req.Product)
	if key == "" {
		key = "trae-cn"
	}
	prod, ok := traectl.ProductByKey(key)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_product",
			"未知产品 "+key+"（可选 trae-cn / solo-cn）")
		return
	}

	open := true
	if req.OpenBrowser != nil {
		open = *req.OpenBrowser
	}

	// 主机按**版本域**选：国内版与国际版是两套独立端点。
	// 用错的表现是"浏览器里登录成功了，但换 token 失败"。
	loginHost, apiHost := s.cfg.Login.LoginHost, s.cfg.Login.APIHost
	if prod.Edition == traectl.EditionIntl {
		apiHost = s.cfg.Intl.UGHost
		// 登录页主机**显式配置**，不从 UG 主机派生。
		//
		// 曾经的派生规则（把 `api.` 换成 `www.`）只在国内版成立：
		// 国际版的 UG 主机是 `ug-normal.trae.ai`，机械套用会得到
		// `www.ug-normal.trae.ai` —— 一个**不存在**的主机，表现为"登录页连不上"，
		// 而且看起来像网络问题。实测踩过这个坑。
		//
		// 仍留派生作为兜底：万一用户自定义了 UG 主机却没配 login_host。
		loginHost = s.cfg.Intl.LoginHost
		if loginHost == "" {
			loginHost = deriveLoginHost(s.cfg.Intl.UGHost)
		}
	}
	// 启动前先探一下登录页 —— 见 login_preflight.go 的完整说明。
	//
	// 这一步把"浏览器显示无法访问 + 网关干等 10 分钟后报含糊超时"
	// 变成"3 秒内返回一句可照做的错误"。
	//
	// 实测踩过：`www.trae.cn` 按区域封锁（volc-dcdn 返回 403「抱歉，您当前区域不支持访问」），
	// 而 `api.trae.cn` 不受影响 —— 所以网关读凭据/续期/调模型全都正常，
	// 只有登录页打不开，极易误判成"网关的 URL 拼错了"。
	if err := loginHostProblem(r.Context(), loginHost); err != nil {
		s.logf("WARN: OAuth 预检失败（产品 %s）: %v", prod.Label, err)
		writeOpenAIError(w, http.StatusBadGateway, "login_host_unreachable", err.Error())
		return
	}

	sess, err := traectl.StartLogin(prod, traectl.LoginOptions{
		LoginHost:   loginHost,
		APIHost:     apiHost,
		OpenBrowser: open,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "login_start_failed", err.Error())
		return
	}

	ls := &loginSession{sess: sess, product: prod.Key, started: time.Now()}
	s.logins.mu.Lock()
	s.logins.sessions[sess.ID] = ls
	s.logins.mu.Unlock()
	s.logins.reap()

	s.logf("INFO: 已启动 OAuth 登录会话 %s（产品 %s），等待浏览器授权", sess.ID, prod.Label)

	// 后台等待结果并在成功时落盘 —— 不依赖前端来"确认保存"。
	go s.completeLogin(ls)

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"url":        sess.URL,
		"product":    prod.Key,
		"expires_in": int(loginWaitTimeout.Seconds()),
		"hint":       "请在浏览器中完成登录；本页可关闭，网关会在授权完成后自动保存凭据",
	})
}

// deriveLoginHost 从 UG/API 主机派生登录页主机。
//
// 依据国内版的实测对应关系：api.trae.cn（API）↔ www.trae.cn（登录页），
// 即把 api. 前缀换成 www.。派生而不是加配置项：少一个要填的值，
// 且这个规则在 CN 上可验证（international 沿用它，用户也能一眼核对）。
func deriveLoginHost(apiHost string) string {
	h := strings.TrimSpace(apiHost)
	if h == "" {
		return ""
	}
	scheme := "https://"
	rest := h
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme, rest = rest[:i+3], rest[i+3:]
	}
	rest = strings.TrimSuffix(rest, "/")
	if strings.HasPrefix(rest, "api.") {
		rest = "www." + strings.TrimPrefix(rest, "api.")
	} else if !strings.HasPrefix(rest, "www.") {
		rest = "www." + rest
	}
	return scheme + rest
}

// loginWaitTimeout 是后台等待登录完成的上限。
const loginWaitTimeout = 10 * time.Minute

// completeLogin 后台等待登录结果并落盘。
func (s *Server) completeLogin(ls *loginSession) {
	ctx, cancel := context.WithTimeout(context.Background(), loginWaitTimeout)
	defer cancel()

	snap, err := ls.sess.Wait(ctx, loginWaitTimeout)
	if err != nil {
		s.logf("WARN: OAuth 登录会话 %s 未完成: %v", ls.sess.ID, err)
		return
	}
	if err := s.saveLoginSnapshot(ls, snap); err != nil {
		s.logf("WARN: OAuth 登录成功但保存失败: %v", err)
	}
}

// saveLoginSnapshot 把登录得到的凭据落盘成便携快照并注册进池。
func (s *Server) saveLoginSnapshot(ls *loginSession, snap *traectl.CredentialSnapshot) error {
	name, err := s.accounts.Add(snap)
	if err != nil {
		ls.saveErr = err.Error()
		return err
	}
	ls.saved = true
	ls.savedAs = name
	ls.userID = snap.UserID

	if err := s.registerAccount(snap, filepath.Join(s.accounts.Dir(), name)); err != nil {
		ls.saveErr = "凭据已保存但注册进池失败: " + err.Error()
		return err
	}
	_ = s.pool.Save()
	s.logf("INFO: OAuth 登录成功，账号 %s 已保存为 %s 并注册进池", snap.UserID, name)
	return nil
}

// handleLoginStatus GET /admin/login/status?session_id=...
//
// **恒返 200**（除非会话不存在）：用 state 字段表达进度，
// 让前端轮询不会因一次 5xx 而中断。
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := r.URL.Query().Get("session_id")
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 session_id")
		return
	}
	ls := s.logins.get(id)
	if ls == nil {
		// 会话不存在也返回 200 + state=unknown：前端据此提示"会话已过期"，
		// 而不是把一个 404 当成网络错误
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": id, "state": "unknown",
			"error": "会话不存在或已过期，请重新发起登录",
		})
		return
	}
	st := ls.sess.Status()
	// 补充落盘结果（前端需要知道"保存到哪个文件了"）
	st["saved"] = ls.saved
	if ls.savedAs != "" {
		st["saved_as"] = ls.savedAs
	}
	if ls.saveErr != "" {
		st["save_error"] = ls.saveErr
	}
	if ls.userID != "" {
		st["user_id"] = ls.userID
	}
	writeJSON(w, http.StatusOK, st)
}

// handleLoginCancel POST /admin/login/cancel?session_id=...
func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := r.URL.Query().Get("session_id")
	if id == "" {
		_ = decodeJSON(r, &struct {
			SessionID string `json:"session_id"`
		}{})
	}
	ls := s.logins.get(id)
	if ls == nil {
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": false, "reason": "会话不存在或已过期"})
		return
	}
	ls.sess.Close()
	s.logins.mu.Lock()
	delete(s.logins.sessions, id)
	s.logins.mu.Unlock()
	s.logf("INFO: 已取消 OAuth 登录会话 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true, "session_id": id})
}

// handleLoginList GET /admin/login/sessions —— 列出进行中的登录会话（诊断）。
func (s *Server) handleLoginList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	s.logins.reap()
	s.logins.mu.Lock()
	out := make([]map[string]any, 0, len(s.logins.sessions))
	for _, ls := range s.logins.sessions {
		st := ls.sess.Status()
		st["product"] = ls.product
		st["saved"] = ls.saved
		out = append(out, st)
	}
	s.logins.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out, "count": len(out)})
}
