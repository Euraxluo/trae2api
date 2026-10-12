package traectl

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// startTestLogin 起一个登录会话（不打开浏览器），返回会话与清理函数。
func startTestLogin(t *testing.T, p Product, apiHost string) *LoginSession {
	t.Helper()
	s, err := StartLogin(p, LoginOptions{
		LoginHost:   "https://www.trae.cn",
		APIHost:     apiHost,
		OpenBrowser: false,
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestLoginURLContainsRequiredParams 授权 URL 必须带齐客户端要求的参数。
//
// 这些参数名是从客户端 loginUrlBuilder 里逆出来的，写错任何一个
// 上游都会拒绝（或静默走到错误的登录分支）。
func TestLoginURLContainsRequiredParams(t *testing.T) {
	p := Products[0] // trae-cn
	s := startTestLogin(t, p, "http://127.0.0.1:1")

	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("URL 不合法: %v", err)
	}
	if u.Path != "/authorization" {
		t.Errorf("path = %q want /authorization", u.Path)
	}
	q := u.Query()
	want := map[string]string{
		"login_version":         "1",
		"auth_from":             "trae",
		"login_channel":         "native_ide",
		"plugin_version":        "local",
		"auth_type":             "local",
		"client_id":             p.ClientID,
		"redirect":              "0",
		"code_challenge_method": "S256",
		"x_app_version":         appVersion,
		"x_app_type":            "IDE_PC",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("参数 %s = %q want %q", k, got, v)
		}
	}
	// 必须有非空的关键参数
	for _, k := range []string{"login_trace_id", "auth_callback_url", "machine_id",
		"device_id", "code_challenge", "x_device_id", "x_machine_id"} {
		if q.Get(k) == "" {
			t.Errorf("参数 %s 缺失或为空", k)
		}
	}
	// 回调地址必须指向本地随机端口
	cb := q.Get("auth_callback_url")
	if !strings.HasPrefix(cb, "http://127.0.0.1:") || !strings.HasSuffix(cb, "/authorize") {
		t.Errorf("auth_callback_url = %q 形状不对", cb)
	}
	if strings.Contains(cb, ":0/") {
		t.Errorf("端口没取到真实值: %q", cb)
	}
	// trae 侧不应有 hide_saas_login（那是 solo 专属）
	if q.Get("hide_saas_login") != "" {
		t.Error("trae 不应带 hide_saas_login")
	}
}

// TestLoginURLSoloAddsHideSaas SOLO 必须带 hide_saas_login=true 且 auth_from=solo。
func TestLoginURLSoloAddsHideSaas(t *testing.T) {
	p := Products[1] // solo-cn
	s := startTestLogin(t, p, "http://127.0.0.1:1")

	q, _ := url.Parse(s.URL)
	vals := q.Query()
	if vals.Get("auth_from") != "solo" {
		t.Errorf("auth_from = %q want solo", vals.Get("auth_from"))
	}
	if vals.Get("hide_saas_login") != "true" {
		t.Errorf("solo 应带 hide_saas_login=true，得到 %q", vals.Get("hide_saas_login"))
	}
	if vals.Get("x_app_type") != "SOLO_PC" {
		t.Errorf("x_app_type = %q want SOLO_PC", vals.Get("x_app_type"))
	}
	if vals.Get("client_id") != "en1oxy7wnw8j9n" {
		t.Errorf("client_id = %q", vals.Get("client_id"))
	}
}

// TestPKCEChallengeMatchesVerifier code_challenge 必须等于
// base64url(sha256(code_verifier))，且用 **无填充** base64url。
//
// 用标准 base64 会因为 +/= 被上游拒绝，而报错通常很含糊。
func TestPKCEChallengeMatchesVerifier(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")
	q, _ := url.Parse(s.URL)
	vals := q.Query()

	verifier := s.codeVerifier
	challenge := vals.Get("code_challenge")
	if verifier == "" || challenge == "" {
		t.Fatal("verifier/challenge 为空")
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Errorf("challenge 与 verifier 不匹配\n got %s\nwant %s", challenge, want)
	}
	// 必须是 URL 安全字符集且无填充
	if strings.ContainsAny(challenge, "+/=") {
		t.Errorf("challenge 含非 URL 安全字符或填充: %q", challenge)
	}
	if strings.ContainsAny(verifier, "+/=") {
		t.Errorf("verifier 含非 URL 安全字符或填充: %q", verifier)
	}
	// verifier 长度：48 字节 base64url ≈ 64 字符
	if len(verifier) < 40 {
		t.Errorf("verifier 太短（%d），客户端用的是 48 字节随机数", len(verifier))
	}
}

// TestDeviceKeyPairUsable 生成的设备密钥必须能真的用来签名 DeviceProof。
//
// 这是登录最关键的不变量：私钥在登录时生成并随凭据保存，
// 之后续期要用它签 DeviceProof。签不了的话凭据过几小时就废。
func TestDeviceKeyPairUsable(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")
	if s.device.PrivateKeyPEM == "" || s.device.PublicKeyPEM == "" {
		t.Fatal("设备密钥对为空")
	}
	// 用它与续期同一套逻辑签一次
	proof, err := signDeviceProof(Products[0].ClientID, "dummy-refresh-token", s.device.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("登录时生成的私钥无法签 DeviceProof: %v", err)
	}
	if proof == nil || proof.Signature == "" {
		t.Error("签名为空")
	}
	if proof.Timestamp == 0 || proof.Nonce == "" {
		t.Error("DeviceProof 缺 Timestamp/Nonce（上游会拒）")
	}
	// 公钥必须与私钥配对（能解析且是 EC）
	if _, err := parseECPrivateKey(s.device.PrivateKeyPEM); err != nil {
		t.Errorf("私钥无法解析: %v", err)
	}
}

// TestDeviceIDsWellFormed deviceId 必须是**数字字符串**（客户端形态），
// machineId 是 hex。写错形态会让上游的 DeviceInfo 校验失败。
func TestDeviceIDsWellFormed(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")
	if s.deviceID == "" || s.machineID == "" {
		t.Fatal("设备标识为空")
	}
	for _, c := range s.deviceID {
		if c < '0' || c > '9' {
			t.Fatalf("deviceId 必须是纯数字，得到 %q", s.deviceID)
		}
	}
	if len(s.machineID) != 64 {
		t.Errorf("machineId 长度 = %d want 64（32 字节 hex）", len(s.machineID))
	}
	if strings.ToLower(s.machineID) != s.machineID {
		t.Error("machineId 应为小写 hex")
	}
}

// TestExtractAuthCode 兼容 authCodeInfo 的多种形态。
func TestExtractAuthCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"AuthCode":"abc123"}`, "abc123"},
		{`{"authCode":"xyz"}`, "xyz"},
		{`{"AuthCode":"","authCode":"fallback"}`, "fallback"},
		{`plaincode`, "plaincode"},
		{`  {"AuthCode":"padded"}  `, "padded"},
		{``, ""},
		{`{}`, ""},
		{`{bad json`, ""}, // 声明是 JSON 但解析失败 → 空（不把坏 JSON 当 code）
	}
	for _, c := range cases {
		if got := extractAuthCode(c.in); got != c.want {
			t.Errorf("extractAuthCode(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// TestCallbackSuccess 完整走一遍回调：本地服务收到授权码 → 换 token → 会话成功。
func TestCallbackSuccess(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exchangePath {
			t.Errorf("路径 = %q want %q", r.URL.Path, exchangePath)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"Result":{"Token":"NEW-TOKEN","RefreshToken":"NEW-REFRESH",
			"ExpiredAt":"2026-12-01T00:00:00.000Z","RefreshExpiredAt":"2027-06-01T00:00:00.000Z",
			"UserID":"916348159159136"}}`)
	}))
	defer srv.Close()

	p := Products[0]
	s := startTestLogin(t, p, srv.URL)

	// 模拟浏览器回调
	cb := fmt.Sprintf("http://127.0.0.1:%d%s?authCodeInfo=%s&userInfo=%s",
		s.port, CallbackPath,
		url.QueryEscape(`{"AuthCode":"AUTH-CODE-1"}`),
		url.QueryEscape(`{"userId":"916348159159136"}`))
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	resp.Body.Close()

	snap, err := s.Wait(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if snap.Token != "NEW-TOKEN" || snap.RefreshToken != "NEW-REFRESH" {
		t.Errorf("凭据不符: %+v", snap)
	}
	if snap.UserID != "916348159159136" {
		t.Errorf("userId = %q", snap.UserID)
	}
	// 设备密钥必须随凭据一起返回（否则无法续期）
	if snap.PrivateKeyPEM == "" || snap.PublicKeyPEM == "" {
		t.Error("快照缺少设备密钥 —— 该凭据无法续期")
	}
	if snap.DeviceID != s.deviceID || snap.MachineID != s.machineID {
		t.Error("快照的设备标识与会话不一致")
	}
	if err := snap.Validate(); err != nil {
		t.Errorf("快照未通过校验: %v", err)
	}

	// 请求体形状：必须带 AuthCode/CodeVerifier/DeviceInfo/IDEVersion，且**没有** DeviceProof
	if gotBody["AuthCode"] != "AUTH-CODE-1" {
		t.Errorf("AuthCode 未透传: %v", gotBody["AuthCode"])
	}
	if gotBody["CodeVerifier"] != s.codeVerifier {
		t.Error("CodeVerifier 未透传（PKCE 会失败）")
	}
	if gotBody["ClientID"] != p.ClientID {
		t.Errorf("ClientID = %v", gotBody["ClientID"])
	}
	if _, has := gotBody["DeviceProof"]; has {
		t.Error("登录请求不应带 DeviceProof（那是续期用的）")
	}
	di, ok := gotBody["DeviceInfo"].(map[string]any)
	if !ok {
		t.Fatalf("DeviceInfo 缺失或类型不对: %T", gotBody["DeviceInfo"])
	}
	for _, k := range []string{"DeviceID", "MachineID", "PlatformCode", "DevicePublicKey", "ClientVersion"} {
		if di[k] == nil || di[k] == "" {
			t.Errorf("DeviceInfo.%s 缺失", k)
		}
	}
	if di["PlatformCode"] != "IDE_PC" {
		t.Errorf("PlatformCode = %v", di["PlatformCode"])
	}
	if di["DevicePublicKey"] == "" {
		t.Error("DevicePublicKey 必须随登录一起注册（否则续期签名对不上）")
	}
}

// TestCallbackError 上游返回 error_code 时应失败而不是静默等待。
func TestCallbackError(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")

	cb := fmt.Sprintf("http://127.0.0.1:%d%s?error_code=20101&error_msg=%s",
		s.port, CallbackPath, url.QueryEscape("用户取消"))
	resp, _ := http.Get(cb)
	if resp != nil {
		resp.Body.Close()
	}

	_, err := s.Wait(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "20101") {
		t.Errorf("错误信息应含上游错误码: %v", err)
	}
}

// TestMissingRefreshTokenIsRejected 上游没回 RefreshToken 时必须明确报错。
//
// 为什么：没有 refreshToken 的凭据**过几小时就会失效**。
// 静默存下一份"看起来能用"的凭据，比当场报错糟得多。
func TestMissingRefreshTokenIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Result":{"Token":"T","UserID":"u1","ExpiredAt":"2026-12-01T00:00:00.000Z"}}`)
	}))
	defer srv.Close()

	s := startTestLogin(t, Products[0], srv.URL)
	cb := fmt.Sprintf("http://127.0.0.1:%d%s?authCodeInfo=%s",
		s.port, CallbackPath, url.QueryEscape(`{"AuthCode":"c"}`))
	resp, _ := http.Get(cb)
	if resp != nil {
		resp.Body.Close()
	}

	_, err := s.Wait(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("缺 RefreshToken 时应报错")
	}
	if !strings.Contains(err.Error(), "RefreshToken") {
		t.Errorf("错误信息应点明缺 RefreshToken: %v", err)
	}
}

// TestUpstreamErrorCodeRejected 上游用 ResponseMetadata.Error 表达失败时也要报错。
func TestUpstreamErrorCodeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ResponseMetadata":{"Error":{"Code":"20403","Message":"auth code expired"}}}`)
	}))
	defer srv.Close()

	s := startTestLogin(t, Products[0], srv.URL)
	cb := fmt.Sprintf("http://127.0.0.1:%d%s?authCodeInfo=%s",
		s.port, CallbackPath, url.QueryEscape(`{"AuthCode":"c"}`))
	resp, _ := http.Get(cb)
	if resp != nil {
		resp.Body.Close()
	}

	_, err := s.Wait(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("上游错误码应导致失败")
	}
	if !strings.Contains(err.Error(), "20403") {
		t.Errorf("错误应含上游码: %v", err)
	}
}

// TestStatusNeverErrors 状态接口必须恒可序列化且不返回错误。
//
// 理由（来自对 workbuddy2api 的复盘）：轮询接口若返回 5xx，
// 前端轮询会中断，用户就看不到"到底卡在哪一步"。所以状态一律用字段表达。
func TestStatusNeverErrors(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")

	st := s.Status()
	if st["state"] != "waiting" {
		t.Errorf("初始状态 = %v want waiting", st["state"])
	}
	if st["session_id"] == "" || st["url"] == "" {
		t.Error("状态里应有 session_id 与 url")
	}
	// 必须能序列化
	if _, err := json.Marshal(st); err != nil {
		t.Fatalf("状态无法序列化: %v", err)
	}

	// 失败后状态仍可序列化，且带 error 字段
	s.fail(fmt.Errorf("boom"))
	st = s.Status()
	if st["state"] != "error" {
		t.Errorf("失败后状态 = %v want error", st["state"])
	}
	if !strings.Contains(fmt.Sprint(st["error"]), "boom") {
		t.Errorf("状态里应带错误原因: %v", st["error"])
	}
	if _, err := json.Marshal(st); err != nil {
		t.Fatalf("失败状态无法序列化: %v", err)
	}
}

// TestWaitTimeout 超时必须返回错误并释放端口（而不是永久挂着）。
func TestWaitTimeout(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")
	start := time.Now()
	_, err := s.Wait(context.Background(), 300*time.Millisecond)
	if err == nil {
		t.Fatal("应超时")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误应为超时: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时耗时过长: %s", elapsed)
	}
	// 端口应已释放：能重新监听同一端口
	// （不直接测端口，改为验证 Close 幂等）
	s.Close()
	s.Close()
}

// TestWaitContextCancel ctx 取消应立刻返回。
func TestWaitContextCancel(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	if _, err := s.Wait(ctx, 10*time.Second); err == nil {
		t.Fatal("应因 ctx 取消而返回错误")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("取消后返回过慢: %s", elapsed)
	}
}

// TestCallbackWrongPathStillHandled 上游版本差异导致路径不同时，
// 只要 query 里有授权码就应当被处理。
func TestCallbackWrongPathStillHandled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Result":{"Token":"T","RefreshToken":"R","UserID":"u1"}}`)
	}))
	defer srv.Close()

	s := startTestLogin(t, Products[0], srv.URL)
	// 用 /login-confirm 而不是 /authorize
	cb := fmt.Sprintf("http://127.0.0.1:%d/login-confirm?authCodeInfo=%s",
		s.port, url.QueryEscape(`{"AuthCode":"c"}`))
	resp, _ := http.Get(cb)
	if resp != nil {
		resp.Body.Close()
	}

	if _, err := s.Wait(context.Background(), 5*time.Second); err != nil {
		t.Errorf("非标准路径也应被处理: %v", err)
	}
}

// TestCallbackWithoutCodeKeepsWaiting 浏览器预检/图标请求不应让会话失败。
func TestCallbackWithoutCodeKeepsWaiting(t *testing.T) {
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")

	// 无授权码的请求（如 favicon）
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/favicon.ico", s.port))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	st := s.Status()
	if st["state"] != "waiting" {
		t.Errorf("无授权码的请求不应改变状态，得到 %v", st["state"])
	}
}

// TestLoginHostOverridable 登录主机可覆盖（企业/私有部署场景）。
func TestLoginHostOverridable(t *testing.T) {
	s, err := StartLogin(Products[0], LoginOptions{
		LoginHost:   "https://custom.example.com/",
		APIHost:     "http://127.0.0.1:1",
		OpenBrowser: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.HasPrefix(s.URL, "https://custom.example.com/authorization?") {
		t.Errorf("未使用自定义主机，或尾斜杠未处理: %s", s.URL)
	}
}

// TestOpenBrowserDisabled 关闭自动打开时不应尝试启动浏览器。
func TestOpenBrowserDisabled(t *testing.T) {
	// 只要能起来就说明没在 StartLogin 里同步打开浏览器（否则测试环境会挂）
	s := startTestLogin(t, Products[0], "http://127.0.0.1:1")
	if s.URL == "" {
		t.Error("URL 为空")
	}
}

// TestLoginLiveURLShape 打印真实 URL 供人工核对（不做网络请求）。
func TestLoginLiveURLShape(t *testing.T) {
	if os.Getenv("TRAE_LIVE_LOGIN") != "1" {
		t.Skip("需 TRAE_LIVE_LOGIN=1")
	}
	for _, p := range Products {
		s := startTestLogin(t, p, DefaultAPIHost)
		t.Logf("[%s] %s", p.Key, s.URL)
	}
}

// TestProcessCallbackManualURL 测试云服务器/Docker下手动粘贴浏览器重定向URL完成授权。
func TestProcessCallbackManualURL(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exchangePath {
			t.Errorf("路径 = %q want %q", r.URL.Path, exchangePath)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["AuthCode"] != "test-auth-code-123" {
			t.Errorf("AuthCode = %v want test-auth-code-123", body["AuthCode"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{
				"Token":            "mock-token-xyz",
				"RefreshToken":     "mock-refresh-xyz",
				"ExpiredAt":        "2030-01-01T00:00:00Z",
				"RefreshExpiredAt": "2030-02-01T00:00:00Z",
				"UserID":           "916348159159136",
			},
		})
	}))
	defer mockServer.Close()

	p := Products[0] // trae-cn
	s, err := StartLogin(p, LoginOptions{
		LoginHost:   "https://www.trae.cn",
		APIHost:     mockServer.URL,
		OpenBrowser: false,
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer s.Close()

	if s.LoginTraceID() == "" {
		t.Error("LoginTraceID 应当非空")
	}

	// 模拟远程服务器部署时，浏览器重定向到 127.0.0.1:<port> 的完整 URL
	mockRedirectURL := fmt.Sprintf(
		"http://127.0.0.1:43029/authorize?isRedirect=true&scope=trae&authCodeInfo=%%7B%%22AuthCode%%22%%3A%%22test-auth-code-123%%22%%2C%%22ExpireAt%%22%%3A1791787473440%%7D&loginTraceID=%s&host=%s&userRegion=cn&userInfo=%%7B%%22UserID%%22%%3A%%22916348159159136%%22%%7D",
		s.LoginTraceID(),
		url.QueryEscape(mockServer.URL),
	)

	snap, err := s.ProcessCallback(mockRedirectURL)
	if err != nil {
		t.Fatalf("ProcessCallback 失败: %v", err)
	}
	if snap == nil {
		t.Fatal("snap 为 nil")
	}
	if snap.Token != "mock-token-xyz" {
		t.Errorf("Token = %q want mock-token-xyz", snap.Token)
	}
	if snap.UserID != "916348159159136" {
		t.Errorf("UserID = %q want 916348159159136", snap.UserID)
	}

	st := s.Status()
	if st["state"] != "success" {
		t.Errorf("会话状态 = %v want success", st["state"])
	}
}
