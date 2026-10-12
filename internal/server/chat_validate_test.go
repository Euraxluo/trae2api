package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/usermgr"
)

// newChatTestServer 构造只够跑 handleChat 前置校验的最小 Server。
func newChatTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg:      &config.Config{},
		pool:     pool.New(pool.Options{MaxInFlight: 1}),
		channels: map[string]*Channel{},
		users:    usermgr.New(filepath.Join(t.TempDir(), "users.json")),
	}
}

func doChat(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChat(w, req)
	return w
}

// TestUnknownRealmPrefixRejected 未知域前缀必须被拒绝。
//
// 背景（实测）：上游对未知**模型名**会静默回退到某个默认模型并返回 200。
// 于是把域写错（如 `trae2:glm-5.2`）会被当成模型名透传出去 ——
// 用户以为调用了 X、实际拿到 Y，还照样消耗额度。
// 真实 Trae 模型名里没有冒号，所以"含冒号且前缀不是已知域"几乎一定是域写错。
func TestUnknownRealmPrefixRejected(t *testing.T) {
	s := newChatTestServer(t)
	cases := []string{
		`{"model":"nosuchrealm:x","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"trae2:glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`, // cn 不是本项目的域
	}
	for _, body := range cases {
		w := doChat(s, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("未知域前缀应 400，得到 %d: %s", w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), "unknown_realm") {
			t.Errorf("错误码应为 unknown_realm: %s", w.Body.String())
		}
		// 错误信息要给出可用前缀，便于用户改正
		if !strings.Contains(w.Body.String(), "trae") || !strings.Contains(w.Body.String(), "solo") {
			t.Errorf("错误信息应列出可用域: %s", w.Body.String())
		}
	}
}

// TestKnownRealmPrefixAccepted 已知域前缀应通过前置校验（后续因无渠道而 503，但不是 400）。
func TestKnownRealmPrefixAccepted(t *testing.T) {
	s := newChatTestServer(t)
	for _, body := range []string{
		`{"model":"trae:glm-5.2","messages":[]}`,
		`{"model":"solo:glm-5.2","messages":[]}`,
		`{"model":"glm-5.2","messages":[]}`, // 裸名 → 默认域
	} {
		w := doChat(s, body)
		if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "unknown_realm") {
			t.Errorf("已知域/裸名不应被判为未知域: %s", w.Body.String())
		}
		// 无渠道 → 503（这正说明它过了域校验）
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("无渠道时应 503，得到 %d: %s", w.Code, w.Body.String())
		}
	}
}

// TestMalformedBodyRejected 请求体不是 JSON 时 400。
func TestMalformedBodyRejected(t *testing.T) {
	s := newChatTestServer(t)
	if w := doChat(s, `{not json`); w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，得到 %d", w.Code)
	}
}

// TestModelWithSlashNotRejected 自定义渠道用 `provider//model` 形式（斜杠而非冒号），
// 不应被域校验误判。
func TestModelWithSlashNotRejected(t *testing.T) {
	s := newChatTestServer(t)
	w := doChat(s, `{"model":"some-provider//some-model","messages":[]}`)
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "unknown_realm") {
		t.Errorf("斜杠形式不应被判为未知域: %s", w.Body.String())
	}
}
