package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/realm"
	"trae2api/internal/traectl"
)

// TestIsClientNotInstalled 区分"客户端没装"与"凭据读不出来"。
//
// ## 为什么这条重要
//
// 国际版默认开启后，"本机没装国际版客户端"是**完全正常**的状态 —— 绝大多数用户
// 只装国内版。若把它当成错误打 WARN，每个这样的用户启动时都会看到两条
// 「账号加载失败」，从而以为网关坏了，并开始排查一个根本不存在的问题。
//
// ## Windows 的坑（这条测试真正守的东西）
//
// Windows 上"路径不存在"有两个不同的错误码：`ERROR_FILE_NOT_FOUND` 与
// `ERROR_PATH_NOT_FOUND`。Go 的 `os.IsNotExist` / `errors.Is(err, fs.ErrNotExist)`
// **只保证覆盖前者**，对后者可能返回 false。
//
// 也就是说：一个只靠 `fs.ErrNotExist` 判断的实现，在 Linux 上测试全绿，
// **到 Windows 上就会把"客户端没装"误报成 WARN** —— 这正是本项目主要的使用平台。
// 所以测试必须显式覆盖 Windows 的错误文本。
func TestIsClientNotInstalled(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"fs.ErrNotExist（Linux 风格）", fs.ErrNotExist, true},
		{"包装过的 fs.ErrNotExist", fmt.Errorf("读取凭据: %w", fs.ErrNotExist), true},
		{"Windows 路径不存在", errors.New("open C:\\Users\\x\\AppData\\Roaming\\Trae\\User\\globalStorage\\storage.json: The system cannot find the path specified."), true},
		{"Windows 文件不存在", errors.New("open C:\\x\\storage.json: The system cannot find the file specified."), true},
		{"Windows 小写系统错误", errors.New("the system cannot find the path specified"), true},
		{"Linux ENOENT 文本", errors.New("open /home/x/.config/Trae/storage.json: no such file or directory"), true},
		// 这些是**真问题**，绝不能当成"没装"
		{"权限问题", errors.New("open C:\\x\\storage.json: Access is denied."), false},
		{"JSON 损坏", errors.New("traectl: 解析 storage.json: unexpected end of JSON input"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isClientNotInstalled(c.err); got != c.want {
			t.Errorf("%s: isClientNotInstalled = %v want %v（err=%v）", c.name, got, c.want, c.err)
		}
	}
}

func TestBuildChannelsAndHandleModels(t *testing.T) {
	cfg := config.Default()
	_ = traectl.ConfigureIntl(traectl.IntlConfig{
		Enabled:      cfg.Intl.Enabled,
		UGHost:       cfg.Intl.UGHost,
		ChatHost:     cfg.Intl.ChatHost,
		TraeClientID: cfg.Intl.TraeClientID,
		SoloClientID: cfg.Intl.SoloClientID,
	})
	p := pool.New(pool.Options{MaxInFlight: 1})
	channels, err := BuildChannels(cfg, p, nil)
	if err != nil {
		t.Fatalf("BuildChannels failed: %v", err)
	}
	s := &Server{
		cfg:      cfg,
		pool:     p,
		channels: channels,
	}
	w := httptest.NewRecorder()
	s.handleModels(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("handleModels status: %d", w.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	foundRealms := map[string]bool{}
	for _, m := range resp.Data {
		r, _ := m["realm"].(string)
		if r != "" {
			foundRealms[r] = true
		}
	}
	for _, rk := range []string{realm.Trae, realm.Solo, realm.TraeIntl, realm.SoloIntl} {
		if !foundRealms[rk] {
			t.Errorf("realm %q not found in /v1/models output", rk)
		}
	}
}

func TestWithAuthConsoleCookie(t *testing.T) {
	wa := newWebAuth("admin123")
	tok := wa.NewSession()
	s := &Server{
		cfg:     &config.Config{APIKey: "secret-key"},
		webAuth: wa,
	}
	nextCalled := false
	handler := s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		if !isAdmin(r) {
			t.Error("expected admin from console cookie")
		}
		w.WriteHeader(http.StatusOK)
	})

	// 1) 未携带 Cookie 且未携带 API Key -> 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}

	// 2) 携带 trae2api_console Cookie -> 200
	nextCalled = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.AddCookie(&http.Cookie{Name: webAuthCookie, Value: tok})
	handler(rec, req)
	if rec.Code != http.StatusOK || !nextCalled {
		t.Fatalf("want 200, got %d (called=%v)", rec.Code, nextCalled)
	}
}

