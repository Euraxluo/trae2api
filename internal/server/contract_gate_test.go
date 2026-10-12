package server

// 本文件是**协调层收口的集成闸**（第 2 轮返工阻塞项 B4）。
//
// ## 它拦的是什么事故
//
// 第 1 轮交付里，4 个角色有 2 个（backend-accounts / backend-dsh）**零交付**：
// 源码文件不存在、契约端点整块缺失，但交付仍然进了验收 —— 因为当时
// 没有任何一道**机器可验**的关口去问一句"契约里的每个端点，真的能命中 handler 吗"。
//
// 角色自己的单元测试拦不住这件事：一个**不存在的文件**不会贡献任何失败的测试。
// 7 个端点缺失时 `go test ./...` 可以全绿。这就是"缺失穿透到验收"的机制。
//
// ## 这道闸的判据
//
// 对冻结契约里的每个端点发一次**真实 HTTP 往返**（走 Handler()，含全局安全头），
// 断言状态码**不是 404 / 405**：
//   - 404 = 路由压根没注册（后端角色没交付 / 抄错了路径名）
//   - 405 = 路径在但方法不对（GET/POST 写反，控制台点了按钮也不会通）
//
// 为什么断言"不是 404/405"而不是"必须是 200"：探针故意用最小/空请求体，
// 于是 400（缺参数）、403（权限）、503（未配置）都是**正确的**响应。
// 这道闸回答的是"端点存在且方法正确"，不是"业务逻辑对不对" ——
// 后者由各角色自己的验收标准（C9/C10/C11）负责。
//
// ## 为什么还要断言"命中了具体模式"
//
// 只有 404 判断有个假阳性：`GET /` 门户是通配路由，任何未注册的 GET 路径
// 都可能落到它上面并返回 200/401，于是"路径写错了"会被误判为"存在"。
// 用 ServeMux.Handler(req) 取出实际命中的 pattern，断言它不是 "/" 通配
// （记录中的 /console、/dashboard、/portal/status 这些精确模式才是真命中）。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"trae2api/internal/accountstore"
	"trae2api/internal/config"
	"trae2api/internal/traectl"
)

// contractProbeEndpoint 是清单里的一条冻结契约端点。
type contractProbeEndpoint struct {
	Role      string         `json:"role"`
	Criterion string         `json:"criterion"`
	Method    string         `json:"method"`
	Path      string         `json:"path"`
	Auth      string         `json:"auth"`
	ProbePath string         `json:"probe_path"`
	Body      map[string]any `json:"body"`
	// NotFoundOK 标记"这个端点**允许**用自己的业务逻辑返回 404"。
	//
	// 为什么要这个开关：2026-10-11 第一次跑这道闸时它报了 2 条 404
	// （/admin/users/{id}/role 与 /admin/users/{id}/reset-password），
	// 但两条其实都是**既有实现正确返回的"用户不存在"** —— 路由完全没问题。
	// 也就是说"404 = 端点缺失"这个判据**单靠状态码是不够的**：
	// 处理器自己的 not_found 与 ServeMux 的"没有这条路由"撞在同一个状态码上。
	//
	// 区分靠 pattern：命中具体模式（非空、非 "/"）说明路由在，
	// 此时的 404 是业务语义（资源不存在）；pattern 为空才是真缺失。
	// 这个字段是给"连路由都懒得分"的端点留一个显式豁免位 ——
	// 探针用不存在的 -id 去打它，404 属于预期行为而非缺失。
	NotFoundOK bool `json:"not_found_ok"`
}

type contractProbeManifest struct {
	RejectStatus []int                   `json:"reject_status"`
	Endpoints    []contractProbeEndpoint `json:"endpoints"`
}

// contractGateGlobalKey 是闸用的全局 API Key。非空才会让 withAuth 走
// "全局 Key = 管理员"分支 —— 闸必须真的经过鉴权中间件，才能证明
// 注册时挂的是 withAuth 而不是裸 handler（后者在生产里等于无需鉴权）。
const contractGateGlobalKey = "sk-contract-gate"

// contractGateRepoRoot 从**本文件自己的绝对路径**推出仓库根。
//
// 为什么不用相对路径（`..\..\.dsh-team\...`）：那条路依赖"go test 的 CWD
// 恰好是包目录"这个隐含前提。实测它确实成立（`go test ./internal/server/`
// 的 CWD 是 internal/server），所以相对路径版本能跑通 —— 但一旦有人用
// `-C`、从别的目录跑、或 go 工具链改变 CWD 约定，这道闸会**静默失效**：
// 找不到清单而我当时的写法是直接 t.Fatal，看起来"红了"，可如果哪天有人
// 图省事把 Fatal 改成 Skip，门禁就变成装饰。用 runtime.Caller 拿本文件的
// 真实路径是唯一不依赖 CWD 的定位方式（backend-accounts 的 notes.md 第 188 行
// 也提出了这条隐患，虽然它把可解析的第二个候选路径误判成了解析不到 ——
// 隐患本身是真的，所以这里改掉）。
func contractGateRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败 —— 无法定位本测试文件，闸失效")
	}
	// file = <repo>/internal/server/contract_gate_test.go
	dir := filepath.Dir(file)              // <repo>/internal/server
	return filepath.Dir(filepath.Dir(dir)) // <repo>
}

// contractGateManifestPath 定位冻结契约清单。
//
// 优先查找 internal/server/contract-endpoints.json（打包进仓库的文件），
// 兼容老开发环境中的 ..\.dsh-team 路径。
func contractGateManifestPath(t *testing.T) string {
	t.Helper()
	repo := contractGateRepoRoot(t)
	candidates := []string{
		filepath.Join(repo, "internal", "server", "contract-endpoints.json"),
		filepath.Join(repo, "..", ".dsh-team", "trae2api-console", "work", "coordinator", "contract-endpoints.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("找不到冻结契约清单（候选: %v）—— 清单是这道闸的输入，缺失即闸失效", candidates)
	return ""
}

func loadContractGateManifest(t *testing.T) contractProbeManifest {
	t.Helper()
	raw, err := os.ReadFile(contractGateManifestPath(t))
	if err != nil {
		t.Fatalf("读取冻结契约清单失败: %v", err)
	}
	var m contractProbeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("冻结契约清单不是合法 JSON: %v", err)
	}
	if len(m.Endpoints) == 0 {
		t.Fatal("冻结契约清单里一个端点都没有 —— 空清单会让这道闸永远通过（最坏的一种绿）")
	}
	if len(m.RejectStatus) == 0 {
		m.RejectStatus = []int{http.StatusNotFound, http.StatusMethodNotAllowed}
	}
	return m
}

// contractGateServer 构造闸用的 Server。
//
// 刻意用生产构造函数 New(cfg) 而不是手搓结构体：手搓的 Server 缺 accounts /
// metrics / webAuth 等字段，端点会走 nil 分支返回 503 —— 那样这道闸就只证明了
// "路由存在"，而证明不了"端点在生产构造路径下也不会内部炸掉"。
// 用 New() 顺带覆盖了"构造函数忘了接线"这一类只在启动时才暴露的事故
// （见 wiring_test.go 记录的那次 IdleTimeout 死代码事故）。
//
// CI / 裸机兼容：在无本地 Trae 客户端的环境下，向 accounts 目录注入一份
// 最小有效快照，让 New(cfg) 能成功构建渠道进入就绪态。
func contractGateServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.APIKey = contractGateGlobalKey
	cfg.WebPassword = ""

	store := accountstore.New(cfg.AccountsDir())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))

	snap := &traectl.CredentialSnapshot{
		Product:       "trae-cn",
		UserID:        "gate-user-1",
		Label:         "集成闸模拟账号",
		Token:         "tok-gate",
		RefreshToken:  "ref-gate",
		DeviceID:      "1234567890",
		MachineID:     "abcdef",
		PrivateKeyPEM: privPEM,
		PublicKeyPEM:  pubPEM,
	}
	if _, err := store.Add(snap); err != nil {
		t.Fatalf("初始化集成闸模拟账号失败: %v", err)
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New(cfg) 失败（闸无法在真实构造路径上运行）: %v", err)
	}
	return s
}

// contractGateProbe 对单个端点做真实 HTTP 往返，返回状态码与命中的路由模式。
func contractGateProbe(t *testing.T, ts *httptest.Server, mux *http.ServeMux, ep contractProbeEndpoint) (int, string) {
	t.Helper()
	path := ep.Path
	if ep.ProbePath != "" {
		path = ep.ProbePath
	}

	// 1) 路由表层面：到底命中了哪个模式（识别"被 GET / 通配吞掉"的假存在）
	_, pattern := mux.Handler(httptest.NewRequest(ep.Method, path, nil))
	if pattern == "" {
		pattern = "<无匹配>"
	}

	// 2) 真实往返：走 Handler()（含全局安全头 + withAuth）
	var body *strings.Reader
	if ep.Method == http.MethodGet || ep.Method == http.MethodDelete {
		body = strings.NewReader("")
	} else if len(ep.Body) > 0 {
		raw, err := json.Marshal(ep.Body)
		if err != nil {
			t.Fatalf("构造探针请求体失败: %v", err)
		}
		body = strings.NewReader(string(raw))
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(ep.Method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("构造请求失败 %s %s: %v", ep.Method, path, err)
	}
	if ep.Auth == "admin" {
		req.Header.Set("Authorization", "Bearer "+contractGateGlobalKey)
	}
	if body.Len() > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败（端点存在与否已无法判定）: %v", ep.Method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, pattern
}

// TestContractEndpointsExist 是协调层的**契约端点存在性集成闸**。
//
// 任何 404（未注册）或 405（方法不对）都直接失败，并逐条列出缺失端点 ——
// 输出要能直接抄进返工单，而不是让人自己去比对两份清单。
func TestContractEndpointsExist(t *testing.T) {
	manifest := loadContractGateManifest(t)
	s := contractGateServer(t)

	mux, ok := s.handler().(*http.ServeMux)
	if !ok {
		t.Fatalf("handler() 不是 *http.ServeMux —— 闸无法读取真实路由表")
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	type row struct {
		ep      contractProbeEndpoint
		status  int
		pattern string
	}
	rows := make([]row, 0, len(manifest.Endpoints))
	for _, ep := range manifest.Endpoints {
		status, pattern := contractGateProbe(t, ts, mux, ep)
		rows = append(rows, row{ep: ep, status: status, pattern: pattern})
	}

	// 逐条打印：交付物要能当"集成探测原始留痕"归档，不能只有 FAIL 时才说话。
	t.Logf("契约端点集成探测（%d 条，拒绝状态码 %v）", len(rows), manifest.RejectStatus)
	for _, r := range rows {
		t.Logf("  [%-16s %-4s] %-7s %-42s -> HTTP %d  pattern=%s",
			r.ep.Role, r.ep.Criterion, r.ep.Method, probeDisplayPath(r.ep), r.status, r.pattern)
	}

	fails := make([]string, 0)
	for _, r := range rows {
		// 通配吞噬："命中"的模式是 GET / 门户（任何未注册的 GET 都会落到它上面）
		wildcard := r.pattern == "/" || r.pattern == "" || r.pattern == "<无匹配>"
		if r.ep.Path != "/" && wildcard {
			if r.status == http.StatusNotFound || r.status == http.StatusMethodNotAllowed {
				fails = append(fails, fmt.Sprintf("%s %s (%s/%s) 未命中具体路由（模式 %q）-> HTTP %d",
					r.ep.Method, probeDisplayPath(r.ep), r.ep.Role, r.ep.Criterion, r.pattern, r.status))
			}
			continue
		}
		// 命中具体模式：路由确实存在。此时只有 405 仍是硬失败（方法注册错了），
		// 404 需看该端点是否显式允许"资源不存在"语义。
		if r.status == http.StatusMethodNotAllowed {
			fails = append(fails, fmt.Sprintf("%s %s (%s/%s) 方法未注册 -> HTTP %d（模式 %q）",
				r.ep.Method, probeDisplayPath(r.ep), r.ep.Role, r.ep.Criterion, r.status, r.pattern))
			continue
		}
		if r.status == http.StatusNotFound && !r.ep.NotFoundOK {
			fails = append(fails, fmt.Sprintf("%s %s (%s/%s) -> HTTP 404（模式 %q；若这是处理器自己的 not_found，"+
				"请在清单里把 not_found_ok 置 true 并说明理由）",
				r.ep.Method, probeDisplayPath(r.ep), r.ep.Role, r.ep.Criterion, r.pattern))
		}
	}
	if len(fails) > 0 {
		sort.Strings(fails)
		t.Fatalf("契约端点存在性闸不通过：%d/%d 条端点不可用（404=未注册，405=方法不对）：\n  - %s\n"+
			"拒绝交付：端点缺失属于角色未交付，不得进入验收。",
			len(fails), len(rows), strings.Join(fails, "\n  - "))
	}
}

func probeDisplayPath(ep contractProbeEndpoint) string {
	if ep.ProbePath != "" {
		return ep.ProbePath
	}
	return ep.Path
}

// TestContractGateRejectsMissingRoute 是**闸的闸**：证明上面那道闸真的会失败。
//
// 一个永远返回"通过"的闸比没有闸更糟 —— 它给出的是虚假的安全感。
// 所以这里用一个肯定不存在的端点当输入，断言闸的判据（404 → 拒绝）确实成立。
func TestContractGateRejectsMissingRoute(t *testing.T) {
	s := contractGateServer(t)
	mux, ok := s.handler().(*http.ServeMux)
	if !ok {
		t.Fatal("handler() 不是 *http.ServeMux")
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 第 1 轮真实缺失过的端点形态：未注册路径 → 必须被识别为"没命中具体路由"。
	//
	// 注意这里刻意**不断言状态码是 404**：Go 1.22+ 的 ServeMux 对
	// "路径前缀存在但这条路径没注册"会返回 405（Method Not Allowed），
	// 而 routes_test.go 里 `GET /definitely-not-a-route` 得到 404 ——
	// 两者都对，取决于是否存在更一般的模式。把状态码写死会让这道"闸的闸"
	// 自身变成假失败（已实测：这里第一版就写死了 404，结果失败）。
	// 真正稳定的判据是**模式为空**：没有命中任何注册模式。
	status, pattern := contractGateProbe(t, ts, mux, contractProbeEndpoint{
		Method: http.MethodPost, Path: "/admin/accounts/definitely-not-registered-gate-selftest",
	})
	if pattern != "" && pattern != "<无匹配>" {
		t.Fatalf("未注册路径竟然命中了模式 %q —— 这道闸的「缺失」判据失效", pattern)
	}
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Fatalf("未注册路径应 404/405，得到 %d（pattern=%q）—— 判据失效", status, pattern)
	}

	// 方法不对的判据也要**用存在的端点自证**，而不是挑一个固定端点。
	//
	// 第一版这里写死了 /admin/accounts/revive，结果同一轮里它恰好还没被注册，
	// 自检报了一个与被测无关的失败（"缺失已穿透"）—— 一道自检不该依赖
	// 被测对象当下的完成度。所以改成：从清单里顺序找第一个能给出 405 的端点，
	// 用事实证明"ServeMux 的方法区分能力在这份路由表上确实生效"。
	var methodProven string
	for _, ep := range loadContractGateManifest(t).Endpoints {
		wrong := http.MethodGet
		if ep.Method == http.MethodGet {
			wrong = http.MethodPost
		}
		st, pattern := contractGateProbe(t, ts, mux, contractProbeEndpoint{Method: wrong, Path: ep.Path})
		if st == http.StatusMethodNotAllowed {
			methodProven = fmt.Sprintf("%s %s -> 405（模式 %q）", wrong, ep.Path, pattern)
			break
		}
	}
	if methodProven == "" {
		t.Errorf("找遍整个契约清单，没有任何端点对错误方法返回 405 —— " +
			"ServeMux 的方法区分没有生效，这道闸的 405 判据形同虚设（清单里的端点可能都被同一个通配模式吞了）")
	} else {
		t.Logf("405 判据自证：%s", methodProven)
	}
}

// TestContractRoleDeliverablesPresent 是角色**交付物存在性**门禁。
//
// 判据（来自返工要求 2）：每个角色必须有 work/<role>/verify.txt，否则该角色
// 视为**未交付**。第 1 轮 backend-accounts / backend-dsh 连 notes.md 都没有，
// 却以"完成"进入验收 —— 这个门禁就是补上那一刀。
//
// 同时要求 notes.md：verify.txt 只有命令输出、没有取舍说明的话，
// 下一轮的人无法判断"没做的事是有意还是漏了"。
func TestContractRoleDeliverablesPresent(t *testing.T) {
	manifest := loadContractGateManifest(t)
	repo := contractGateRepoRoot(t)
	workRoot := filepath.Join(repo, "..", ".dsh-team", "trae2api-console", "work")
	if _, err := os.Stat(workRoot); err != nil {
		// 在 CI 或独立构建环境中（如 GitHub Actions），仓库独立检出，外层 .dsh-team 并不随仓库提交
		t.Skipf("非工作流执行环境，角色交付目录不存在（%s），跳过交付物检查", workRoot)
	}

	roles := map[string]bool{}
	for _, ep := range manifest.Endpoints {
		roles[ep.Role] = true
	}
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Strings(names)

	missing := make([]string, 0)
	for _, role := range names {
		for _, f := range []string{"verify.txt", "notes.md"} {
			p := filepath.Join(workRoot, role, f)
			st, err := os.Stat(p)
			if err != nil {
				missing = append(missing, fmt.Sprintf("%s 缺 %s", role, f))
				continue
			}
			if st.Size() == 0 {
				missing = append(missing, fmt.Sprintf("%s 的 %s 是空文件（等于没交付）", role, f))
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("角色交付物门禁不通过（work/<role>/ 下必须同时有 notes.md 与 verify.txt）：\n  - %s",
			strings.Join(missing, "\n  - "))
	}
}

// TestContractGateProbeMethodMismatchNothingHappens 固定一个容易误判的事实：
// **用错方法探测不会执行任何有副作用的逻辑**。
//
// 这道闸的 405 自证会拿清单里的端点逐个试错方法（例如 POST /admin/dsh/stop
// 的 GET）。如果哪天有人把这层保护去掉（比如给所有路径注册了通配方法），
// 那"探测"就会变成"真的停服务 / 真删账号"。这个测试用
// /admin/accounts/delete、/admin/dsh/start、/admin/dsh/stop 钉住该语义：
// 用错方法进不去处理器，池内账号数一个都不能少。
func TestContractGateProbeMethodMismatchNothingHappens(t *testing.T) {
	s := contractGateServer(t)
	before := len(s.pool.List())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, ep := range []contractProbeEndpoint{
		{Method: http.MethodGet, Path: "/admin/accounts/delete"},
		{Method: http.MethodGet, Path: "/admin/dsh/start"},
		{Method: http.MethodGet, Path: "/admin/dsh/stop"},
	} {
		status, pattern := contractGateProbe(t, ts, muxOf(t, s), ep)
		if status == http.StatusOK {
			t.Errorf("用 %s 探测 %s 得到了 200（模式 %q）—— 错误的 HTTP 方法竟然执行了处理器，探测会产生副作用",
				ep.Method, ep.Path, pattern)
		}
	}
	if after := len(s.pool.List()); after != before {
		t.Errorf("错方法探测改变了池内账号数：%d → %d（探测必须无副作用）", before, after)
	}
}

// muxOf 取真实路由表（带断言，避免各处重复写类型断言）。
func muxOf(t *testing.T, s *Server) *http.ServeMux {
	t.Helper()
	mux, ok := s.handler().(*http.ServeMux)
	if !ok {
		t.Fatal("handler() 不是 *http.ServeMux —— 闸无法读取真实路由表")
	}
	return mux
}
