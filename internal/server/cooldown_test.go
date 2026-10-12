package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trae2api/internal/pool"
	"trae2api/internal/traectl"
)

// newTestServer 构造一个只带账号池与冷却配置的最小 Server，
// 用于测试错误→冷却的映射（不需要真实凭据）。
func newTestServer(t *testing.T) (*Server, *pool.Account) {
	t.Helper()
	p := pool.New(pool.Options{
		MaxInFlight: 1, BreakerThreshold: 3, BreakerCooldown: time.Minute,
		SoftRateBase: 50 * time.Millisecond, SoftRateMax: time.Second,
		ModelBase: 50 * time.Millisecond, PickGap: 0,
	})
	a := &pool.Account{UID: "trae-cn-test", Realm: "trae", Owner: "public", Label: "test"}
	p.Add(a)
	return &Server{pool: p, modelCooldown: 50 * time.Millisecond}, a
}

func stOf(t *testing.T, s *Server) pool.Status {
	t.Helper()
	list := s.pool.List()
	if len(list) != 1 {
		t.Fatalf("账号数 = %d want 1", len(list))
	}
	return list[0]
}

// TestNoteFailureRateLimit 4011 限流 → 账号级软冷却（短时避让）。
func TestNoteFailureRateLimit(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.UpstreamError{Status: 200, Path: "sse:error",
		Body: `{"code":4011,"message":"rate limit"}`}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if !st.Cooling {
		t.Fatal("4011 应触发账号级冷却")
	}
	if st.CoolKind != "soft" {
		t.Errorf("4011 应为软冷却，得到 %q", st.CoolKind)
	}
	if len(st.ModelCooling) != 0 {
		t.Errorf("4011 不应产生模型级冷却: %v", st.ModelCooling)
	}
}

// TestNoteFailureAuthHard 凭证失效 → 账号级**硬**冷却（调了必然失败，不参与兜底）。
func TestNoteFailureAuthHard(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.UpstreamError{Status: http.StatusUnauthorized, Body: `{"code":1001}`}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if !st.Cooling || st.CoolKind != "hard" {
		t.Fatalf("401 应为硬冷却，得到 cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
	// 硬冷却账号不得被选中（含兜底）
	if got := s.pool.Pick("trae", "glm-5.2", nil, nil); got != nil {
		t.Errorf("硬冷却账号不应被选中，得到 %v", got.UID)
	}
}

// TestNoteFailureModelScoped 未分类的上游码 → 只冷却该模型（换模型即豁免）。
//
// 注意这里刻意用**未分类**的码（走 default 分支），而不是 4001 ——
// 4001 是"请求本身不合法"，见 TestNoteFailureClientRequestNotPunished。
func TestNoteFailureModelScoped(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.UpstreamError{Status: http.StatusOK, Body: `{"code":12345,"message":"unclassified"}`}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if st.Cooling {
		t.Error("未分类上游码不应产生账号级冷却（否则一个坏模型会冻掉整个账号）")
	}
	if _, ok := st.ModelCooling["glm-5.2"]; !ok {
		t.Fatalf("未分类上游码应产生模型级冷却，得到 %v", st.ModelCooling)
	}
	// 换模型应豁免
	if got := s.pool.Pick("trae", "kimi-k3", nil, nil); got == nil {
		t.Error("换模型应可正常选号")
	}
	// 同模型应被排除
	if got := s.pool.Pick("trae", "glm-5.2", nil, nil); got != nil {
		t.Errorf("被冷却的模型不应选中账号，得到 %v", got.UID)
	}
}

// TestNoteFailureServerError 5xx → 账号级短冷却 + 记一次失败（参与熔断）。
func TestNoteFailureServerError(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.UpstreamError{Status: http.StatusBadGateway, Body: "bad gateway"}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if !st.Cooling || st.CoolKind != "soft" {
		t.Fatalf("5xx 应为软冷却，得到 cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
	if st.Fails != 1 {
		t.Errorf("5xx 应记一次失败，得到 %d", st.Fails)
	}
}

// TestNoteFailureRefreshError 续期失败 → 硬冷却（需用户重新登录）。
func TestNoteFailureRefreshError(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.RefreshError{HTTPStatus: 200, Code: 20405, Message: "device proof required"}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if !st.Cooling || st.CoolKind != "hard" {
		t.Fatalf("续期失败应为硬冷却，得到 cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
}

// TestNoteFailureAlreadyStreaming 已开始输出时不叠加额外冷却判断。
func TestNoteFailureAlreadyStreaming(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := fmt.Errorf("%w: 上游断了", errAlreadyStreaming)
	s.noteFailure(ch, a, "glm-5.2", err)

	if st := stOf(t, s); st.Cooling {
		t.Error("errAlreadyStreaming 不应再叠加冷却（内层已按真实错误处置）")
	}
}

// TestNoteFailureTransportNotPunished 传输层失败**不罚号**。
//
// 这是对齐 workbuddy2api 的明确规则：DNS/连接/TLS/超时是本地网络问题，
// 不是账号问题。若罚号，一次断网会把整个健康池冷却一遍。
// 上层轮换（maxRotate）已能处理"这个号这次不通"。
func TestNoteFailureTransportNotPunished(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.TransportError{Op: "chat", Err: errors.New("connection reset by peer")}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if st.Cooling {
		t.Error("传输层失败不应冷却账号")
	}
	if st.Fails != 0 {
		t.Errorf("传输层失败不应记失败计数，得到 %d", st.Fails)
	}
	if st.ErrCount != 0 {
		t.Errorf("传输层失败不应计入错误数，得到 %d", st.ErrCount)
	}
	if !st.Healthy {
		t.Error("传输层失败后账号应仍为健康")
	}
}

// TestNoteFailureUnclassifiedNotPunished 未分类错误同样不罚号（保守策略）。
//
// 理由：要罚号必须先能把它归类到**明确的账号级问题**（401/限流/额度）。
// 未分类就罚，等于用"我们没看懂"当作账号有罪的证据。
func TestNoteFailureUnclassifiedNotPunished(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	s.noteFailure(ch, a, "glm-5.2", errors.New("something inexplicable"))

	st := stOf(t, s)
	if st.Cooling {
		t.Error("未分类错误不应冷却账号")
	}
	if st.Fails != 0 {
		t.Errorf("未分类错误不应记失败计数，得到 %d", st.Fails)
	}
}

// TestNoteFailureUnwrapTransport 包装过的 TransportError 也必须被识别。
func TestNoteFailureUnwrapTransport(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	inner := &traectl.TransportError{Op: "chat(重试)", Err: errors.New("dial tcp: timeout")}
	wrapped := fmt.Errorf("上层包装: %w", inner)
	s.noteFailure(ch, a, "glm-5.2", wrapped)

	if st := stOf(t, s); st.Cooling || st.Fails != 0 {
		t.Errorf("包装后的传输层失败仍不应罚号: cooling=%v fails=%d", st.Cooling, st.Fails)
	}
}

// TestUpstreamErrorCodeExtraction 业务码提取（含上游返回的错误体形态）。
func TestUpstreamErrorCodeExtraction(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"code":4011,"message":"rate limit"}`, 4011},
		{`{"code":4001,"message":"bad request"}`, 4001},
		{`{"code":0}`, 0},
		{`not json`, 0},
		{``, 0},
	}
	for _, c := range cases {
		ue := &traectl.UpstreamError{Status: 200, Body: c.body}
		if got := ue.UpstreamCode(); got != c.want {
			t.Errorf("UpstreamCode(%q)=%d want %d", c.body, got, c.want)
		}
	}
}

// TestWriteOpenAIErrorShape 错误信封必须符合 OpenAI 形态（客户端按此解析）。
func TestWriteOpenAIErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, http.StatusTooManyRequests, "rate_limit_exceeded", "太频繁了")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"error"`, `"message"`, `"type"`, "rate_limit_exceeded", "太频繁了"} {
		if !strings.Contains(body, want) {
			t.Errorf("响应体缺少 %s: %s", want, body)
		}
	}
}

// TestNoteFailureClientRequestNotPunished 请求本身不合法时**不冷却任何东西**。
//
// 实测：上游对 `assistant` 消息 content 为空（null / "" / 缺失）返回
// 业务码 4001 "param is invalid"。这**既不是账号的问题、也不是模型的问题** ——
// 同一账号、同一模型服务其他客户端完全正常。
//
// 冷却任何东西都是误伤：会把一个健康模型对所有人封掉几分钟。
// （这个误伤是我在实测时发现的：连续探测被上一次 400 的冷却挡成了 503。）
func TestNoteFailureClientRequestNotPunished(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}

	for _, e := range []*traectl.UpstreamError{
		{Status: http.StatusBadRequest, Body: `{"code":4001,"message":"param is invalid"}`},
		{Status: http.StatusOK, Body: `{"code":4001,"message":"param is invalid"}`},
		{Status: http.StatusBadRequest, Body: `{"code":0,"message":"bad request"}`},
	} {
		s.noteFailure(ch, a, "glm-5.2", e)
		st := stOf(t, s)
		if st.Cooling {
			t.Errorf("HTTP %d code=%d：请求不合法不该冷却账号", e.Status, e.UpstreamCode())
		}
		if len(st.ModelCooling) != 0 {
			t.Errorf("HTTP %d code=%d：请求不合法不该冷却模型（会误伤其他客户端）: %v",
				e.Status, e.UpstreamCode(), st.ModelCooling)
		}
		if st.Fails != 0 {
			t.Errorf("请求不合法不该记失败计数: %d", st.Fails)
		}
	}
	// 且账号仍可被选中（模型没被冻）
	if got := s.pool.Pick("trae", "glm-5.2", nil, nil); got == nil {
		t.Error("账号与模型都不该被冻，应仍可选中")
	}
}

// TestIsClientRequestError 分类边界。
func TestIsClientRequestError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusBadRequest, `{"code":4001}`, true},
		{http.StatusOK, `{"code":4001}`, true},
		{http.StatusBadRequest, `{}`, true},
		{http.StatusOK, `{"code":12345}`, false},
		{http.StatusOK, `{"code":4011}`, false}, // 限流有专门分支
		{http.StatusUnauthorized, `{"code":0}`, false},
		{http.StatusInternalServerError, `{"code":0}`, false},
	}
	for _, c := range cases {
		ue := &traectl.UpstreamError{Status: c.status, Body: c.body}
		if got := isClientRequestError(ue); got != c.want {
			t.Errorf("isClientRequestError(HTTP %d, %s) = %v want %v", c.status, c.body, got, c.want)
		}
	}
}
