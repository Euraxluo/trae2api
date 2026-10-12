package server

// 本文件是 dsh 内嵌（虚拟智能体模型 + 网页 UI 启停）的测试。
//
// ## 设计原则：不依赖开发机上装没装 DeepSeek Harness
//
// "假 dsh"就是**测试二进制自己**（靠环境变量分派，见 TestDSHHelperProcess），
// 通过 dshLaunchResolver 注入。这样：
//   - 在任何机器上都能跑，不需要 DeepSeek Harness、不需要网络、不需要下载 Node.js；
//   - 能精确构造"超时""提前退出""不打印地址"这些**真实 dsh 很难复现**的路径。
//
// 反过来，如果测试直接调真 dsh，那它在 CI 上只能被跳过 —— 而"被跳过的测试"
// 等于没有测试，只是让人以为有覆盖。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/usermgr"
)

// ---- 测试脚手架 ----

// TestDSHHelperProcess 是"假 dsh"：同一个测试二进制，靠 TRAE2API_DSH_FAKE 分派。
//
// 环境变量没设置时它就是个空测试（在父进程里直接返回），所以可以安全地留在包里。
func TestDSHHelperProcess(t *testing.T) {
	mode := os.Getenv("TRAE2API_DSH_FAKE")
	if mode == "" {
		return
	}
	switch mode {
	case "exit":
		// 模拟"起来就崩"（dsh 的配置/凭据有问题时就是这样）
		os.Exit(3)
	case "noprint":
		// 模拟"起来了但从不打印地址"（卡在初始化里）
		time.Sleep(60 * time.Second)
		return
	}
	// 真实 dsh 的 stdout 契约（本机实测，见 dsh.go 顶部注释）：
	//     dsh web: http://127.0.0.1:52368/?token=<token>
	fmt.Println("dsh web: http://127.0.0.1:54321/?token=fake-token-abc123")
	// 等父进程来收：SIGTERM/SIGINT 都要能优雅退出。
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ch:
	case <-time.After(60 * time.Second):
	}
}

// fakeDSHPlan 构造一个指向"假 dsh"的启动计划。
func fakeDSHPlan(mode string) dshLaunchPlan {
	return dshLaunchPlan{
		Kind: "test-fake",
		Argv: []string{os.Args[0], "-test.run=TestDSHHelperProcess"},
		Env:  []string{"TRAE2API_DSH_FAKE=" + mode},
		// 与真实路径一致：--port 0 + 从 stdout 解析地址。
		PortFromStdout: true,
		TrustedHosts:   []string{"127.0.0.1", "localhost"},
	}
}

// withFakeDSH 把启动计划解析替换成"假 dsh"。
func withFakeDSH(t *testing.T, mode string) {
	t.Helper()
	old := dshLaunchResolver
	dshLaunchResolver = func() (dshLaunchPlan, error) { return fakeDSHPlan(mode), nil }
	t.Cleanup(func() { dshLaunchResolver = old })
}

// newDSHTestServer 构造一个最小可用的 Server（不跑 New，不需要真实账号）。
func newDSHTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg:   &config.Config{APIKey: ""},
		pool:  pool.New(pool.Options{MaxInFlight: 1}),
		users: usermgr.New(filepath.Join(t.TempDir(), "users.json")),
	}
}

// ---- C10：dsh 出现在模型清单里 ----

// TestDSHModelInModelsList 是 C10 的**单元级**证明：/v1/models 里必须有
// id == "dsh" 的条目，且它被显式标注为 agent（不参与账号池调度）。
func TestDSHModelInModelsList(t *testing.T) {
	s := newDSHTestServer(t)
	w := httptest.NewRecorder()
	s.handleModels(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d want 200", w.Code)
	}
	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	var found map[string]any
	for _, m := range resp.Data {
		if m["id"] == dshModelID {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatalf("模型清单里没有 id==%q 的条目（拿到 %d 个模型）", dshModelID, len(resp.Data))
	}
	// agent 标记：控制台/上层靠它区分"虚拟智能体"与"上游模型"
	if found["agent"] != true {
		t.Errorf("dsh 条目应带 agent=true，实际 %v", found["agent"])
	}
	if found["virtual"] != true {
		t.Errorf("dsh 条目应带 virtual=true，实际 %v", found["virtual"])
	}
	if found["schedulable"] != false {
		t.Errorf("dsh 条目应带 schedulable=false（它不能进账号池调度），实际 %v", found["schedulable"])
	}
	if found["object"] != "model" {
		t.Errorf("object = %v want model", found["object"])
	}
	// 清单里**不**应该出现两次 dsh（渠道目录万一也叫 dsh 就会重复）
	n := 0
	for _, m := range resp.Data {
		if m["id"] == dshModelID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("dsh 条目出现 %d 次，应为 1 次", n)
	}
}

// TestDSHIsDSHModel 模型名匹配规则。
//
// 关键取舍：只认裸名 dsh（与 provider//model 形式），**不认** `trae:dsh` ——
// 那是"域写错了"，交给 realm 校验报更准确的错。
func TestDSHIsDSHModel(t *testing.T) {
	cases := map[string]bool{
		"dsh":              true,
		"DSH":              true,
		" dsh ":            true,
		"provider//dsh":    true,
		"dsh:glm-5.2":      false,
		"trae:dsh":         false,
		"dsh2":             false,
		"glm-5.2":          false,
		"":                 false,
		"provider//dshell": false,
	}
	for in, want := range cases {
		if got := isDSHModel(in); got != want {
			t.Errorf("isDSHModel(%q) = %v want %v", in, got, want)
		}
	}
}

// TestDSHChatRejectsDSHModel 虚拟模型**不能**进对话链路。
//
// 为什么这条重要：dsh 不在任何渠道目录里。放行的后果是 strict_models=false 时
// 请求会真的打到上游、静默回退到默认模型并扣额度（用户以为调用了 dsh）。
// 这里断言它被**明确拒绝**（400 + 专用错误码），而不是走到"渠道不可用"或上游。
func TestDSHChatRejectsDSHModel(t *testing.T) {
	s := newDSHTestServer(t)
	h := s.Handler()

	body := `{"model":"dsh","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("model=dsh 应被拒（400），得到 %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if resp.Error.Code != "dsh_agent_not_routable" {
		t.Errorf("错误码 = %q want dsh_agent_not_routable", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "Agent") {
		t.Errorf("错误信息应把用户指到 Agent 标签页：%q", resp.Error.Message)
	}

	// 反证：普通模型名**不**走这条分支（这里会因为没有渠道而报 channel_unavailable，
	// 说明请求确实被交给了 handleChat）。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if strings.Contains(w2.Body.String(), "dsh_agent_not_routable") {
		t.Errorf("普通模型不该被 dsh 分支拦下：%s", w2.Body.String())
	}
}

// ---- C11：控制台 Agent 标签页 ----

// TestDSHConsoleHasAgentTab 是 C11 的**页面级**证明：控制台 HTML 里必须有
// Agent/dsh 标签页、iframe 容器，以及三个管理端点的调用。
func TestDSHConsoleHasAgentTab(t *testing.T) {
	s := newDSHTestServer(t)
	w := httptest.NewRecorder()
	s.handleConsole(w, httptest.NewRequest(http.MethodGet, "/console", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d want 200", w.Code)
	}
	html := w.Body.String()
	// 注意（console-ui 改绑时的同步）：控制台已整体换成参考实现那套页面，
	// dsh 面板的 DOM id / 函数名随之改名（旧名 → 新名）：
	//   id="tab-agent"      → id="tabContent-dsh"
	//   id="tab-btn-agent"  → id="mainTabBtn-dsh"
	//   function showTab    → function switchMainTab
	//   function dshStart   → function startDsh
	//   function dshStop    → function stopDsh
	// 断言意图不变：控制台必须有一个可切换的 dsh 面板、内嵌 iframe、
	// 三个端点绑定，以及同站 cookie 的地址选择逻辑。
	for _, want := range []string{
		"Agent (dsh)",            // 标签本身
		`id="tabContent-dsh"`,    // 标签页面板
		`id="mainTabBtn-dsh"`,    // 标签按钮
		"<iframe",                // 内嵌容器
		"admin/dsh/start",        // 一键启动
		"admin/dsh/stop",         // 一键停止
		"admin/dsh/status",       // 状态轮询
		"function switchMainTab", // 切换逻辑
		"function startDsh",      // 启动按钮的实现
		"function stopDsh",       // 停止按钮的实现
		"function dshIframeURL",  // 内嵌地址选择（同站 cookie 那一层）
	} {
		if !strings.Contains(html, want) {
			t.Errorf("控制台 HTML 里缺少 %q", want)
		}
	}
	// 控制台必须放宽 frame-src，否则 iframe 会被 CSP 拦掉（且只在 DevTools 里可见）
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-src") {
		t.Errorf("控制台 CSP 缺少 frame-src，iframe 会被拦：%q",
			w.Header().Get("Content-Security-Policy"))
	}
	// 但控制台自身仍不允许被别的站点嵌套
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Errorf("frame-ancestors 应保持 'self'：%q", w.Header().Get("Content-Security-Policy"))
	}
}

// TestDSHEndpointsRegistered 三个端点必须存在（且不是 404）。
func TestDSHEndpointsRegistered(t *testing.T) {
	s := newDSHTestServer(t)
	h := s.Handler()
	cases := []struct{ method, path string }{
		{http.MethodPost, "/admin/dsh/start"},
		{http.MethodPost, "/admin/dsh/stop"},
		{http.MethodGet, "/admin/dsh/status"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code == http.StatusNotFound {
			t.Errorf("%s %s 未注册（404）", c.method, c.path)
		}
	}
}

// TestDSHStatusEndpointStructured C11：/admin/dsh/status 返回结构化 JSON。
//
// 关键点：**没装 node / 没装 dsh 时也必须 200**。状态本身（缺依赖）是正常结果，
// 用 5xx 表达会让控制台把"没装 node"误判成"接口坏了"。
func TestDSHStatusEndpointStructured(t *testing.T) {
	s := newDSHTestServer(t)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/dsh/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d want 200（缺依赖也是正常状态）：%s", w.Code, w.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("状态不是合法 JSON: %v", err)
	}
	for _, k := range []string{"object", "state", "running", "node", "hint", "chat_forwarding"} {
		if _, ok := st[k]; !ok {
			t.Errorf("状态缺少字段 %q：%v", k, st)
		}
	}
	if st["object"] != "dsh.status" {
		t.Errorf("object = %v want dsh.status", st["object"])
	}
	if st["running"] != false {
		t.Errorf("没启动过时 running 应为 false，实际 %v", st["running"])
	}
	if st["state"] != dshStateStopped {
		t.Errorf("state = %v want %v", st["state"], dshStateStopped)
	}
	// 未实现的能力必须**显式**标注，不能让用户以为选了 dsh 就能对话
	fwd, ok := st["chat_forwarding"].(map[string]any)
	if !ok {
		t.Fatalf("chat_forwarding 结构不对：%v", st["chat_forwarding"])
	}
	if fwd["supported"] != false {
		t.Errorf("chat_forwarding.supported 应为 false（未实现转发），实际 %v", fwd["supported"])
	}
	// node 探测结果必须如实透出
	node, ok := st["node"].(map[string]any)
	if !ok {
		t.Fatalf("node 结构不对：%v", st["node"])
	}
	if _, ok := node["found"]; !ok {
		t.Errorf("node 应带 found 字段：%v", node)
	}
}

// ---- 启停：幂等 / 优雅 / 不留孤儿 ----

// TestDSHStartStopIdempotent 启停的幂等性。
//
// 这是"按钮会被连点"的直接防线：重复 start 必须复用同一个进程（否则每次点击
// 都会多一个 dsh，端口和内存都翻倍），重复 stop 不能报错。
func TestDSHStartStopIdempotent(t *testing.T) {
	withFakeDSH(t, "ok")
	m := newDSHManager()

	st, err := m.Start()
	if err != nil {
		t.Fatalf("首次启动失败: %v（status=%v）", err, st)
	}
	if st["state"] != dshStateRunning || st["running"] != true {
		t.Fatalf("启动后状态应为 running：%v", st)
	}
	if st["port"] != 54321 {
		t.Errorf("端口应从子进程输出解析得到 54321，实际 %v", st["port"])
	}
	if st["token"] != "fake-token-abc123" {
		t.Errorf("token 应被解析出来，实际 %v", st["token"])
	}
	pid1 := st["pid"]
	if pid1 == nil {
		t.Fatalf("运行态必须有 pid：%v", st)
	}

	// 幂等：第二次 start 必须是同一个进程
	st2, err := m.Start()
	if err != nil {
		t.Fatalf("重复启动失败: %v", err)
	}
	if st2["pid"] != pid1 {
		t.Fatalf("重复 start 产生了第二个进程：pid %v → %v", pid1, st2["pid"])
	}
	if st2["starts"] != 1 {
		t.Errorf("成功启动次数应为 1，实际 %v", st2["starts"])
	}

	m.mu.Lock()
	run := m.run
	m.mu.Unlock()
	if run == nil {
		t.Fatal("运行态下 m.run 不该为空")
	}

	// 停止 + 幂等
	if _, err := m.Stop(); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	st3, err := m.Stop()
	if err != nil {
		t.Fatalf("重复 stop 不该报错: %v", err)
	}
	if st3["state"] != dshStateStopped || st3["running"] != false {
		t.Errorf("停止后状态应为 stopped：%v", st3)
	}
	if st3["pid"] != nil {
		t.Errorf("停止后不该还留着 pid：%v", st3["pid"])
	}

	// 进程必须真的被回收：exited 已关闭 且 Wait 已返回（否则就是僵尸/孤儿）
	select {
	case <-run.exited:
	default:
		t.Fatal("停止后子进程没有被回收（exited 未关闭）")
	}
	if run.cmd.ProcessState == nil || !run.cmd.ProcessState.Exited() {
		t.Fatal("停止后子进程没有被 Wait 回收（ProcessState 为空）")
	}
}

// TestDSHStopWithoutStartIsNoop 没启动过就 stop：幂等，不报错，不编造状态。
func TestDSHStopWithoutStartIsNoop(t *testing.T) {
	withFakeDSH(t, "ok")
	m := newDSHManager()
	st, err := m.Stop()
	if err != nil {
		t.Fatalf("未启动时 stop 不该报错: %v", err)
	}
	if st["running"] != false || st["state"] != dshStateStopped {
		t.Errorf("状态不对：%v", st)
	}
}

// TestDSHStartFailureReportsState 子进程"起来就崩"时必须如实报告，且不留进程。
func TestDSHStartFailureReportsState(t *testing.T) {
	withFakeDSH(t, "exit")
	m := newDSHManager()

	st, err := m.Start()
	if err == nil {
		t.Fatal("子进程立即退出时 Start 应报错")
	}
	if st["state"] != dshStateFailed {
		t.Errorf("state = %v want %v", st["state"], dshStateFailed)
	}
	if st["running"] != false {
		t.Errorf("失败时 running 必须为 false：%v", st)
	}
	if st["pid"] != nil {
		t.Errorf("失败后不该留着 pid：%v", st["pid"])
	}
	if msg, _ := st["last_error"].(string); msg == "" {
		t.Error("失败时必须给出 last_error，否则用户无从下手")
	}
	m.mu.Lock()
	leftover := m.run
	m.mu.Unlock()
	if leftover != nil {
		t.Errorf("失败后 m.run 必须清空，实际 %v", leftover)
	}
}

// TestDSHStartTimeoutReclaimsProcess 启动超时（子进程不打印地址）必须**回收**进程。
//
// 这是"不要留孤儿"最容易被漏掉的一条路径：超时返回时如果只是 return，
// 那个卡住的 dsh 会一直活着占着端口。
func TestDSHStartTimeoutReclaimsProcess(t *testing.T) {
	withFakeDSH(t, "noprint")
	oldWait := dshStartWait
	dshStartWait = 700 * time.Millisecond
	t.Cleanup(func() { dshStartWait = oldWait })

	m := newDSHManager()
	run, err := m.spawn(fakeDSHPlan("noprint"))
	if err == nil {
		t.Fatal("子进程不打印地址时应超时失败")
	}
	if run == nil {
		t.Fatal("spawn 失败时应把 run 一起返回，否则无法确认进程被回收")
	}
	if run.cmd.ProcessState == nil || !run.cmd.ProcessState.Exited() {
		t.Fatal("超时后子进程没有被回收 —— 这就是孤儿进程")
	}
}

// TestDSHParseURL 解析真实 dsh 打印的地址。
func TestDSHParseURL(t *testing.T) {
	// 本机实测的原样输出（桌面版 0.2.0-rc.2）
	real := "dsh web: http://127.0.0.1:52368/?token=yYm8JXsUNyqNieU1sBz_S0g5cT8_83mXZY9nPeu-ZTs"
	got, ok := parseDSHURL(real)
	if !ok {
		t.Fatalf("解析真实输出失败：%q", real)
	}
	if got != "http://127.0.0.1:52368/?token=yYm8JXsUNyqNieU1sBz_S0g5cT8_83mXZY9nPeu-ZTs" {
		t.Errorf("解析结果 = %q", got)
	}
	host, port, token, err := splitDSHURL(got)
	if err != nil {
		t.Fatalf("拆分失败: %v", err)
	}
	if host != "127.0.0.1" || port != 52368 || token == "" {
		t.Errorf("拆分结果 = %q %d %q", host, port, token)
	}

	// 不该被误认的行
	for _, bad := range []string{
		"",
		"正在启动 dsh web…",
		"http://127.0.0.1:52368/",               // 没有 token
		"https://example.com/?token=x",          // 端口不是数字
		"(node:648) [DEP0180] DeprecationWarni", // 真实的 stderr 噪声
	} {
		if u, ok := parseDSHURL(bad); ok {
			t.Errorf("不该把 %q 当访问地址（得到 %q）", bad, u)
		}
	}
}

// TestDSHStateHintCoversAllStates 每个状态都要有可执行的提示。
//
// 用户看到 node_missing 只知道"缺东西"；提示里必须写清"装什么、怎么装"。
func TestDSHStateHintCoversAllStates(t *testing.T) {
	states := []string{
		dshStateStopped, dshStateStarting, dshStateRunning,
		dshStateStopping, dshStateNodeMissing, dshStateNotFound, dshStateFailed,
	}
	for _, s := range states {
		if got := dshStateHint(s); strings.TrimSpace(got) == "" {
			t.Errorf("状态 %q 没有提示文案", s)
		}
	}
	if !strings.Contains(dshStateHint(dshStateNodeMissing), "node") {
		t.Error("node_missing 的提示必须提到 node")
	}
}

// TestDSHLaunchErrorState 缺依赖时的状态映射。
func TestDSHLaunchErrorState(t *testing.T) {
	if got := dshLaunchErrorState(errDSHNodeMissing); got != dshStateNodeMissing {
		t.Errorf("node 缺失应映射到 %q，得到 %q", dshStateNodeMissing, got)
	}
	if got := dshLaunchErrorState(errDSHNotFound); got != dshStateNotFound {
		t.Errorf("dsh 缺失应映射到 %q，得到 %q", dshStateNotFound, got)
	}
	if got := dshLaunchErrorState(fmt.Errorf("别的错")); got != dshStateFailed {
		t.Errorf("其它错误应映射到 %q，得到 %q", dshStateFailed, got)
	}
}

// TestDSHStartReportsNodeMissing 解析不出启动方式时，端点要如实报缺什么。
func TestDSHStartReportsNodeMissing(t *testing.T) {
	old := dshLaunchResolver
	dshLaunchResolver = func() (dshLaunchPlan, error) { return dshLaunchPlan{}, errDSHNodeMissing }
	t.Cleanup(func() { dshLaunchResolver = old })

	s := newDSHTestServer(t)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/dsh/start", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺 node 时 start 应 503，得到 %d：%s", w.Code, w.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if st["ok"] != false {
		t.Errorf("ok 应为 false：%v", st["ok"])
	}
	if st["state"] != dshStateNodeMissing {
		t.Errorf("state = %v want %v", st["state"], dshStateNodeMissing)
	}
	if hint, _ := st["hint"].(string); !strings.Contains(hint, "node") {
		t.Errorf("提示应说明缺 node：%q", hint)
	}

	// 但 status 仍是 200（"缺依赖"是状态，不是接口故障）
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/admin/dsh/status", nil))
	if w2.Code != http.StatusOK {
		t.Errorf("status 应恒为 200，得到 %d", w2.Code)
	}
	var st2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &st2)
	if st2["state"] != dshStateNodeMissing {
		t.Errorf("status 应保留缺依赖状态，得到 %v", st2["state"])
	}
}

// TestDSHStopKeepsDependencyState stop 不该擦掉"缺什么"的信息。
//
// 这是个很容易写错的地方：stop 时无脑把 state 设成 stopped，
// 用户就会看到"什么都没发生"，而真正的问题是缺 node。
func TestDSHStopKeepsDependencyState(t *testing.T) {
	old := dshLaunchResolver
	dshLaunchResolver = func() (dshLaunchPlan, error) { return dshLaunchPlan{}, errDSHNodeMissing }
	t.Cleanup(func() { dshLaunchResolver = old })

	m := newDSHManager()
	if _, err := m.Start(); err == nil {
		t.Fatal("应报缺 node")
	}
	st, err := m.Stop()
	if err != nil {
		t.Fatalf("stop 不该报错: %v", err)
	}
	if st["state"] != dshStateNodeMissing {
		t.Errorf("stop 后应保留 node_missing，实际 %v", st["state"])
	}
}

// TestDSHStopForExitKillsImmediately 网关退出路径必须**立刻**回收子进程。
//
// 这条是实测事故的回归防线：第一版走的是完整 Stop()（先 softKill、再等 5 秒
// grace），而网关从收到信号到进程退出只有**几十毫秒** —— 实测结果是网关退了、
// dsh 活得好好的（孤儿）。stopForExit 必须只做同步强杀，不等优雅窗口。
func TestDSHStopForExitKillsImmediately(t *testing.T) {
	withFakeDSH(t, "ok")
	m := newDSHManager()
	if _, err := m.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	m.mu.Lock()
	run := m.run
	m.mu.Unlock()
	if run == nil {
		t.Fatal("运行态下 m.run 不该为空")
	}

	start := time.Now()
	m.stopForExit()
	elapsed := time.Since(start)

	if elapsed > dshStopGrace/2 {
		t.Errorf("stopForExit 耗时 %v —— 它还在等优雅窗口，网关早就退出了", elapsed)
	}
	// 状态必须当场落定：网关不会再有机会更新它
	st := m.Status()
	if st["state"] != dshStateStopped || st["running"] != false {
		t.Errorf("stopForExit 后状态不对：%v", st)
	}
	if st["pid"] != nil {
		t.Errorf("stopForExit 后不该留着 pid：%v", st["pid"])
	}
	// 重复调用必须安全（信号可能被投递多次）
	m.stopForExit()

	select {
	case <-run.exited:
	case <-time.After(15 * time.Second):
		t.Fatal("stopForExit 后子进程没有被回收 —— 这就是孤儿")
	}
	if run.cmd.ProcessState == nil || !run.cmd.ProcessState.Exited() {
		t.Error("子进程没有被 Wait 回收（ProcessState 为空）")
	}
}

// TestDSHDesktopDetectionChecksAsarNotInnerPath 桌面版探测必须查 **app.asar 文件本身**。
//
// 这条测试是实测事故的回归防线：`app.asar` 是**文件**不是目录，里面
// `dsh/node_modules/.../cli.js` 这个路径在磁盘上不存在（Electron 在运行期把
// asar 当目录解析）。第一版代码查的是 cli.js 的存在性，于是"明明装了桌面版
// 却报 dsh_not_found" —— 而且单测全绿，因为那些测试只覆盖"找不到时如实报错"。
func TestDSHDesktopDetectionChecksAsarNotInnerPath(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Programs", "DeepSeek Harness")
	if err := os.MkdirAll(filepath.Join(install, "resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 造出"桌面版安装"的最小形状：一个 exe + 一个 app.asar（**普通文件**）
	for _, f := range []string{
		filepath.Join(install, "DeepSeek Harness.exe"),
		filepath.Join(install, "resources", "app.asar"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCALAPPDATA", root)

	plan, ok := dshDesktopPlan()
	if !ok {
		t.Fatal("桌面版布局存在时必须能被探测到（查 cli.js 而不是 app.asar 就会漏掉）")
	}
	if plan.Kind != "desktop" {
		t.Errorf("Kind = %q want desktop", plan.Kind)
	}
	if len(plan.Argv) < 3 || !strings.Contains(plan.Argv[2], "app.asar") {
		t.Errorf("应把 asar 内的 CLI 路径交给子进程：%v", plan.Argv)
	}
	// Electron 必须退化成 node，否则会去开窗口而不是跑 CLI
	foundEnv := false
	for _, e := range plan.Env {
		if e == "ELECTRON_RUN_AS_NODE=1" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("缺少 ELECTRON_RUN_AS_NODE=1：%v", plan.Env)
	}
	// 端口必须是动态的：命令行里带 --port 0，地址从 stdout 解析
	if !plan.PortFromStdout {
		t.Error("桌面版计划应走 --port 0 + 从 stdout 解析地址（端口动态分配）")
	}
	joined := strings.Join(plan.Argv, " ")
	for _, want := range []string{"--port 0", "--no-open", "--host 127.0.0.1", "--trusted-host localhost"} {
		if !strings.Contains(joined, want) {
			t.Errorf("启动参数缺少 %q：%s", want, joined)
		}
	}
}

// TestDSHEndpointsRequireAdmin 启停是有副作用的操作，必须要求管理员。
func TestDSHEndpointsRequireAdmin(t *testing.T) {
	s := newDSHTestServer(t)
	s.cfg = &config.Config{APIKey: "secret-key"}
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/dsh/start", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 调 start 应 401，得到 %d：%s", w.Code, w.Body.String())
	}
}
