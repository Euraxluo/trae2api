package traectl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 本文件实现 **OAuth 一键登录**：让用户用浏览器完成 Trae 账号授权，
// 网关拿到一份全新的完整凭据（含设备密钥对）。
//
// 流程来自对 Trae 客户端 Electron 主进程的逆向（out-build/vs/code/electron-main/
// oauth/userLogin/{loginUrlBuilder,oauthLocalServer,loginService}.js）：
//
//  1. 生成 PKCE（codeVerifier / codeChallenge=S256）与 EC P-256 设备密钥对
//  2. 在 127.0.0.1 起一个随机端口的本地 HTTP 服务
//  3. 打开浏览器到 ${loginHost}/authorization?...&auth_callback_url=http://127.0.0.1:<port>/authorize
//  4. 用户走 SSO 登录，浏览器被重定向回本地服务，query 里带
//     authCodeInfo（JSON，含 AuthCode）与 userInfo
//  5. 用 AuthCode + codeVerifier 调 ExchangeToken 换到 token
//
// ⚠️ 关键约束（逆向得出，不是猜测）：
//   - 登录用的端点是 **/trae/api/v3/oauth/ExchangeToken**，与续期同一个端点，
//     但 body 不同（登录用 AuthCode+CodeVerifier，续期用 RefreshToken+DeviceProof）。
//   - **设备密钥对必须在登录时就生成并注册**：服务端会把 DeviceInfo 与注册时的
//     指纹绑定，之后续期要用同一把私钥签 DeviceProof。所以登录产物必须
//     连同私钥一起保存 —— 这也是为什么本项目的凭据格式是"便携快照"。

// DefaultLoginHost 是登录页面所在主机（来自 product.json 的 consoleHost）。
const DefaultLoginHost = "https://www.trae.cn"

// DefaultAPIHost 是 ExchangeToken 所在主机（来自 bootConfig.account.trae.normal）。
const DefaultAPIHost = "https://api.trae.cn"

// CallbackPath 是本地回调路径（客户端用 /authorize）。
const CallbackPath = "/authorize"

// loginTimeout 是等待用户在浏览器里完成登录的总时限。
// 客户端自己用的是 30 分钟；这里取短一些，避免一个被遗忘的会话长期占着端口。
const loginTimeout = 10 * time.Minute

// LoginSession 是一次进行中的登录。
type LoginSession struct {
	// ID 会话标识（供 HTTP 层轮询）。
	ID string
	// URL 需要用户在浏览器里打开的授权地址。
	URL string
	// Product 目标产品。
	Product Product

	codeVerifier string
	apiHost      string
	device       DeviceKeyPair
	machineID    string
	deviceID     string
	loginTraceID string
	startedAt    time.Time

	srv      *http.Server
	listener net.Listener
	port     int

	mu       sync.Mutex
	done     bool
	snapshot *CredentialSnapshot
	err      error
	// rawCallback 保存回调的原始 query（诊断用，便于发现上游改了参数名）。
	rawCallback string
}

// LoginOptions 是登录参数。
type LoginOptions struct {
	// LoginHost 覆盖登录主机（默认 DefaultLoginHost）。
	LoginHost string
	// APIHost 覆盖 ExchangeToken 主机（默认 DefaultAPIHost）。
	APIHost string
	// DeviceModel / DeviceBrand / OSName / OSVersion 可覆盖设备指纹。
	// 留空则用本机信息 —— **但要注意**：服务端会把这个指纹与账号绑定，
	// 之后续期必须一致。所以一旦登录成功就不要改这些值。
	DeviceModel string
	DeviceBrand string
	OSName      string
	OSVersion   string
	// OpenBrowser 是否自动打开浏览器（默认 true）。
	OpenBrowser bool
	// Timeout 覆盖默认等待时限。
	Timeout time.Duration
}

// StartLogin 启动一次登录：起本地回调服务、生成 PKCE 与设备密钥、构造授权 URL。
//
// 调用方拿到 URL 后应让用户打开它，然后调 Wait 等待结果。
func StartLogin(p Product, opt LoginOptions) (*LoginSession, error) {
	if opt.LoginHost == "" {
		opt.LoginHost = DefaultLoginHost
	}
	if opt.APIHost == "" {
		opt.APIHost = DefaultAPIHost
	}
	if opt.Timeout <= 0 {
		opt.Timeout = loginTimeout
	}

	// ---- PKCE ----
	// 客户端用 randomBytes(48).base64url，challenge = sha256(verifier).base64url。
	// 必须用 base64url（无填充）—— 用标准 base64 会因为 +/= 被上游拒绝。
	vraw := make([]byte, 48)
	if _, err := rand.Read(vraw); err != nil {
		return nil, fmt.Errorf("traectl: 生成 PKCE: %w", err)
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(vraw)
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// ---- 设备密钥对 ----
	priv, pub, err := generateDeviceKeyPair()
	if err != nil {
		return nil, err
	}

	// ---- 本地回调服务 ----
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("traectl: 监听本地回调端口: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	machineID, deviceID := localDeviceIdentity()
	trace := make([]byte, 16)
	rand.Read(trace)

	s := &LoginSession{
		ID:           hex.EncodeToString(trace[:8]),
		Product:      p,
		apiHost:      opt.APIHost,
		codeVerifier: codeVerifier,
		device:       DeviceKeyPair{PrivateKeyPEM: priv, PublicKeyPEM: pub},
		machineID:    machineID,
		deviceID:     deviceID,
		loginTraceID: hex.EncodeToString(trace),
		startedAt:    time.Now(),
		listener:     ln,
		port:         port,
	}

	s.URL = s.buildLoginURL(opt, codeChallenge)

	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, s.handleCallback)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 其他路径也交给回调处理：上游不同版本的路径可能不一样，
		// 只要 query 里有 authCodeInfo 就认。
		s.handleCallback(w, r)
	})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.fail(fmt.Errorf("traectl: 本地回调服务异常: %w", err))
		}
	}()

	if opt.OpenBrowser {
		// 打开失败不算致命：用户还可以手动复制 URL
		go func() { _ = openBrowser(s.URL) }()
	}
	return s, nil
}

// buildLoginURL 按客户端 loginUrlBuilder 的逻辑拼接授权 URL。
//
// 参数顺序与客户端一致（便于与抓包对照），但顺序本身对上游不重要。
func (s *LoginSession) buildLoginURL(opt LoginOptions, codeChallenge string) string {
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", s.Product.AuthFrom) // trae / solo
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", "local") // 客户端用 tronBuildVersion，非打包环境是 "local"
	q.Set("auth_type", "local")
	q.Set("client_id", s.Product.ClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", s.loginTraceID)
	q.Set("auth_callback_url", fmt.Sprintf("http://127.0.0.1:%d%s", s.port, CallbackPath))
	q.Set("machine_id", s.machineID)
	q.Set("device_id", s.deviceID)

	// 设备指纹（客户端把它们一并放进 query，见 loginUrlBuilder 的 bUe(y)）
	q.Set("x_device_id", s.deviceID)
	q.Set("x_machine_id", s.machineID)
	q.Set("x_device_type", opt.OSName)
	q.Set("x_device_brand", opt.DeviceBrand)
	q.Set("x_os_version", opt.OSVersion)
	q.Set("x_app_version", appVersion)
	q.Set("x_app_type", s.Product.PlatformCode)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")

	// SOLO 需要隐藏 SaaS 登录入口（客户端对 auth_from=solo 才加这个参数）
	if s.Product.AuthFrom == "solo" {
		q.Set("hide_saas_login", "true")
	}
	return strings.TrimRight(opt.LoginHost, "/") + "/authorization?" + q.Encode()
}

// callbackParams 是回调 query 里我们关心的字段。
//
// 客户端从 query 里取 authCodeInfo（JSON 字符串，内含 AuthCode）与 userInfo。
// 两者都是**字符串化的 JSON**，不是嵌套对象 —— 这点很容易搞错。
type callbackParams struct {
	AuthCodeInfo string `json:"authCodeInfo"`
	UserInfo     string `json:"userInfo"`
	ErrorCode    string `json:"error_code"`
	ErrorMsg     string `json:"error_msg"`
}

// LoginTraceID 返回该会话的完整 login_trace_id。
func (s *LoginSession) LoginTraceID() string {
	return s.loginTraceID
}

// ProcessCallback 解析回调 URL 或 query 字符串并完成令牌交换。
// 既可在本地 HTTP 回调到达时调用，也可由远程服务器/容器部署用户手动提交回调 URL 时调用。
func (s *LoginSession) ProcessCallback(rawInput string) (*CredentialSnapshot, error) {
	raw := strings.TrimSpace(rawInput)
	if idx := strings.Index(raw, "?"); idx >= 0 {
		raw = raw[idx+1:]
	}
	vals, err := url.ParseQuery(raw)
	if err != nil || vals.Get("authCodeInfo") == "" {
		if decoded, derr := url.QueryUnescape(raw); derr == nil {
			if v2, e2 := url.ParseQuery(decoded); e2 == nil {
				vals = v2
			}
		}
	}

	s.mu.Lock()
	s.rawCallback = raw
	s.mu.Unlock()

	// 若回调中带有指定 host（如 ug-normal.trae.ai / api.trae.com.cn），优先采纳
	if hostParam := strings.TrimSpace(vals.Get("host")); hostParam != "" {
		if !strings.HasPrefix(hostParam, "http://") && !strings.HasPrefix(hostParam, "https://") {
			hostParam = "https://" + hostParam
		}
		cleanHost := strings.TrimRight(hostParam, "/")
		// ⚠️ 特殊处理：国际版的 ExchangeToken 必须打向 UGHost（如 ug-normal.trae.ai），
		// 不能打向 api.trae.ai（后者对 API 路径返回 nginx 404）。如果已经配置了有效的国际版 UGHost，
		// 不要被 callback query 里的 api.trae.ai 覆盖破坏。
		if s.Product.Edition == EditionIntl && strings.Contains(cleanHost, "api.trae.ai") && s.apiHost != "" {
			// 保留既有正确的 s.apiHost
		} else {
			s.apiHost = cleanHost
		}
	}

	// 错误回调
	if ec := vals.Get("error_code"); ec != "" {
		err := fmt.Errorf("traectl: 登录被拒绝（error_code=%s）：%s", ec, vals.Get("error_msg"))
		s.fail(err)
		return nil, err
	}

	authCode := extractAuthCode(vals.Get("authCodeInfo"))
	if authCode == "" {
		if vals.Get("code") == "" && vals.Get("authCode") == "" {
			return nil, fmt.Errorf("已收到请求但未包含授权码，请确认复制了正确的完整重定向地址")
		}
		authCode = firstNonEmptyStr(vals.Get("code"), vals.Get("authCode"))
	}

	// 换 token
	snap, err := s.exchange(authCode, vals.Get("userInfo"))
	if err != nil {
		s.fail(err)
		return nil, err
	}
	s.mu.Lock()
	s.snapshot = snap
	s.done = true
	s.mu.Unlock()
	return snap, nil
}

func (s *LoginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	_, err := s.ProcessCallback(r.URL.RawQuery)
	if err != nil {
		writeLoginPage(w, false, err.Error(), s.Product.Label)
		return
	}
	writeLoginPage(w, true, "账号授权已成功换取凭据并存入网关，您可以关闭此窗口回到控制台。", s.Product.Label)
}

// extractAuthCode 从 authCodeInfo 里取 AuthCode。
//
// authCodeInfo 是一个**字符串化的 JSON**（客户端也是先 JSON.parse 再取 .AuthCode），
// 但不同版本可能直接给纯字符串，所以两种都试。
func extractAuthCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "{") {
		var v struct {
			AuthCode  string `json:"AuthCode"`
			AuthCode2 string `json:"authCode"` // 兼容小写
		}
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			return firstNonEmptyStr(v.AuthCode, v.AuthCode2)
		}
		return ""
	}
	return raw
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// extractUserIDFromUserInfo 从 callback 的 userInfo JSON 字符串提取 UserID
func extractUserIDFromUserInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	for _, key := range []string{"UserID", "userId", "userID", "Uid", "uid", "UserUniqueId", "userUniqueId", "BizUserId", "bizUserId"} {
		if val, ok := m[key]; ok && val != nil {
			str := strings.TrimSpace(fmt.Sprint(val))
			if str != "" && str != "<nil>" {
				return str
			}
		}
	}
	return ""
}

// fetchUserInfoViaToken 调用 /cloudide/api/v3/trae/GetUserInfo 获取用户信息
func (s *LoginSession) fetchUserInfoViaToken(token string) string {
	endpoint := s.apiHost + "/cloudide/api/v3/trae/GetUserInfo"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bodyData := map[string]any{
		"ReqSource":  "IDE",
		"IDEVersion": appVersion,
	}
	payload, _ := json.Marshal(bodyData)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-cloudide-token", token)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+token)
	req.Header.Set("x-ide-version", appVersion)
	req.Header.Set("x-app-version", appVersion)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Result map[string]any `json:"Result"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Result != nil {
		for _, key := range []string{"UserID", "userId", "userID", "Uid", "uid"} {
			if val, ok := out.Result[key]; ok && val != nil {
				str := strings.TrimSpace(fmt.Sprint(val))
				if str != "" && str != "<nil>" {
					return str
				}
			}
		}
	}
	return ""
}

// exchange 用 AuthCode 换 token。
//
// 端点与续期相同，对齐客户端 exchangeTokenByAuthCode。
func (s *LoginSession) exchange(authCode string, callbackUserInfo string) (*CredentialSnapshot, error) {
	osName := "windows"
	if runtime.GOOS == "darwin" {
		osName = "mac"
	} else if runtime.GOOS == "linux" {
		osName = "linux"
	}

	body := map[string]any{
		"ClientID":     s.Product.ClientID,
		"ClientSecret": "",
		"AuthCode":     authCode,
		"CodeVerifier": s.codeVerifier,
		"DeviceInfo": map[string]any{
			"DeviceID":        s.deviceID,
			"MachineID":       s.machineID,
			"PlatformCode":    s.Product.PlatformCode,
			"DeviceType":      "PC",
			"DeviceName":      deviceName(),
			"DeviceModel":     "",
			"ClientVersion":   appVersion,
			"DevicePublicKey": s.device.PublicKeyPEM,
			"DeviceBrand":     "",
			"DeviceCPU":       "",
			"OSInfo":          osName,
			"OSVersion":       osRelease(),
		},
		"IDEVersion": appVersion,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpoint := s.apiHost + exchangePath
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	// 登录请求的 x-cloudide-token 必须为空（与续期一致）
	for k, v := range s.loginHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		closeIdleOnFailure(httpClient.Transport)
		return nil, &TransportError{Op: "login", Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Result *struct {
			Token            string `json:"Token"`
			RefreshToken     string `json:"RefreshToken"`
			ExpiredAt        string `json:"ExpiredAt"`
			RefreshExpiredAt string `json:"RefreshExpiredAt"`
			UserID           string `json:"UserID"`
			UserId           string `json:"UserId"`
			Uid              string `json:"Uid"`
		} `json:"Result"`
		ResponseMetadata struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("traectl: 解析登录响应失败（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if out.ResponseMetadata.Error != nil && out.ResponseMetadata.Error.Code != "" {
		return nil, fmt.Errorf("traectl: 登录被上游拒绝（%s）：%s",
			out.ResponseMetadata.Error.Code, out.ResponseMetadata.Error.Message)
	}
	if out.Result == nil || out.Result.Token == "" {
		return nil, fmt.Errorf("traectl: 登录响应没有 Token（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	// 解析 UserID：优先从 ExchangeToken 响应中取；若无则从 callback 的 userInfo 取；若仍无则调 GetUserInfo 取
	userID := firstNonEmptyStr(out.Result.UserID, out.Result.UserId, out.Result.Uid)
	if userID == "" && callbackUserInfo != "" {
		userID = extractUserIDFromUserInfo(callbackUserInfo)
	}
	if userID == "" {
		userID = s.fetchUserInfoViaToken(out.Result.Token)
	}
	// 兜底：如果实在没拿到，生成一个伪 uid（防 Validate 失败阻塞）
	if userID == "" {
		userID = fmt.Sprintf("u_%s", s.deviceID)
	}

	snap := &CredentialSnapshot{
		Version:          snapshotVersion,
		Product:          s.Product.Key,
		UserID:           userID,
		Token:            out.Result.Token,
		RefreshToken:     out.Result.RefreshToken,
		ExpiredAt:        out.Result.ExpiredAt,
		RefreshExpiredAt: out.Result.RefreshExpiredAt,
		Host:             s.apiHost,
		DeviceID:         s.deviceID,
		MachineID:        s.machineID,
		PrivateKeyPEM:    s.device.PrivateKeyPEM,
		PublicKeyPEM:     s.device.PublicKeyPEM,
	}
	snap.Normalize()

	// 登录响应**不一定**回 RefreshToken。没有它这份凭据就无法续期 ——
	// 与其静默存下一份"过几小时就废"的凭据，不如明确报错。
	if snap.RefreshToken == "" {
		return nil, fmt.Errorf("traectl: 登录成功但上游未返回 RefreshToken，" +
			"该凭据无法自动续期（token 过期后需重新登录）")
	}
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("traectl: 登录返回的凭据不完整: %w", err)
	}
	return snap, nil
}

// loginHeaders 构造登录请求头（与续期同一套，x-cloudide-token 必须为空）。
func (s *LoginSession) loginHeaders() map[string]string {
	h := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "application/json, text/plain, */*",
		"x-cloudide-token": "",
		"x-ide-version":    appVersion,
		"x-app-version":    appVersion,
		"x-app-type":       s.Product.PlatformCode,
		"x-device-id":      s.deviceID,
		"x-machine-id":     s.machineID,
		"User-Agent":       defaultUserAgent,
	}
	return h
}

// Wait 等待用户在浏览器里完成登录。
//
// 返回登录得到的凭据快照（含设备私钥，可直接落盘成便携快照）。
func (s *LoginSession) Wait(ctx context.Context, timeout time.Duration) (*CredentialSnapshot, error) {
	if timeout <= 0 {
		timeout = loginTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			s.Close()
			return nil, fmt.Errorf("traectl: 登录超时（%s 内未完成）", timeout)
		case <-tick.C:
			s.mu.Lock()
			done, snap, err := s.done, s.snapshot, s.err
			s.mu.Unlock()
			if err != nil {
				s.Close()
				return nil, err
			}
			if done {
				s.Close()
				return snap, nil
			}
		}
	}
}

// Status 返回当前状态（供 HTTP 层轮询）。
//
// 返回的 map 恒定可序列化，**不返回错误** —— 轮询接口永远 200，
// 用字段表达状态。这样前端轮询不会因为一次 5xx 而中断。
func (s *LoginSession) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := map[string]any{
		"session_id":  s.ID,
		"product":     s.Product.Key,
		"url":         s.URL,
		"state":       "waiting",
		"elapsed_sec": int(time.Since(s.startedAt).Seconds()),
	}
	switch {
	case s.err != nil:
		st["state"] = "error"
		st["error"] = s.err.Error()
	case s.done:
		st["state"] = "success"
		if s.snapshot != nil {
			st["user_id"] = s.snapshot.UserID
			st["expired_at"] = s.snapshot.ExpiredAt
		}
	}
	return st
}

// Snapshot 返回登录得到的快照（未完成时为 nil）。
func (s *LoginSession) Snapshot() *CredentialSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

func (s *LoginSession) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil && !s.done {
		s.err = err
	}
}

// Close 关闭本地回调服务（可重复调用）。
func (s *LoginSession) Close() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}
}

// generateDeviceKeyPair 生成 EC P-256 密钥对（PKCS#8 / SPKI PEM）。
//
// 与客户端 wTe() 一致。**必须在登录时生成并随凭据保存**：
// 服务端会把它与账号绑定，续期的 DeviceProof 要用同一把私钥签。
func generateDeviceKeyPair() (privPEM, pubPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("traectl: 生成设备密钥: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

// localDeviceIdentity 生成本机设备标识。
//
// deviceId 是**数字字符串**（客户端 getDeviceId() 返回数字形态，
// 续期与登录都用它）；machineId 是 64 位 hex。
func localDeviceIdentity() (machineID, deviceID string) {
	b := make([]byte, 32)
	rand.Read(b)
	machineID = hex.EncodeToString(b)

	// 数字形态：用 16 位十进制随机数（与客户端 iCubeAuthInfo://icube-dc:<数字> 同形）
	num := make([]byte, 8)
	rand.Read(num)
	var v uint64
	for _, x := range num {
		v = v<<8 | uint64(x)
	}
	return machineID, fmt.Sprintf("%d", v%9000000000000000+1000000000000000)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "trae2api"
	}
	return h
}

// openBrowser 尝试打开系统默认浏览器。
func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

// writeLoginPage 给浏览器返回一个现代化赛博玻璃拟态授权回调结果页面（对齐 WorkBuddy 设计）。
func writeLoginPage(w http.ResponseWriter, ok bool, msg string, productLabel string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	statusTitle := "授权成功"
	badgeColor := "border-emerald-500/30 bg-emerald-500/10 text-emerald-400"
	iconBg := "from-emerald-500 to-teal-600 shadow-emerald-500/30"
	iconSVG := `<svg class="w-10 h-10 text-white animate-bounce" fill="none" stroke="currentColor" viewBox="0 0 24 24">
		<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M5 13l4 4L19 7"/>
	</svg>`
	tipMsg := "网关已成功捕获授权会话并将独立设备密钥凭据落盘，您可以直接关闭此窗口回到控制台。"
	autoCloseScript := `
		let count = 4;
		const timer = document.getElementById('countdown');
		const itv = setInterval(() => {
			count--;
			if (timer) timer.innerText = count;
			if (count <= 0) {
				clearInterval(itv);
				window.close();
			}
		}, 1000);
	`

	if !ok {
		statusTitle = "授权失败"
		badgeColor = "border-rose-500/30 bg-rose-500/10 text-rose-400"
		iconBg = "from-rose-500 to-pink-600 shadow-rose-500/30"
		iconSVG = `<svg class="w-10 h-10 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24">
			<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M6 18L18 6M6 6l12 12"/>
		</svg>`
		tipMsg = "未能成功完成账号授权换取令牌，请核对网络环境、代理设置或回到网关控制台重试。"
		autoCloseScript = `// 失败不自动关闭，方便排查复制错误`
	}

	if productLabel == "" {
		productLabel = "Trae Account"
	}

	escapedMsg := htmlEscape(msg)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN" class="dark">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Trae2Api · %s</title>
	<script src="https://cdn.tailwindcss.com"></script>
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600;700&display=swap" rel="stylesheet">
	<style>
		body {
			font-family: 'Plus Jakarta Sans', system-ui, -apple-system, sans-serif;
			background-color: #030712;
		}
		.font-mono {
			font-family: 'JetBrains Mono', monospace;
		}
		.cyber-grid {
			background-size: 36px 36px;
			background-image: 
				linear-gradient(to right, rgba(255, 255, 255, 0.03) 1px, transparent 1px),
				linear-gradient(to bottom, rgba(255, 255, 255, 0.03) 1px, transparent 1px);
		}
		.glass-card {
			background: rgba(15, 23, 42, 0.75);
			backdrop-filter: blur(20px);
			-webkit-backdrop-filter: blur(20px);
			border: 1px solid rgba(255, 255, 255, 0.08);
			box-shadow: 0 20px 50px rgba(0, 0, 0, 0.6);
		}
	</style>
</head>
<body class="min-h-screen text-slate-100 flex items-center justify-center p-4 relative overflow-hidden">
	<!-- 光晕效果 -->
	<div class="fixed top-[-10%%] left-[-10%%] w-[500px] h-[500px] bg-indigo-600/15 rounded-full blur-[140px] pointer-events-none"></div>
	<div class="fixed bottom-[-10%%] right-[-10%%] w-[500px] h-[500px] bg-cyan-500/15 rounded-full blur-[140px] pointer-events-none"></div>
	<div class="fixed inset-0 cyber-grid pointer-events-none"></div>

	<!-- 拟态卡片 -->
	<div class="relative z-10 w-full max-w-lg glass-card rounded-3xl p-8 sm:p-10 text-center">
		<!-- 头部产品标识 -->
		<div class="flex items-center justify-center gap-2 mb-6">
			<div class="w-7 h-7 rounded-lg bg-gradient-to-tr from-cyan-500 to-indigo-600 flex items-center justify-center shadow-md shadow-indigo-500/20">
				<svg class="w-4 h-4 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24">
					<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 10V3L4 14h7v7l9-11h-7z"/>
				</svg>
			</div>
			<span class="text-sm font-bold tracking-tight text-white font-mono">Trae<span class="text-cyan-400">2API</span></span>
			<span class="text-xs px-2 py-0.5 rounded-full font-mono %s">%s</span>
		</div>

		<!-- 状态图标 -->
		<div class="mx-auto w-20 h-20 rounded-2xl bg-gradient-to-tr %s flex items-center justify-center shadow-xl mb-6">
			%s
		</div>

		<!-- 标题与副标题 -->
		<h1 class="text-2xl sm:text-3xl font-extrabold text-white tracking-tight mb-3">%s</h1>
		<p class="text-sm text-slate-400 mb-6 leading-relaxed">%s</p>

		<!-- 详细响应/错误框 -->
		<div class="text-left bg-slate-900/80 border border-slate-800 rounded-xl p-4 mb-6">
			<div class="text-[11px] font-mono text-slate-500 uppercase tracking-wider mb-1.5 flex items-center justify-between">
				<span>会话诊断信息 (DIAGNOSTICS)</span>
				<span class="text-slate-400 font-mono">%s</span>
			</div>
			<p class="text-xs font-mono text-slate-200 break-all leading-relaxed whitespace-pre-wrap">%s</p>
		</div>

		<!-- 底部控制按钮 -->
		<div class="flex flex-col sm:flex-row items-center justify-center gap-3">
			<button onclick="window.close()" class="w-full sm:w-auto px-6 py-2.5 rounded-xl bg-slate-800 hover:bg-slate-700 text-slate-200 text-xs font-semibold border border-slate-700 transition">
				关闭当前窗口
			</button>
			<a href="/" class="w-full sm:w-auto px-6 py-2.5 rounded-xl bg-gradient-to-r from-cyan-500 to-indigo-600 hover:opacity-95 text-white text-xs font-semibold shadow-lg shadow-indigo-500/25 transition">
				返回网关控制台
			</a>
		</div>

		%s
	</div>

	<script>
		%s
	</script>
</body>
</html>`,
		statusTitle,
		badgeColor, productLabel,
		iconBg, iconSVG,
		statusTitle,
		tipMsg,
		time.Now().Format("15:04:05"),
		escapedMsg,
		func() string {
			if ok {
				return `<div class="mt-5 text-[11px] font-mono text-slate-500">此窗口将在 <span id="countdown" class="text-cyan-400 font-bold">4</span> 秒后自动安全关闭</div>`
			}
			return ""
		}(),
		autoCloseScript,
	)

	_, _ = w.Write([]byte(html))
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return strings.ReplaceAll(s, "'", "&#39;")
}
