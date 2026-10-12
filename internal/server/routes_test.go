package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/usermgr"
)

// TestHandlerRoutesRegister 路由表必须能注册成功。
//
// 为什么需要这个测试：Go 1.22+ 的 ServeMux 会在**注册时** panic 于模式冲突，
// 而注册发生在 Handler() 里 —— 编译期不报、单元测试不覆盖就只能在启动时炸。
// 本项目真实踩过一次：
//
//	panic: pattern "GET /" conflicts with pattern "/admin/users":
//	  GET / matches fewer methods than /admin/users, but has a more general path pattern
//
// 这个测试把"启动才炸"提前到"测试就炸"。
func TestHandlerRoutesRegister(t *testing.T) {
	s := &Server{
		cfg:   &config.Config{APIKey: ""},
		pool:  pool.New(pool.Options{MaxInFlight: 1, SoftRateBase: 0, SoftRateMax: 0, ModelBase: 0}),
		users: usermgr.New(filepath.Join(t.TempDir(), "users.json")),
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("路由注册 panic（模式冲突）: %v", r)
		}
	}()
	h := s.Handler()
	if h == nil {
		t.Fatal("Handler() 返回 nil")
	}
}

// TestHandlerRoutesResolve 关键路由必须能命中（防止路径写错但注册成功）。
func TestHandlerRoutesResolve(t *testing.T) {
	s := &Server{
		cfg:   &config.Config{APIKey: ""},
		pool:  pool.New(pool.Options{MaxInFlight: 1}),
		users: usermgr.New(filepath.Join(t.TempDir(), "users.json")),
	}
	h := s.Handler()

	cases := []struct {
		method, path string
		wantNot      int // 期望"不是这个状态码"（表示路由命中了 handler）
	}{
		{http.MethodGet, "/healthz", http.StatusNotFound},
		{http.MethodGet, "/v1/models", http.StatusNotFound},
		{http.MethodGet, "/status", http.StatusNotFound},
		{http.MethodGet, "/v1/credits", http.StatusNotFound},
		{http.MethodGet, "/admin/users", http.StatusNotFound},
		{http.MethodPost, "/admin/users", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == c.wantNot {
			t.Errorf("%s %s 返回 404 —— 路由未注册或路径写错", c.method, c.path)
		}
	}
}

// TestHandlerUnknownRoute 未注册路径必须是 404（而不是被 "GET /" 通配吞掉）。
func TestHandlerUnknownRoute(t *testing.T) {
	s := &Server{
		cfg:   &config.Config{APIKey: ""},
		pool:  pool.New(pool.Options{MaxInFlight: 1}),
		users: usermgr.New(filepath.Join(t.TempDir(), "users.json")),
	}
	h := s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/definitely-not-a-route", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("未知路径应 404，得到 %d", w.Code)
	}
}
