package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api/internal/config"
	"trae2api/internal/usermgr"
)

// newAuthServer 构造只用于鉴权测试的最小 Server。
func newAuthServer(t *testing.T, apiKey string) (*Server, *usermgr.Manager) {
	t.Helper()
	um := usermgr.New(filepath.Join(t.TempDir(), "users.json"))
	return &Server{cfg: &config.Config{APIKey: apiKey}, users: um}, um
}

// probe 返回一个记录"是否通过鉴权、以及看到的身份"的探针 handler。
func probe() (http.HandlerFunc, *struct {
	called bool
	user   *usermgr.User
	admin  bool
}) {
	rec := &struct {
		called bool
		user   *usermgr.User
		admin  bool
	}{}
	h := func(w http.ResponseWriter, r *http.Request) {
		rec.called = true
		rec.user = userFrom(r)
		rec.admin = isAdmin(r)
		w.WriteHeader(http.StatusOK)
	}
	return h, rec
}

func do(s *Server, h http.HandlerFunc, authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	s.withAuth(h)(w, req)
	return w
}

// TestAuthAnonymousWhenNoUsersNoKey 单机免密模式：既没用户也没全局 Key → 放行且视为管理员。
//
// 这是"零回归"的锚点：老用户升级后行为不能变。
func TestAuthAnonymousWhenNoUsersNoKey(t *testing.T) {
	s, _ := newAuthServer(t, "")
	h, rec := probe()
	w := do(s, h, "")
	if w.Code != http.StatusOK || !rec.called {
		t.Fatalf("单机免密应放行，得到 %d called=%v", w.Code, rec.called)
	}
	if !rec.admin {
		t.Error("单机免密应视为管理员")
	}
}

// TestAuthGlobalKey 配置了全局 Key：必须带对才能过，且视为管理员。
func TestAuthGlobalKey(t *testing.T) {
	s, _ := newAuthServer(t, "sk-global-secret")

	h, rec := probe()
	if w := do(s, h, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无 Key 应 401，得到 %d", w.Code)
	}
	if rec.called {
		t.Error("未鉴权的请求不应进入 handler")
	}

	h2, rec2 := probe()
	if w := do(s, h2, "Bearer wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("错误 Key 应 401，得到 %d", w.Code)
	}
	if rec2.called {
		t.Error("错误 Key 不应进入 handler")
	}

	h3, rec3 := probe()
	if w := do(s, h3, "Bearer sk-global-secret"); w.Code != http.StatusOK {
		t.Errorf("正确 Key 应放行，得到 %d", w.Code)
	}
	if !rec3.admin {
		t.Error("全局 Key 应视为管理员")
	}
}

// TestAuthUsersRequireKey 一旦存在用户，**禁止匿名通过**。
//
// 这是关键安全语义：否则"创建了用户"反而让 api_key 为空时的网关完全敞开。
func TestAuthUsersRequireKey(t *testing.T) {
	s, um := newAuthServer(t, "") // 故意不配全局 Key
	// 首个用户必须是管理员（防自锁规则），故先建 admin
	if _, err := um.Create("root", usermgr.RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	h, rec := probe()
	if w := do(s, h, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("有用户但未配全局 Key 时，匿名应 401，得到 %d", w.Code)
	}
	if rec.called {
		t.Error("匿名请求不应进入 handler")
	}
}

// TestAuthUserKeyRole 用户专属 Key 生效，且角色正确传递。
func TestAuthUserKeyRole(t *testing.T) {
	s, um := newAuthServer(t, "")
	admin, _ := um.Create("root", usermgr.RoleAdmin, "") // 首个用户必须是管理员
	user, _ := um.Create("alice", usermgr.RoleUser, "")

	h, rec := probe()
	if w := do(s, h, "Bearer "+user.APIKey); w.Code != http.StatusOK {
		t.Fatalf("普通用户 Key 应放行，得到 %d", w.Code)
	}
	if rec.user == nil || rec.user.Username != "alice" {
		t.Errorf("身份未注入: %+v", rec.user)
	}
	if rec.admin {
		t.Error("普通用户不应是管理员")
	}

	h2, rec2 := probe()
	if w := do(s, h2, "Bearer "+admin.APIKey); w.Code != http.StatusOK {
		t.Fatalf("管理员 Key 应放行，得到 %d", w.Code)
	}
	if !rec2.admin {
		t.Error("管理员应被识别为 admin")
	}
}

// TestAuthDisabledUserRejected 已禁用用户的 Key 立即失效。
func TestAuthDisabledUserRejected(t *testing.T) {
	s, um := newAuthServer(t, "")
	admin, _ := um.Create("root", usermgr.RoleAdmin, "") // 首个用户必须是管理员
	user, _ := um.Create("alice", usermgr.RoleUser, "")
	um.SetDisabled(user.ID, true)
	_ = admin

	h, rec := probe()
	if w := do(s, h, "Bearer "+user.APIKey); w.Code != http.StatusUnauthorized {
		t.Errorf("已禁用用户的 Key 应 401，得到 %d", w.Code)
	}
	if rec.called {
		t.Error("已禁用用户不应进入 handler")
	}
}

// TestAuthGlobalKeyStillWorksWithUsers 有用户时全局 Key 仍可用（管理员入口）。
func TestAuthGlobalKeyStillWorksWithUsers(t *testing.T) {
	s, um := newAuthServer(t, "sk-global")
	um.Create("root", usermgr.RoleAdmin, "") // 首个用户必须是管理员
	um.Create("alice", usermgr.RoleUser, "")

	h, rec := probe()
	if w := do(s, h, "Bearer sk-global"); w.Code != http.StatusOK {
		t.Fatalf("全局 Key 应放行，得到 %d", w.Code)
	}
	if !rec.admin {
		t.Error("全局 Key 应为管理员")
	}
}

// TestRequireAdmin 非管理员访问管理端点应 403（而不是 401/500）。
func TestRequireAdmin(t *testing.T) {
	s, um := newAuthServer(t, "")
	admin, _ := um.Create("root", usermgr.RoleAdmin, "") // 首个用户必须是管理员
	user, _ := um.Create("alice", usermgr.RoleUser, "")

	guarded := s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	if w := do(s, guarded, "Bearer "+user.APIKey); w.Code != http.StatusForbidden {
		t.Errorf("普通用户访问管理端点应 403，得到 %d", w.Code)
	}
	if w := do(s, guarded, "Bearer "+admin.APIKey); w.Code != http.StatusOK {
		t.Errorf("管理员应放行，得到 %d", w.Code)
	}
	// 免密模式（无用户无 Key）下也应视为管理员
	s2, _ := newAuthServer(t, "")
	guarded2 := s2.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if w := do(s2, guarded2, ""); w.Code != http.StatusOK {
		t.Errorf("免密模式应视为管理员，得到 %d", w.Code)
	}
}

// TestOwnerOf 归属集合：管理员为 nil（不限），普通用户为自己的 ID 与用户名。
func TestOwnerOf(t *testing.T) {
	s, um := newAuthServer(t, "")
	admin, _ := um.Create("root", usermgr.RoleAdmin, "") // 首个用户必须是管理员
	user, _ := um.Create("alice", usermgr.RoleUser, "")

	reqUser := httptest.NewRequest(http.MethodGet, "/x", nil).
		WithContext(context.WithValue(context.Background(), userCtxKey, user))
	owners := s.ownerOf(reqUser)
	if len(owners) != 2 || owners[0] != user.ID || owners[1] != "alice" {
		t.Errorf("普通用户归属 = %v，应为 [ID, username]", owners)
	}

	reqAdmin := httptest.NewRequest(http.MethodGet, "/x", nil).
		WithContext(context.WithValue(context.Background(), userCtxKey, admin))
	if got := s.ownerOf(reqAdmin); got != nil {
		t.Errorf("管理员归属应为 nil（不限），得到 %v", got)
	}

	// 匿名（免密模式）也应不限
	reqAnon := httptest.NewRequest(http.MethodGet, "/x", nil)
	if got := s.ownerOf(reqAnon); got != nil {
		t.Errorf("匿名归属应为 nil，得到 %v", got)
	}
}

// ============================================================================
// 用户密码认证（internal/server/auth.go + usermgr 的密码部分）
//
// 这一节的测试**全部走真实路由表**（s.handler()）而不是直接调 handler：
// 契约给的是 URL，若路由没注册，直接调 handler 照样能过 —— 那测不出真问题。
// ============================================================================

// newAuthFlowServer 构造用于认证流程测试的 Server。
//
// 与上面的 newAuthServer 的区别：这里带 config.Default() + 临时数据目录 +
// 登录限流器（限流本身是认证语义的一部分，必须一起测）。
func newAuthFlowServer(t *testing.T) (*Server, *usermgr.Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	um := usermgr.New(filepath.Join(dir, "users.json"))
	return &Server{cfg: cfg, users: um, loginLimit: newLoginLimiter()}, um
}

// seedAdmin 预置一个管理员并返回它的 API Key（首个用户必须是管理员）。
func seedAdmin(t *testing.T, um *usermgr.Manager) string {
	t.Helper()
	u, err := um.Create("root", usermgr.RoleAdmin, "测试预置")
	if err != nil {
		t.Fatalf("预置管理员失败: %v", err)
	}
	return u.APIKey
}

// call 通过真实路由表发一个请求。body 为空表示不带请求体。
func call(t *testing.T, s *Server, method, path, body, apiKey string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	return w
}

// jsonBody 解析响应体。
func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, w.Body.String())
	}
	return m
}

// errCode 取出 OpenAI 风格错误信封里的 code。
func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	env, ok := jsonBody(t, w)["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 error 信封（body=%q）", w.Body.String())
	}
	code, _ := env["code"].(string)
	return code
}

// sessionCookieOf 从响应里取登录态 cookie。
func sessionCookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("响应里没有 %s cookie（Set-Cookie=%v）", sessionCookie, w.Header().Values("Set-Cookie"))
	return nil
}

// enableRegister 通过管理员端点打开自助注册（顺带把这个端点也测掉）。
func enableRegister(t *testing.T, s *Server, adminKey string) {
	t.Helper()
	w := call(t, s, http.MethodPost, "/admin/system/allow_register", `{"enabled":true}`, adminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("开启注册失败: %d %s", w.Code, w.Body.String())
	}
}

// TestPBKDF2SHA256Vectors 手写 PBKDF2 必须与公开测试向量**逐字节**一致。
//
// 这是本节最重要的一条：PBKDF2 是"自己实现的安全原语"，写错了不会报错 ——
// 只会让所有哈希变成另一种值（甚至把密码强度削掉而不自知）。
// 向量来自 RFC 7914 §11 与 PBKDF2-HMAC-SHA256 的公开测试集。
func TestPBKDF2SHA256Vectors(t *testing.T) {
	cases := []struct {
		name           string
		password, salt string
		iter, keyLen   int
		want           string
	}{
		{"RFC7914 P=password S=salt c=1", "password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"RFC7914 c=2", "password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"RFC7914 c=4096", "password", "salt", 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"RFC7914 多块 dkLen=40", "passwordPASSWORDpassword",
			"saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
		{"RFC7914 口令含 NUL 字节", "pass\x00word", "sa\x00lt", 4096, 16,
			"89b69d0516f829893c696226650a8687"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(usermgr.PBKDF2SHA256(
			[]byte(c.password), []byte(c.salt), c.iter, c.keyLen))
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// TestPasswordIterationsMeetsContract 迭代次数必须 >= 契约下限 100000。
func TestPasswordIterationsMeetsContract(t *testing.T) {
	if usermgr.PasswordIterations < 100000 {
		t.Fatalf("PBKDF2 迭代次数 %d 低于契约下限 100000", usermgr.PasswordIterations)
	}
	if usermgr.MinPasswordLen < 6 {
		t.Errorf("最短密码长度 %d 过短", usermgr.MinPasswordLen)
	}
}

// TestVerifyPasswordRejectsIncompleteRecord 残缺记录不能被当成"万能钥匙"。
//
// 这条针对的是一种很危险的实现方式：把"没有密码"当作"密码为空即可通过"。
// 那样一条空的密码记录就成了任何人都能登录的后门。
func TestVerifyPasswordRejectsIncompleteRecord(t *testing.T) {
	// 已知向量（P=password S=salt c=1）验证 VerifyPassword 的接线正确
	salt := hex.EncodeToString([]byte("salt"))
	hash := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if !usermgr.VerifyPassword("password", hash, salt, 1) {
		t.Error("正确密码应通过（VerifyPassword 与 PBKDF2 的接线可能有误）")
	}
	if usermgr.VerifyPassword("Password", hash, salt, 1) {
		t.Error("错误密码不应通过")
	}
	// 残缺记录一律 false
	for _, c := range []struct{ name, hash, salt string }{
		{"没有密码（老用户）", "", ""},
		{"只有哈希没有盐", hash, ""},
		{"只有盐没有哈希", "", salt},
		{"哈希不是 hex", "zzzz", salt},
		{"盐不是 hex", hash, "zzzz"},
	} {
		if usermgr.VerifyPassword("password", c.hash, c.salt, 1) {
			t.Errorf("%s：不应通过校验", c.name)
		}
	}
}

// TestSetPasswordThenCheckPassword usermgr 层的设置/校验（顺带证明不自死锁）。
//
// ⚠️ 若 SetPassword 里误用 Save()（再拿一次读锁）会**自死锁**，
// 这条测试会直接挂住（go test 默认 10 分钟超时后报 panic: test timed out）。
func TestSetPasswordThenCheckPassword(t *testing.T) {
	um := usermgr.New(filepath.Join(t.TempDir(), "users.json"))
	seedAdmin(t, um)
	u, err := um.Create("alice", usermgr.RoleUser, "")
	if err != nil {
		t.Fatal(err)
	}
	// 创建时没设密码 → 不能密码登录
	if um.CheckPassword(u.ID, "") || um.CheckPassword(u.ID, "whatever") {
		t.Error("未设密码的用户不应通过任何密码校验")
	}
	if err := um.SetPassword(u.ID, "s3cret-pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if !um.CheckPassword(u.ID, "s3cret-pw") {
		t.Error("设置后正确密码应通过")
	}
	if um.CheckPassword(u.ID, "s3cret-pW") {
		t.Error("大小写不同不应通过")
	}
	if um.CheckPassword("ghost", "s3cret-pw") {
		t.Error("不存在的用户不应通过")
	}
	// 太短的密码必须被拒（否则契约里的"至少 6 位"形同虚设）
	if err := um.SetPassword(u.ID, "12345"); err == nil {
		t.Error("5 位密码应被拒绝")
	}
	if err := um.SetPassword(u.ID, "      "); err == nil {
		t.Error("全空白密码应被拒绝")
	}
	// 中文密码按**字符数**算长度（"密码"两个字 = 6 字节，不能因此算够长）
	if err := um.SetPassword(u.ID, "密码"); err == nil {
		t.Error("2 个中文字符（6 字节）不该被当作够长")
	}
	if err := um.SetPassword(u.ID, "中文密码六位"); err != nil {
		t.Errorf("6 个中文字符应合法: %v", err)
	}
}

// TestPasswordStoredAsPBKDF2NotPlaintext 落盘必须是"每用户独立盐 + PBKDF2"，
// 而不是明文、也不是单轮 sha256。
func TestPasswordStoredAsPBKDF2NotPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	um := usermgr.New(path)
	seedAdmin(t, um)
	const pw = "correct-horse-battery"
	u, err := um.CreateWithPassword("alice", usermgr.RoleUser, pw, "测试")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pw) {
		t.Fatal("users.json 里出现了明文密码")
	}
	// 单轮 sha256 的指纹：hash == sha256(password + ":" + salt)
	single := sha256.Sum256([]byte(pw + ":" + u.Salt))
	if u.PasswordHash == hex.EncodeToString(single[:]) {
		t.Fatal("用的是单轮 sha256（契约禁止）")
	}
	if len(u.PasswordHash) != 64 {
		t.Errorf("派生密钥应为 32 字节 hex（64 字符），得到 %d 字符", len(u.PasswordHash))
	}
	if len(u.Salt) != 32 {
		t.Errorf("盐应为 16 字节 hex（32 字符），得到 %d 字符", len(u.Salt))
	}
	if u.PasswordIter < 100000 {
		t.Errorf("落盘的迭代次数 %d 低于契约下限", u.PasswordIter)
	}
	// 两个用同一密码的用户必须得到不同的哈希（每用户独立盐）
	u2, err := um.CreateWithPassword("bob", usermgr.RoleUser, pw, "测试")
	if err != nil {
		t.Fatal(err)
	}
	if u2.Salt == u.Salt || u2.PasswordHash == u.PasswordHash {
		t.Error("同一密码的两个用户必须有不同的盐与哈希")
	}
	// 重启后密码仍可校验（哈希/盐/迭代次数都要落盘）
	m2 := usermgr.New(path)
	if err := m2.Load(); err != nil {
		t.Fatal(err)
	}
	if !m2.CheckPassword(u.ID, pw) {
		t.Error("重启加载后密码应仍可校验")
	}
}

// TestContractC10AuthFlow 把契约 B 节的端点**串成一条真实流程**跑一遍。
//
// 覆盖：GET /admin/system、POST /admin/system/allow_register、
// POST /auth/register、/auth/login、/auth/logout、/me/password、
// /me/api-key/reset（全部按契约的状态码与字段断言）。
func TestContractC10AuthFlow(t *testing.T) {
	s, um := newAuthFlowServer(t)
	adminKey := seedAdmin(t, um)

	// 1) GET /admin/system —— 默认关闭注册，且透出 version / service
	w := call(t, s, http.MethodGet, "/admin/system", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/system = %d，want 200", w.Code)
	}
	sys := jsonBody(t, w)
	if sys["allow_register"] != false {
		t.Errorf("allow_register 默认应为 false，得到 %v", sys["allow_register"])
	}
	if sys["version"] != Version || sys["service"] != ServiceName {
		t.Errorf("version/service 不符: %v / %v", sys["version"], sys["service"])
	}

	// 2) 关闭状态下注册必须 403 register_disabled
	w = call(t, s, http.MethodPost, "/auth/register", `{"username":"alice","password":"s3cret-pw"}`, "")
	if w.Code != http.StatusForbidden || errCode(t, w) != "register_disabled" {
		t.Fatalf("关闭注册时应 403 register_disabled，得到 %d %s", w.Code, w.Body.String())
	}

	// 3) 管理员开启注册
	w = call(t, s, http.MethodPost, "/admin/system/allow_register", `{"enabled":true}`, adminKey)
	if w.Code != http.StatusOK || jsonBody(t, w)["allow_register"] != true {
		t.Fatalf("开启注册失败: %d %s", w.Code, w.Body.String())
	}
	if got := jsonBody(t, call(t, s, http.MethodGet, "/admin/system", "", ""))["allow_register"]; got != true {
		t.Errorf("开启后 GET /admin/system 应报 true，得到 %v", got)
	}

	// 4) POST /auth/register
	w = call(t, s, http.MethodPost, "/auth/register", `{"username":"alice","password":"s3cret-pw"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", w.Code, w.Body.String())
	}
	reg := jsonBody(t, w)
	for _, k := range []string{"id", "username", "role", "api_key"} {
		if _, ok := reg[k]; !ok {
			t.Errorf("注册响应缺字段 %q: %s", k, w.Body.String())
		}
	}
	if reg["username"] != "alice" || reg["role"] != "user" {
		t.Errorf("注册响应不符: %v", reg)
	}
	if key, _ := reg["api_key"].(string); !strings.HasPrefix(key, "sk-trae-") {
		t.Errorf("api_key 格式不符: %v", reg["api_key"])
	}
	uid, _ := reg["id"].(string)

	// 5) POST /auth/login —— 必须下发 trae2api_session cookie
	w = call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"s3cret-pw"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	login := jsonBody(t, w)
	if login["username"] != "alice" || login["role"] != "user" || login["id"] != uid {
		t.Errorf("登录响应不符: %v", login)
	}
	sess := sessionCookieOf(t, w)
	if !sess.HttpOnly {
		t.Error("会话 cookie 必须是 HttpOnly")
	}
	if sess.Path != "/" {
		t.Errorf("会话 cookie Path 应为 /，得到 %q", sess.Path)
	}
	if sess.MaxAge <= 0 {
		t.Errorf("会话 cookie 应有过期时间，得到 MaxAge=%d", sess.MaxAge)
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Errorf("会话 cookie 应为 SameSite=Lax，得到 %v", sess.SameSite)
	}
	if sess.Name == webAuthCookie {
		t.Fatalf("会话 cookie 覆盖了控制台门禁 cookie（%s）", webAuthCookie)
	}

	// 6) POST /me/api-key/reset —— 换 Key，旧 Key 立即失效
	oldKey, _ := login["api_key"].(string)
	w = call(t, s, http.MethodPost, "/me/api-key/reset", "", "", sess)
	if w.Code != http.StatusOK {
		t.Fatalf("自助换 Key 失败: %d %s", w.Code, w.Body.String())
	}
	rot := jsonBody(t, w)
	newKey, _ := rot["api_key"].(string)
	if newKey == "" || newKey == oldKey {
		t.Errorf("应返回新的 api_key，得到 %v（旧 %v）", rot["api_key"], oldKey)
	}
	if _, ok := rot["warning"]; !ok {
		t.Error("换 Key 响应应带 warning（提示旧 Key 已失效）")
	}
	if _, ok := um.FindByAPIKey(oldKey); ok {
		t.Error("旧 API Key 应已立即失效")
	}
	if _, ok := um.FindByAPIKey(newKey); !ok {
		t.Error("新 API Key 应立即可用")
	}

	// 7) POST /me/password —— 自助改密
	w = call(t, s, http.MethodPost, "/me/password",
		`{"old_password":"s3cret-pw","new_password":"new-pw-12345"}`, "", sess)
	if w.Code != http.StatusOK || jsonBody(t, w)["ok"] != true {
		t.Fatalf("自助改密失败: %d %s", w.Code, w.Body.String())
	}

	// 8) 旧密码立即失效
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("改密后旧密码应 401，得到 %d", w.Code)
	}
	// 9) 新密码可用
	w = call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"new-pw-12345"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("改密后新密码应可登录: %d %s", w.Code, w.Body.String())
	}
	sess2 := sessionCookieOf(t, w)

	// 10) POST /auth/logout
	w = call(t, s, http.MethodPost, "/auth/logout", "", "", sess2)
	if w.Code != http.StatusOK || jsonBody(t, w)["ok"] != true {
		t.Fatalf("登出失败: %d %s", w.Code, w.Body.String())
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("登出应下发一个过期的会话 cookie")
	}
	// 登出后该 cookie 不再可用
	if w := call(t, s, http.MethodPost, "/me/api-key/reset", "", "", sess2); w.Code != http.StatusUnauthorized {
		t.Errorf("登出后 /me/api-key/reset 应 401，得到 %d", w.Code)
	}

	// 11) 关掉注册后必须立刻 403
	if w := call(t, s, http.MethodPost, "/admin/system/allow_register",
		`{"enabled":false}`, adminKey); w.Code != http.StatusOK || jsonBody(t, w)["allow_register"] != false {
		t.Fatalf("关闭注册失败: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, http.MethodPost, "/auth/register",
		`{"username":"bob","password":"s3cret-pw"}`, ""); w.Code != http.StatusForbidden {
		t.Errorf("关闭注册后应 403，得到 %d", w.Code)
	}
}

// TestRegisterErrors 注册的错误路径：409 重名 / 400 参数不合法。
func TestRegisterErrors(t *testing.T) {
	s, um := newAuthFlowServer(t)
	adminKey := seedAdmin(t, um)
	enableRegister(t, s, adminKey)

	if w := call(t, s, http.MethodPost, "/auth/register",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("首次注册应成功: %d %s", w.Code, w.Body.String())
	}
	// 重名 → 409
	w := call(t, s, http.MethodPost, "/auth/register", `{"username":"alice","password":"other-pw"}`, "")
	if w.Code != http.StatusConflict {
		t.Errorf("重名注册应 409，得到 %d %s", w.Code, w.Body.String())
	}
	if code := errCode(t, w); code != "username_exists" {
		t.Errorf("重名错误 code = %q，want username_exists", code)
	}
	// 大小写不同也是同一个用户名（与 usermgr 的去重规则一致）
	if w := call(t, s, http.MethodPost, "/auth/register",
		`{"username":"ALICE","password":"other-pw"}`, ""); w.Code != http.StatusConflict {
		t.Errorf("大小写不同的重名应 409，得到 %d", w.Code)
	}
	// 密码太短 → 400
	if w := call(t, s, http.MethodPost, "/auth/register",
		`{"username":"bob","password":"12345"}`, ""); w.Code != http.StatusBadRequest {
		t.Errorf("密码过短应 400，得到 %d", w.Code)
	}
	// 用户名过短 / 含非法字符 → 400
	for _, name := range []string{"ab", "has space", "a:b", strings.Repeat("x", 33)} {
		body := `{"username":"` + name + `","password":"s3cret-pw"}`
		if w := call(t, s, http.MethodPost, "/auth/register", body, ""); w.Code != http.StatusBadRequest {
			t.Errorf("用户名 %q 应 400，得到 %d", name, w.Code)
		}
	}
	// 非法注册**不能**留下用户
	if um.Count() != 2 { // root + alice
		t.Errorf("用户数 = %d，want 2（失败的注册不该建出用户）", um.Count())
	}
}

// TestRegisterFirstUserMustBeAdmin 自助注册不能绕过"首个用户必须是管理员"。
//
// 若给这条规则开例外，等于把管理权交给**第一个访问者** —— 开公网时尤其危险。
func TestRegisterFirstUserMustBeAdmin(t *testing.T) {
	s, um := newAuthFlowServer(t)
	// 一个用户都没有，但注册开关被打开了（运维在空库上直接开了注册）
	if err := um.SetAllowRegister(true); err != nil {
		t.Fatal(err)
	}
	w := call(t, s, http.MethodPost, "/auth/register", `{"username":"alice","password":"s3cret-pw"}`, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空库上的首个自助注册应被拒（400），得到 %d %s", w.Code, w.Body.String())
	}
	if code := errCode(t, w); code != "first_user_must_be_admin" {
		t.Errorf("错误 code = %q，want first_user_must_be_admin", code)
	}
	if um.Count() != 0 {
		t.Errorf("被拒的注册不该建出用户，Count = %d", um.Count())
	}
	// 管理员先建好首个管理员后，自助注册才能正常工作
	adminKey := seedAdmin(t, um)
	enableRegister(t, s, adminKey)
	if w := call(t, s, http.MethodPost, "/auth/register",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusOK {
		t.Errorf("有管理员后自助注册应成功: %d %s", w.Code, w.Body.String())
	}
}

// TestLoginFailureIsIndistinguishable 登录失败必须**不区分**用户名不存在与密码错。
//
// 断言到字节级：状态码与响应体必须完全一致。区分它们等于送出一个
// 用户名枚举接口（先批量筛出真实用户名，再集中爆破）。
func TestLoginFailureIsIndistinguishable(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	if _, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", ""); err != nil {
		t.Fatal(err)
	}

	// 用户不存在
	wGhost := call(t, s, http.MethodPost, "/auth/login", `{"username":"ghost","password":"s3cret-pw"}`, "")
	// 用户存在但密码错
	wWrong := call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"wrong-pw"}`, "")
	// 用户存在但没设密码（老用户）
	if _, err := um.Create("legacy", usermgr.RoleUser, ""); err != nil {
		t.Fatal(err)
	}
	wNoPw := call(t, s, http.MethodPost, "/auth/login", `{"username":"legacy","password":"s3cret-pw"}`, "")

	if wGhost.Code != http.StatusUnauthorized || wWrong.Code != http.StatusUnauthorized || wNoPw.Code != http.StatusUnauthorized {
		t.Fatalf("三种失败都应是 401，得到 %d / %d / %d", wGhost.Code, wWrong.Code, wNoPw.Code)
	}
	if wGhost.Body.String() != wWrong.Body.String() || wWrong.Body.String() != wNoPw.Body.String() {
		t.Errorf("登录失败响应必须逐字节一致：\n ghost = %s\n wrong = %s\n noPw  = %s",
			wGhost.Body.String(), wWrong.Body.String(), wNoPw.Body.String())
	}
}

// TestLoginLegacyUserWithoutPasswordRejected 老用户平滑过渡。
//
// 升级前创建的账号没有密码字段：**不允许**密码登录（不搞"默认密码"，也不强制迁移），
// 但 API Key 必须照旧可用 —— 否则升级等于把所有存量客户端踢下线。
func TestLoginLegacyUserWithoutPasswordRejected(t *testing.T) {
	s, um := newAuthFlowServer(t)
	adminKey := seedAdmin(t, um)
	legacy, err := um.Create("legacy", usermgr.RoleUser, "升级前创建")
	if err != nil {
		t.Fatal(err)
	}
	// 空密码 / 常见弱口令 / 用户名本身，都不该放进来
	//
	// 只试 4 次：第 5 次会**合法地**触发按 IP 锁定（maxFail=5），
	// 那会把"密码不对"的断言变成"被限流"，测不到本来想测的东西
	//（限流本身由 TestLoginRateLimited 覆盖）。
	for _, pw := range []string{"", "admin", "password", "legacy"} {
		body := `{"username":"legacy","password":"` + pw + `"}`
		if w := call(t, s, http.MethodPost, "/auth/login", body, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("没有密码的老用户不该能用 %q 登录，得到 %d", pw, w.Code)
		}
	}
	// API Key 照旧可用（平滑过渡的核心）
	if w := call(t, s, http.MethodGet, "/v1/models", "", legacy.APIKey); w.Code != http.StatusOK {
		t.Errorf("老用户的 API Key 应照旧可用，得到 %d %s", w.Code, w.Body.String())
	}
	// 管理员给他设一个密码后，密码登录即可用（这是过渡路径）
	w := call(t, s, http.MethodPost, "/admin/users/"+legacy.ID+"/reset-password",
		`{"password":"brand-new-pw"}`, adminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("管理员设密码失败: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"legacy","password":"brand-new-pw"}`, ""); w.Code != http.StatusOK {
		t.Errorf("设了密码后应能密码登录，得到 %d %s", w.Code, w.Body.String())
	}
}

// TestLoginDisabledUserRejected 被禁用的用户不能登录，且登录态立刻失效。
func TestLoginDisabledUserRejected(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	u, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", "")
	if err != nil {
		t.Fatal(err)
	}
	// 先正常登录拿一个会话
	w := call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"s3cret-pw"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	sess := sessionCookieOf(t, w)
	if err := um.SetDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	// 禁用后不能再登录
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("被禁用的用户不该能登录，得到 %d", w.Code)
	}
	// 已经拿到的会话也要立刻失效（"禁用账号"必须马上生效）
	if w := call(t, s, http.MethodPost, "/me/api-key/reset", "", "", sess); w.Code != http.StatusUnauthorized {
		t.Errorf("禁用后旧会话应失效，得到 %d", w.Code)
	}
}

// TestLoginRateLimited 登录失败必须计入按 IP 的限流（防爆破）。
func TestLoginRateLimited(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	if _, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", ""); err != nil {
		t.Fatal(err)
	}
	body := `{"username":"alice","password":"wrong-pw"}`
	// maxFail = 5（见 security.go）：第 1–4 次 401，第 5 次触发锁定 → 429
	for i := 1; i <= 4; i++ {
		if w := call(t, s, http.MethodPost, "/auth/login", body, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应为 401，得到 %d", i, w.Code)
		}
	}
	if w := call(t, s, http.MethodPost, "/auth/login", body, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 5 次失败应触发限流（429），得到 %d %s", w.Code, w.Body.String())
	}
	// 锁定后**即使密码正确**也要被挡住（否则限流形同虚设）
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("锁定期内即使密码正确也应 429，得到 %d", w.Code)
	}
}

// TestMePasswordInvalidatesAllSessions 改密必须作废该用户的**全部**会话。
//
// 改密最常见的动机就是"怀疑会话被别人拿到了"；此时留着旧会话等于没改。
func TestMePasswordInvalidatesAllSessions(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	if _, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", ""); err != nil {
		t.Fatal(err)
	}
	login := func() *http.Cookie {
		w := call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"s3cret-pw"}`, "")
		if w.Code != http.StatusOK {
			t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
		}
		return sessionCookieOf(t, w)
	}
	sessA, sessB := login(), login()

	w := call(t, s, http.MethodPost, "/me/password",
		`{"old_password":"s3cret-pw","new_password":"another-pw-1"}`, "", sessA)
	if w.Code != http.StatusOK {
		t.Fatalf("改密失败: %d %s", w.Code, w.Body.String())
	}
	for name, c := range map[string]*http.Cookie{"当前会话": sessA, "另一个会话": sessB} {
		if w := call(t, s, http.MethodPost, "/me/api-key/reset", "", "", c); w.Code != http.StatusUnauthorized {
			t.Errorf("改密后 %s 应失效（401），得到 %d", name, w.Code)
		}
	}
	// 新密码可登录、旧密码不可
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"another-pw-1"}`, ""); w.Code != http.StatusOK {
		t.Errorf("新密码应可登录，得到 %d", w.Code)
	}
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("旧密码应失效，得到 %d", w.Code)
	}
}

// TestMePasswordWrongOldPassword 原密码错必须 401，且不能改动密码。
func TestMePasswordWrongOldPassword(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	if _, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", ""); err != nil {
		t.Fatal(err)
	}
	w := call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"s3cret-pw"}`, "")
	sess := sessionCookieOf(t, w)

	w = call(t, s, http.MethodPost, "/me/password",
		`{"old_password":"wrong","new_password":"another-pw-1"}`, "", sess)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("原密码错应 401，得到 %d %s", w.Code, w.Body.String())
	}
	// 密码不该被改动（旧密码仍可登录）
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusOK {
		t.Errorf("失败的改密不该改动密码，得到 %d", w.Code)
	}
	// 新密码必须是合法的
	w = call(t, s, http.MethodPost, "/me/password",
		`{"old_password":"s3cret-pw","new_password":"123"}`, "", sess)
	if w.Code != http.StatusBadRequest {
		t.Errorf("过短的新密码应 400，得到 %d", w.Code)
	}
}

// TestMeRequiresLogin /me/* 没有登录态必须 401（不能匿名操作）。
func TestMeRequiresLogin(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	for _, path := range []string{"/me/password", "/me/api-key/reset"} {
		if w := call(t, s, http.MethodPost, path, `{"old_password":"a","new_password":"bbbbbb"}`, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s 无登录态应 401，得到 %d", path, w.Code)
		}
		// 伪造一个 cookie 值也不行
		fake := &http.Cookie{Name: sessionCookie, Value: "deadbeef"}
		if w := call(t, s, http.MethodPost, path, `{"old_password":"a","new_password":"bbbbbb"}`, "", fake); w.Code != http.StatusUnauthorized {
			t.Errorf("%s 伪造 cookie 应 401，得到 %d", path, w.Code)
		}
	}
}

// TestSessionInvalidatedWhenUserDeleted 用户被删除后，他的会话必须立即作废。
func TestSessionInvalidatedWhenUserDeleted(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	u, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", "")
	if err != nil {
		t.Fatal(err)
	}
	w := call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"s3cret-pw"}`, "")
	sess := sessionCookieOf(t, w)
	if err := um.Delete(u.ID); err != nil {
		t.Fatal(err)
	}
	if w := call(t, s, http.MethodPost, "/me/api-key/reset", "", "", sess); w.Code != http.StatusUnauthorized {
		t.Errorf("用户被删除后旧会话应失效，得到 %d", w.Code)
	}
}

// TestAdminResetPassword 管理员代改密码：权限、状态码、字段与后续登录。
func TestAdminResetPassword(t *testing.T) {
	s, um := newAuthFlowServer(t)
	adminKey := seedAdmin(t, um)
	u, err := um.Create("alice", usermgr.RoleUser, "")
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/users/" + u.ID + "/reset-password"

	// 非管理员必须 403
	if w := call(t, s, http.MethodPost, path, `{"password":"s3cret-pw"}`, u.APIKey); w.Code != http.StatusForbidden {
		t.Errorf("普通用户调用应 403，得到 %d", w.Code)
	}
	// 无凭证必须 401
	if w := call(t, s, http.MethodPost, path, `{"password":"s3cret-pw"}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证应 401，得到 %d", w.Code)
	}
	// 管理员：200 {"id","username"}
	w := call(t, s, http.MethodPost, path, `{"password":"s3cret-pw"}`, adminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("管理员设密码失败: %d %s", w.Code, w.Body.String())
	}
	got := jsonBody(t, w)
	if got["id"] != u.ID || got["username"] != "alice" {
		t.Errorf("响应字段不符: %v", got)
	}
	if _, leaked := got["password"]; leaked {
		t.Error("响应里不该回显密码")
	}
	if _, leaked := got["api_key"]; leaked {
		t.Error("代改密码不该顺带换 Key")
	}
	// 设完之后该用户就能密码登录了
	if w := call(t, s, http.MethodPost, "/auth/login",
		`{"username":"alice","password":"s3cret-pw"}`, ""); w.Code != http.StatusOK {
		t.Errorf("设密码后应能登录，得到 %d %s", w.Code, w.Body.String())
	}
	// 不存在的用户 → 404
	if w := call(t, s, http.MethodPost, "/admin/users/ghost/reset-password",
		`{"password":"s3cret-pw"}`, adminKey); w.Code != http.StatusNotFound {
		t.Errorf("不存在的用户应 404，得到 %d", w.Code)
	}
	// 太短的密码 → 400
	if w := call(t, s, http.MethodPost, path, `{"password":"123"}`, adminKey); w.Code != http.StatusBadRequest {
		t.Errorf("过短密码应 400，得到 %d", w.Code)
	}
}

// TestAllowRegisterTogglePermissions 开放注册开关的权限与参数校验。
func TestAllowRegisterTogglePermissions(t *testing.T) {
	s, um := newAuthFlowServer(t)
	adminKey := seedAdmin(t, um)
	u, err := um.Create("alice", usermgr.RoleUser, "")
	if err != nil {
		t.Fatal(err)
	}
	// 普通用户不能开注册
	if w := call(t, s, http.MethodPost, "/admin/system/allow_register", `{"enabled":true}`, u.APIKey); w.Code != http.StatusForbidden {
		t.Errorf("普通用户应 403，得到 %d", w.Code)
	}
	if um.AllowRegister() {
		t.Error("被拒的请求不该改动开关")
	}
	// 无凭证
	if w := call(t, s, http.MethodPost, "/admin/system/allow_register", `{"enabled":true}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证应 401，得到 %d", w.Code)
	}
	// 漏传 enabled 必须 400（不能把"拼写错误"静默当成关闭注册）
	if w := call(t, s, http.MethodPost, "/admin/system/allow_register", `{}`, adminKey); w.Code != http.StatusBadRequest {
		t.Errorf("漏传 enabled 应 400，得到 %d", w.Code)
	}
	// 显式 false 也必须能工作（这就是为什么用 *bool）
	if w := call(t, s, http.MethodPost, "/admin/system/allow_register", `{"enabled":false}`, adminKey); w.Code != http.StatusOK {
		t.Errorf("显式 false 应 200，得到 %d %s", w.Code, w.Body.String())
	}
	if um.AllowRegister() {
		t.Error("显式 false 应关闭注册")
	}
}

// TestAllowRegisterPersistsAcrossReload 开关必须落盘，且**不能改动 users.json 的格式**。
//
// 格式要求是硬性的：users.json 是裸数组（[User,...]），
// 为了塞一个设置项而改格式会让升级前的文件读不出来（等于升级即丢全部用户）。
func TestAllowRegisterPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	m1 := usermgr.New(path)
	if _, err := m1.Create("root", usermgr.RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	if m1.AllowRegister() {
		t.Fatal("默认必须关闭注册（fail-closed）")
	}
	if err := m1.SetAllowRegister(true); err != nil {
		t.Fatalf("SetAllowRegister: %v", err)
	}
	// 设置存在独立文件里
	if _, err := os.Stat(filepath.Join(dir, "system.json")); err != nil {
		t.Errorf("system.json 应已生成: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Errorf("users.json 必须仍是裸数组格式，实际开头是 %q", firstLine(string(raw)))
	}
	// 重新加载后开关仍在
	m2 := usermgr.New(path)
	if err := m2.Load(); err != nil {
		t.Fatal(err)
	}
	if !m2.AllowRegister() {
		t.Error("开关应跨重启保留")
	}
	// 设置文件损坏时必须 fail-closed（按关闭处理），而不是按开启
	if err := os.WriteFile(filepath.Join(dir, "system.json"), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	m3 := usermgr.New(path)
	if err := m3.Load(); err != nil {
		t.Fatalf("system.json 损坏不该让 Load 失败（那会连用户表一起丢）: %v", err)
	}
	if m3.Count() != 1 {
		t.Errorf("用户表应照常加载，Count = %d", m3.Count())
	}
	if m3.AllowRegister() {
		t.Error("设置文件损坏时应按关闭处理（fail-closed）")
	}
}

// TestSessionCookieCoexistsWithConsoleCookie 两个 cookie 必须并存、互不覆盖。
//
// 用户登录（trae2api_session）不该顺手把控制台页面门禁（trae2api_console）也退掉，
// 反之亦然 —— 它们是两件正交的事（见 webauth.go 的说明）。
func TestSessionCookieCoexistsWithConsoleCookie(t *testing.T) {
	s, um := newAuthFlowServer(t)
	seedAdmin(t, um)
	if _, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", ""); err != nil {
		t.Fatal(err)
	}
	// 登录只下发 trae2api_session
	w := call(t, s, http.MethodPost, "/auth/login", `{"username":"alice","password":"s3cret-pw"}`, "")
	sess := sessionCookieOf(t, w)
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookie {
			t.Errorf("用户登录不该动控制台门禁 cookie（%s）", webAuthCookie)
		}
	}
	// 带着控制台 cookie 一起请求也不能互相干扰
	console := &http.Cookie{Name: webAuthCookie, Value: "console-token"}
	if w := call(t, s, http.MethodPost, "/me/api-key/reset", "", "", sess, console); w.Code != http.StatusOK {
		t.Errorf("两个 cookie 并存时 /me/api-key/reset 应成功，得到 %d %s", w.Code, w.Body.String())
	}
	// 登出只清 trae2api_session
	w = call(t, s, http.MethodPost, "/auth/logout", "", "", sess, console)
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookie {
			t.Errorf("用户登出不该清掉控制台门禁 cookie（%s）", webAuthCookie)
		}
	}
}

// TestUserSessionStoreTTL 会话表：签发 / 解析 / 单条注销 / 按用户批量注销 / 过期。
func TestUserSessionStoreTTL(t *testing.T) {
	st := newUserSessionStore()
	tok, err := st.new("u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Errorf("token 应为 32 字节 hex（64 字符），得到 %d", len(tok))
	}
	if id, ok := st.userID(tok); !ok || id != "u1" {
		t.Errorf("解析失败: %q %v", id, ok)
	}
	if _, ok := st.userID("not-a-token"); ok {
		t.Error("伪造 token 不该通过")
	}
	if _, ok := st.userID(""); ok {
		t.Error("空 token 不该通过")
	}
	// 同一用户可以有多个并发会话
	tok2, _ := st.new("u1")
	if _, ok := st.userID(tok2); !ok {
		t.Error("第二个会话应有效")
	}
	// 注销单条不影响另一条
	st.drop(tok)
	if _, ok := st.userID(tok); ok {
		t.Error("已注销的 token 不该有效")
	}
	if _, ok := st.userID(tok2); !ok {
		t.Error("另一条会话不该被误伤")
	}
	// 按用户批量注销
	tok3, _ := st.new("u1")
	tokOther, _ := st.new("u2")
	if n := st.dropUser("u1"); n != 2 {
		t.Errorf("应作废 2 条，得到 %d", n)
	}
	if _, ok := st.userID(tok2); ok {
		t.Error("批量注销后不该有效")
	}
	if _, ok := st.userID(tok3); ok {
		t.Error("批量注销后不该有效")
	}
	if _, ok := st.userID(tokOther); !ok {
		t.Error("别的用户的会话不该被误伤")
	}
	// 过期的会话不该通过
	st.mu.Lock()
	st.sessions["expired"] = userSession{userID: "u2", expires: time.Now().Add(-time.Second)}
	st.mu.Unlock()
	if _, ok := st.userID("expired"); ok {
		t.Error("过期会话不该通过")
	}
	if n := st.count(); n != 1 {
		t.Errorf("清理后应剩 1 条有效会话，得到 %d", n)
	}
}

// TestUserListDoesNotLeakPasswordHash 用户列表**不能**回传密码哈希/盐。
//
// 为什么必须测：哈希一旦泄露，"打穿服务"就退化成"拿到 users.json 离线爆破"，
// 而 users.go 的响应字段是**逐个手写**的白名单 —— 将来有人给 User 加字段、
// 顺手改成 writeJSON(w, u)，就会静默泄露。这条测试是那个护栏。
func TestUserListDoesNotLeakPasswordHash(t *testing.T) {
	s, um := newAuthFlowServer(t)
	adminKey := seedAdmin(t, um)
	u, err := um.CreateWithPassword("alice", usermgr.RoleUser, "s3cret-pw", "")
	if err != nil {
		t.Fatal(err)
	}
	w := call(t, s, http.MethodGet, "/admin/users", "", adminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/users = %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, needle := range []string{"password_hash", "salt", "password_iter", u.PasswordHash, u.Salt, "s3cret-pw"} {
		if needle == "" {
			continue
		}
		if strings.Contains(body, needle) {
			t.Errorf("用户列表泄露了 %q：%s", needle, body)
		}
	}
	// 顺带确认：列表仍然给出打码 Key（有用但不可用的那部分信息）
	if !strings.Contains(body, "api_key_masked") {
		t.Errorf("列表应给出打码 API Key：%s", body)
	}
}

// firstLine 取首行（仅用于报错信息）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
