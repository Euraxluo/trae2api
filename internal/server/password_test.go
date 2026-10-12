package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"trae2api/internal/config"
	"trae2api/internal/usermgr"
)

// newPwServer 构造带密码闸与运行时密码存储的 Server。
//
// 必须带 users 管理器：`withAuth` 在"无用户且无全局 api_key"时会注入匿名管理员，
// 而我们**要经过 withAuth** 才能测到真实的鉴权路径（直接调 handler 会因无身份 403，
// 那测不出业务逻辑，只测出"没鉴权"）。
func newPwServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	s := &Server{
		cfg:           cfg,
		users:         usermgr.New(filepath.Join(dir, "users.json")),
		webAuth:       newWebAuth(""),
		consoleSecret: newConsoleSecretStore(dir),
		loginLimit:    newLoginLimiter(),
	}
	return s
}

func postPw(t *testing.T, s *Server, oldPw, newPw string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"old_password": oldPw, "new_password": newPw})
	req := httptest.NewRequest(http.MethodPost, "/admin/password", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.withAuth(s.handlePasswordChange).ServeHTTP(w, req)
	return w
}

// TestPasswordSetFromPasswordless 单机免密时可直接设置密码。
//
// 必须允许 —— 否则用户**永远无法首次设置**密码（核验原密码时无密码可核）。
func TestPasswordSetFromPasswordless(t *testing.T) {
	s := newPwServer(t)
	if s.webAuth.Enabled() {
		t.Fatal("初始应为免密")
	}
	if w := postPw(t, s, "", "new-pw-123"); w.Code != http.StatusOK {
		t.Fatalf("免密时应允许直接设置密码，得到 %d: %s", w.Code, w.Body.String())
	}
	if !s.webAuth.Enabled() || !s.webAuth.Check("new-pw-123") {
		t.Error("密码未生效")
	}
}

// TestPasswordChangeRequiresOld 已有密码时必须核验原密码。
//
// 为什么必须核验：控制台会话是 HttpOnly Cookie。若不核验，一个**已登录**的
// 会话（用户忘了锁屏）就能改掉密码，把真正的主人锁在外面。
func TestPasswordChangeRequiresOld(t *testing.T) {
	s := newPwServer(t)
	if w := postPw(t, s, "", "old-pw-123"); w.Code != http.StatusOK {
		t.Fatal("首次设置失败")
	}
	// 原密码错 → 拒绝
	if w := postPw(t, s, "wrong", "new-pw-456"); w.Code != http.StatusUnauthorized {
		t.Errorf("原密码错误应 401，得到 %d", w.Code)
	}
	if !s.webAuth.Check("old-pw-123") {
		t.Error("失败的改密不该改动密码")
	}
	// 原密码对 → 通过
	if w := postPw(t, s, "old-pw-123", "new-pw-456"); w.Code != http.StatusOK {
		t.Fatalf("原密码正确应通过，得到 %d: %s", w.Code, w.Body.String())
	}
	if !s.webAuth.Check("new-pw-456") || s.webAuth.Check("old-pw-123") {
		t.Error("密码未更新")
	}
}

// TestPasswordMinLength 新密码至少 6 位；留空表示清除。
func TestPasswordMinLength(t *testing.T) {
	s := newPwServer(t)
	for _, short := range []string{"a", "abc", "12345"} {
		if w := postPw(t, s, "", short); w.Code != http.StatusBadRequest {
			t.Errorf("密码 %q 过短应被拒，得到 %d", short, w.Code)
		}
	}
	if w := postPw(t, s, "", "123456"); w.Code != http.StatusOK {
		t.Errorf("6 位应通过，得到 %d", w.Code)
	}
	// 留空 = 清除（恢复免密）
	if w := postPw(t, s, "123456", ""); w.Code != http.StatusOK {
		t.Fatalf("留空应清除密码，得到 %d: %s", w.Code, w.Body.String())
	}
	if s.webAuth.Enabled() {
		t.Error("清除后应恢复免密")
	}
}

// TestPasswordChangeDropsSessions 改密必须**作废所有会话**。
//
// 改密码的常见动机之一就是"怀疑有人拿到了会话"——
// 此时让旧会话继续有效等于没改。
func TestPasswordChangeDropsSessions(t *testing.T) {
	s := newPwServer(t)
	_ = postPw(t, s, "", "first-pw")
	tok := s.webAuth.NewSession()
	if !s.webAuth.ValidSession(tok) {
		t.Fatal("会话应有效")
	}
	if w := postPw(t, s, "first-pw", "second-pw"); w.Code != http.StatusOK {
		t.Fatal("改密失败")
	}
	if s.webAuth.ValidSession(tok) {
		t.Error("改密后旧会话必须失效（否则等于没改）")
	}
	if n := s.webAuth.SessionCount(); n != 0 {
		t.Errorf("会话数应为 0，实际 %d", n)
	}
}

// TestPasswordPersistAcrossRestart 密码落盘并在"重启"后生效。
func TestPasswordPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	s1 := &Server{cfg: cfg, webAuth: newWebAuth(""), consoleSecret: newConsoleSecretStore(dir)}
	if w := postPw(t, s1, "", "persisted-pw"); w.Code != http.StatusOK {
		t.Fatal("设置失败")
	}

	// 模拟重启：新建 Store 读取
	got, ok, err := newConsoleSecretStore(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok || got != "persisted-pw" {
		t.Errorf("落盘密码 = %q (存在=%v) want persisted-pw", got, ok)
	}
}

// TestPasswordClearedPersists 清除密码也要落盘 ——
// 否则重启后 config 里的旧密码会"复活"。
func TestPasswordClearedPersists(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.WebPassword = "from-config" // config 里有一个旧密码
	s := &Server{cfg: cfg, webAuth: newWebAuth(cfg.WebPassword), consoleSecret: newConsoleSecretStore(dir)}

	if w := postPw(t, s, "from-config", ""); w.Code != http.StatusOK {
		t.Fatal("清除失败")
	}
	// 落盘必须是"存在且为空"
	got, ok, err := newConsoleSecretStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("清除也必须落盘（否则重启后 config 里的旧密码会复活）")
	}
	if got != "" {
		t.Errorf("落盘应为空密码，得到 %q", got)
	}
}

// TestConsoleSecretStoreMissingFile 文件不存在 → 应回落到 config。
func TestConsoleSecretStoreMissingFile(t *testing.T) {
	st := newConsoleSecretStore(t.TempDir())
	pw, ok, err := st.Load()
	if err != nil {
		t.Fatalf("缺失文件不该报错: %v", err)
	}
	if ok {
		t.Error("文件不存在时 ok 应为 false（表示还没改过）")
	}
	if pw != "" {
		t.Errorf("密码应为空，得到 %q", pw)
	}
}

// TestConsoleSecretStoreAtomic 落盘是原子的（不留半截文件）。
func TestConsoleSecretStoreAtomic(t *testing.T) {
	dir := t.TempDir()
	st := newConsoleSecretStore(dir)
	if err := st.Save("pw-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save("pw-2"); err != nil {
		t.Fatal(err)
	}
	// 不该留下 .tmp
	if _, err := filepath.Glob(filepath.Join(dir, "*.tmp")); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(files) != 0 {
		t.Errorf("残留临时文件: %v", files)
	}
	pw, _, _ := st.Load()
	if pw != "pw-2" {
		t.Errorf("密码 = %q want pw-2", pw)
	}
}

// TestPasswordStatusEndpoint 状态端点不泄露密码本身。
func TestPasswordStatusEndpoint(t *testing.T) {
	s := newPwServer(t)
	_ = postPw(t, s, "", "secret-pw-xyz")
	req := httptest.NewRequest(http.MethodGet, "/admin/password", nil)
	w := httptest.NewRecorder()
	s.withAuth(s.handlePasswordStatus).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "secret-pw-xyz") {
		t.Fatal("状态端点泄露了密码明文！")
	}
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["enabled"] != true {
		t.Error("enabled 应为 true")
	}
	if m["min_length"] != float64(MinPasswordLen) {
		t.Errorf("min_length = %v", m["min_length"])
	}
}
