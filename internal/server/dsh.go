package server

// 本文件把 DeepSeek Harness (dsh) 作为一个**虚拟智能体模型**接进网关：
//
//	GET  /v1/models            —— 多一个 id == "dsh" 的条目（agent: true）
//	POST /admin/dsh/start      —— 一键拉起 dsh 网页 UI（幂等）
//	POST /admin/dsh/stop       —— 优雅停止（先 TERM，超时再 KILL；幂等）
//	GET  /admin/dsh/status     —— 结构化状态（含端口、URL、token、日志尾巴）
//
// 为什么是"虚拟模型"而不是真模型：dsh 是跑在**本机**上的一个 agent 进程，
// 它没有额度、不属于任何渠道、也不该被选号器调度。把它塞进账号池会污染调度
// 统计 —— 会出现一个"永远不会冷却、也永远不会成功"的假账号。
// 因此 /v1/chat/completions 里 model=dsh 会被**明确拒绝**（见 handleChatRouter），
// 而不是当裸模型名透传给上游（上游对未知模型名会静默回退到默认模型并返回 200，
// 用户以为调用了 dsh、实际拿到别的模型的回答，还照样扣额度）。
//
// ## 本机实测到的 dsh 契约（2026-10，桌面版 0.2.0-rc.2）
//
//	dsh web --host 127.0.0.1 --port 0 --no-open [--trusted-host <authority>]
//
//   - `--port 0` 让**操作系统**分配空闲端口，dsh 把最终地址打印到 stdout：
//     `dsh web: http://127.0.0.1:52368/?token=<token>`
//     —— 端口动态分配，不和用户其他程序抢固定端口。
//   - 访问需要 token：不带 token 访问根路径返回 401，带 token 会 303 到 `./`
//     并下发 `dsh-auth-*` cookie（HttpOnly + SameSite=Strict）。
//   - 响应头里**没有** X-Frame-Options / frame-ancestors，可以被 iframe 内嵌；
//     但网关控制台自己的 CSP 是 `default-src 'self'`（frame-src 回落到它），
//     所以控制台单独放宽了 frame-src（见 server.go 的 consoleCSP）。
//   - SameSite=Strict 的 cookie 只在**同站**请求里发送，所以内嵌时 iframe 必须
//     和控制台用同一个主机名（都用 127.0.0.1 或都用 localhost）。为了允许
//     `localhost`，启动时加了 `--trusted-host localhost`（实测有效）。
//
// ## 未实现（如实标注，不猜 API）
//
// 把 /v1/chat/completions 的对话**转发**给 dsh agent 没有做：dsh 的对话不走
// 简单 HTTP，而是它自己的 WebSocket/RPC 通道（`@deepseek-ai/dsh-api-gateway` +
// 会话控制器），契约无法从本机可靠确认。猜一个形状只会得到一个"看起来能用、
// 实际全错"的转发。状态里用 chat_forwarding 字段显式暴露这一点。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---- 常量与状态 ----

const (
	// dshModelID 是虚拟智能体模型的 id（/v1/models 里 id == "dsh"）。
	dshModelID = "dsh"

	// dshHost 是 dsh 网页 UI 的绑定地址。
	//
	// 只绑回环：dsh 的网页 UI 带完整的本机文件/终端能力，绑 0.0.0.0 等于把
	// 本机控制权开放到局域网。要远程用请自己套反向代理 + 认证。
	dshHost = "127.0.0.1"

	// dshStartTimeout 是等待 dsh 打印访问地址（或端口可连）的上限。
	//
	// 为什么给到 90s：冷启动要加载 profile、插件与语言服务，实测首次可达十几秒；
	// 给太短会把"慢"误报成"起不来"。
	dshStartTimeout = 90 * time.Second

	// dshStopGrace 是"优雅停止"的等待窗口，超时后强杀。
	dshStopGrace = 5 * time.Second

	// dshStopKillWait 是强杀后等待进程真正被回收的上限。
	dshStopKillWait = 10 * time.Second

	// dshLogTailMax 是状态里保留的子进程输出行数（排障用）。
	dshLogTailMax = 20

	// dshPortPlaceholder 是 TRAE2API_DSH_CMD 里的端口占位符。
	dshPortPlaceholder = "{port}"
)

// 进程状态。全部是字符串，直接进 JSON（前端不必维护枚举映射）。
const (
	dshStateStopped     = "stopped"
	dshStateStarting    = "starting"
	dshStateRunning     = "running"
	dshStateStopping    = "stopping"
	dshStateNodeMissing = "node_missing"
	dshStateNotFound    = "dsh_not_found"
	dshStateFailed      = "failed"
)

var (
	// errDSHNodeMissing 表示本机没有 Node.js 运行时。
	errDSHNodeMissing = errors.New("本机没有找到 node（dsh 需要 Node.js 运行时）")
	// errDSHNotFound 表示找不到可用的 dsh 入口。
	errDSHNotFound = errors.New("本机没有找到可用的 dsh（DeepSeek Harness）")
)

// dshLaunchResolver 是"怎么把 dsh 拉起来"的解析函数。
//
// 做成包级变量是为了让测试能注入一个**可控的假 dsh**（同一个测试二进制 + 环境
// 变量分派），这样测试不依赖开发机上是否真的装了 DeepSeek Harness —— 否则这个
// 测试在 CI 上要么被跳过、要么变成"只有作者机器能过"的假绿。
var dshLaunchResolver = resolveDSHLaunch

// dshStartWait 是等待"就绪"（拿到地址 / 端口可连）的实际上限。
//
// 单独抽成变量是为了让测试把它调到几百毫秒：否则"启动超时后进程必须被回收"
// 这条测试要跑 90 秒，实际结果就是没人会跑它。
var dshStartWait = dshStartTimeout

// ---- 虚拟模型条目 ----

// dshModelEntry 返回 /v1/models 里虚拟智能体模型 dsh 的条目。
//
// 字段刻意与真实模型条目**保持同构**（realm / display_name / model_type ...），
// 前端不必为它写特例；同时用 agent=true、virtual=true、schedulable=false 三个
// 字段显式声明"它不是上游模型"。
func dshModelEntry() map[string]any {
	return map[string]any{
		"id":           dshModelID,
		"object":       "model",
		"owned_by":     "DeepSeek Harness",
		"realm":        "agent",
		"display_name": "dsh（DeepSeek Harness Agent）",
		"model_type":   "agent",

		// 三个"它不是真模型"的显式标记。
		// 为什么用三个而不是一个：agent 给前端做展示分支，virtual 给上层做
		// "别当上游模型"的判断，schedulable=false 给调度/统计侧一个**否定式**
		// 约束（默认值 true 的字段最容易被漏判）。
		"agent":       true,
		"virtual":     true,
		"schedulable": false,

		"multimodal":          false,
		"native_tool_calling": true,
		"parallel_tool_calls": false,
		"context_window":      0,
		"prompt_max_tokens":   0,
		"is_beta":             false,
		"fee_model_level":     0,
		"multiplier":          "—",
		"tags":                []string{"agent", "dsh", "virtual"},
		"description": "内嵌的 DeepSeek Harness 智能体。它不是上游模型：不参与账号池调度、" +
			"不消耗额度。请在控制台「Agent (dsh)」标签页启停并使用它的网页 UI。",
	}
}

// isDSHModel 判断请求里的模型名是不是虚拟智能体 dsh。
//
// 兼容 `provider//model` 形式（自定义渠道的写法），但**不**接受 `trae:dsh`
// 这类带域前缀的写法 —— 那是域写错，交给 realm 校验去报更准确的错。
func isDSHModel(model string) bool {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "//"); i >= 0 {
		m = m[i+2:]
	}
	return strings.EqualFold(m, dshModelID)
}

// handleChatRouter 在进入真正的对话处理之前拦下虚拟智能体模型 dsh。
//
// 为什么必须在**入口**拦：dsh 不在任何渠道的模型目录里，若放行，
// strict_models=true 时是一个含糊的 "unknown_model"，strict_models=false 时
// 更糟 —— 会真的打到上游、静默回退到默认模型并扣额度。两种都不是我们想要的。
//
// 代价：这里要把请求体读进内存再放回去。可以接受 —— handleChat 本来就会
// 把整个 body 读进内存（见 readBody）。
func (s *Server) handleChatRouter(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && isDSHModel(probe.Model) {
		s.rejectDSHModel(w)
		return
	}
	s.handleChat(w, r)
}

// rejectDSHModel 明确拒绝把 dsh 当上游模型调用。
func (s *Server) rejectDSHModel(w http.ResponseWriter) {
	st := s.dshAgent().Status()
	msg := "dsh 是内嵌的智能体（DeepSeek Harness），不参与账号池调度、也没有额度概念：" +
		"网关不会把它当上游模型转发。请打开控制台「Agent (dsh)」标签页使用它的网页 UI"
	if running, _ := st["running"].(bool); running {
		msg += "（当前已在运行）"
	} else {
		msg += "（当前未运行，可在该标签页一键启动）"
	}
	writeOpenAIError(w, http.StatusBadRequest, "dsh_agent_not_routable", msg)
}

// ---- 启动计划 ----

// dshLaunchPlan 描述"怎么把 dsh 网页 UI 拉起来"。
type dshLaunchPlan struct {
	// Kind 供状态展示与排障：env / node / path / desktop。
	Kind string
	// Argv 完整命令行（argv[0] 是可执行文件）。
	Argv []string
	// Env 追加在 os.Environ() 之后的环境变量。
	Env []string
	// PortFromStdout 为真时，命令行里带的是 `--port 0`，真实端口与访问
	// token 从子进程 stdout 打印的访问地址里解析。
	PortFromStdout bool
	// Port 是 PortFromStdout=false 时由网关**预先分配**的端口。
	Port int
	// TrustedHosts 是启动时声明为可信的主机名（--trusted-host）。
	// 控制台据此决定内嵌时能不能换用 location.hostname（见 console.go）。
	TrustedHosts []string
	// Note 是人类可读的来源说明。
	Note string
}

// resolveDSHLaunch 按优先级挑一个可用的启动方式。
//
// 优先级：显式覆盖 > 显式入口脚本 > PATH 上的 dsh > 桌面版安装目录。
// 为什么不直接硬编码桌面版路径：那是厂商安装布局，会随版本变化；
// 放在最后作为"开箱即用"的兜底，并且每一项都做存在性检查。
func resolveDSHLaunch() (dshLaunchPlan, error) {
	// 1) 显式覆盖：TRAE2API_DSH_CMD（整条命令行）。
	//
	// 约定：命令行里出现 {port} 就替换成网关分配的空闲端口；
	// 没有 {port} 就自动追加 `--port 0` 并改从 stdout 解析地址。
	if raw := strings.TrimSpace(os.Getenv("TRAE2API_DSH_CMD")); raw != "" {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			return dshLaunchPlan{}, fmt.Errorf("TRAE2API_DSH_CMD 是空的")
		}
		plan := dshLaunchPlan{Kind: "env", Note: "TRAE2API_DSH_CMD"}
		if containsPlaceholder(fields) {
			port, err := freePort()
			if err != nil {
				return plan, fmt.Errorf("分配端口失败: %w", err)
			}
			plan.Argv = replacePlaceholder(fields, strconv.Itoa(port))
			plan.Port = port
			plan.TrustedHosts = []string{dshHost}
			return plan, nil
		}
		plan.Argv = append(fields, "--port", "0")
		plan.PortFromStdout = true
		plan.TrustedHosts = []string{dshHost}
		return plan, nil
	}

	// 2) 显式指定的 dsh 入口脚本 + PATH 上的 node。
	if entry := strings.TrimSpace(os.Getenv("TRAE2API_DSH_ENTRY")); entry != "" {
		if node, err := exec.LookPath("node"); err == nil && fileExists(entry) {
			plan := dshWebPlan("node", []string{node, "--expose-internals", entry})
			plan.Note = "TRAE2API_DSH_ENTRY=" + entry
			return plan, nil
		}
	}

	// 3) PATH 上的 dsh（官方 CLI 的常规安装方式）。
	if p, err := exec.LookPath("dsh"); err == nil {
		plan := dshWebPlan("path", []string{p})
		plan.Note = "PATH 上的 dsh: " + p
		return plan, nil
	}

	// 4) 桌面版安装目录里的 dsh CLI。
	//
	// 注意：这一项**不需要 node** —— 桌面版自带的 dsh 是打包进 app.asar 的
	// JS 入口，只能用 Electron 二进制以 ELECTRON_RUN_AS_NODE=1 方式跑
	//（app.asar 不是真目录，普通 node 读不到里面的文件）。
	if plan, ok := dshDesktopPlan(); ok {
		return plan, nil
	}

	// 都不可用：如实报告缺的是哪一环，让用户知道该装什么。
	if _, err := exec.LookPath("node"); err != nil {
		return dshLaunchPlan{}, errDSHNodeMissing
	}
	return dshLaunchPlan{}, errDSHNotFound
}

// dshWebPlan 拼出 `dsh web` 的启动参数。
func dshWebPlan(kind string, argv []string) dshLaunchPlan {
	return dshLaunchPlan{
		Kind: kind,
		Argv: append(argv, "web",
			"--host", dshHost,
			// --port 0 = 让操作系统挑空闲端口（不硬编码，避免和用户其他程序冲突）
			"--port", "0",
			// --no-open = 不要自动弹浏览器：网关是服务端，弹窗只会在服务器上乱开窗口
			"--no-open",
			// 允许用 localhost 这个主机名访问（实测：token 的 authority 会跟着
			// 请求的 Host 走）。控制台如果是从 http://localhost:7870 打开的，
			// iframe 必须也用 localhost 才能带上 SameSite=Strict 的登录 cookie。
			"--trusted-host", "localhost",
			"--trusted-host", dshHost,
		),
		PortFromStdout: true,
		TrustedHosts:   []string{dshHost, "localhost"},
	}
}

// dshDesktopInstall 描述一份桌面版安装。
//
// ⚠️ Asar 是**文件**不是目录：`resources\app.asar` 是一个 120MB 的归档，
// 里面 `dsh\node_modules\...\cli.js` 这个路径在磁盘上**不存在**（Electron 在
// 运行期把 asar 当目录解析）。所以存在性检查必须查 Asar 本身 —— 查 Cli 会
// 永远为 false，表现为"明明装了桌面版却报 dsh_not_found"（这个 bug 是实测抓到的，
// 单元测试抓不到：它只测"找不到时如实报错"，不测"装了就找得到"）。
type dshDesktopInstall struct {
	Exe  string // Electron 可执行文件
	Asar string // app.asar 文件
	Cli  string // asar 内的 dsh CLI 虚拟路径（交给子进程用）
}

// dshDesktopPlan 在桌面版 DeepSeek Harness 的安装目录里找 dsh CLI。
func dshDesktopPlan() (dshLaunchPlan, bool) {
	for _, c := range dshDesktopCandidates() {
		if !fileExists(c.Exe) || !fileExists(c.Asar) {
			continue
		}
		plan := dshWebPlan("desktop", []string{c.Exe, "--expose-internals", c.Cli})
		// ELECTRON_RUN_AS_NODE=1 让 Electron 二进制退化成 node 跑这个脚本。
		plan.Env = append(plan.Env, "ELECTRON_RUN_AS_NODE=1")
		plan.Note = "DeepSeek Harness 桌面版：" + filepath.Dir(c.Exe)
		return plan, true
	}
	return dshLaunchPlan{}, false
}

// dshDesktopCandidates 返回桌面版安装的候选列表。
//
// 平台差异只在布局上：Windows 是 <root>\resources\app.asar\...，
// macOS 是 <App>.app/Contents/Resources/app.asar\...。Linux 的官方安装布局
// 未能从本机确认，所以**不猜** —— 留空并让用户用 TRAE2API_DSH_CMD 指定。
func dshDesktopCandidates() []dshDesktopInstall {
	cliRel := filepath.Join("dsh", "node_modules", "@deepseek-ai",
		"dsh-desktop-host", "lib", "cli.js")
	mk := func(root, asarDir, exeName string) dshDesktopInstall {
		asar := filepath.Join(root, asarDir, "app.asar")
		return dshDesktopInstall{
			Exe:  filepath.Join(root, exeName),
			Asar: asar,
			Cli:  filepath.Join(asar, cliRel),
		}
	}
	switch runtime.GOOS {
	case "windows":
		var roots []string
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			roots = append(roots,
				filepath.Join(la, "Programs", "DeepSeek Harness"),
				filepath.Join(la, "DeepSeek Harness"))
		}
		for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
			if pf != "" {
				roots = append(roots, filepath.Join(pf, "DeepSeek Harness"))
			}
		}
		out := make([]dshDesktopInstall, 0, len(roots))
		for _, r := range roots {
			out = append(out, mk(r, "resources", "DeepSeek Harness.exe"))
		}
		return out
	case "darwin":
		return []dshDesktopInstall{
			mk("/Applications/DeepSeek Harness.app/Contents", "Resources", "MacOS/DeepSeek Harness"),
		}
	}
	return nil
}

func containsPlaceholder(fields []string) bool {
	for _, f := range fields {
		if strings.Contains(f, dshPortPlaceholder) {
			return true
		}
	}
	return false
}

func replacePlaceholder(fields []string, port string) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.ReplaceAll(f, dshPortPlaceholder, port)
	}
	return out
}

// freePort 让操作系统分配一个空闲端口。
//
// 注意这里的固有竞态：监听 → 读端口 → 关闭 → 交给子进程，中间有一个窗口
// 可能被别的程序抢走。所以**默认路径根本不用它**（默认是 `--port 0`，
// 端口由 dsh 自己拿、自己打印）。只有用户在 TRAE2API_DSH_CMD 里写了
// {port} 时才走这条退路。
func freePort() (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(dshHost, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("非预期的监听地址类型 %T", l.Addr())
	}
	return addr.Port, nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---- 地址解析 ----

// dshURLRe 从一行输出里挑出候选地址。
//
// 实测 dsh 的打印格式：
//
//	dsh web: http://127.0.0.1:52368/?token=yYm8JXsUNyqNieU1sBz_S0g5cT8_83mXZY9nPeu-ZTs
var dshURLRe = regexp.MustCompile(`https?://[^\s"'<>]+`)

// parseDSHURL 从子进程输出里解析访问地址。
//
// 要求必须带 token 且端口是数字：没有 token 的地址嵌进 iframe 只会得到 401，
// 那还不如在启动阶段就明确报"没等到可用地址"。
func parseDSHURL(line string) (string, bool) {
	raw := dshURLRe.FindString(line)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Query().Get("token") == "" {
		return "", false
	}
	if _, err := strconv.Atoi(u.Port()); err != nil {
		return "", false
	}
	return u.String(), true
}

// splitDSHURL 拆出 host / port / token。
func splitDSHURL(raw string) (string, int, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, "", err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return "", 0, "", fmt.Errorf("地址 %q 里没有端口", raw)
	}
	return u.Hostname(), port, u.Query().Get("token"), nil
}

// ---- 进程管理器 ----

// dshRun 是一次具体的 dsh 子进程运行。
type dshRun struct {
	cmd          *exec.Cmd
	plan         dshLaunchPlan
	pid          int
	host         string
	port         int
	token        string
	url          string
	startedAt    time.Time
	trustedHosts []string

	// exited 在 cmd.Wait() 返回后关闭（由 wait 协程关闭）。
	// exitErr 在该协程里写、在 exited 关闭后读 —— 由 channel close 建立 happens-before。
	exited  chan struct{}
	exitErr error
}

// dshNodeInfo 是 Node.js 运行时探测结果。
type dshNodeInfo struct {
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// dshManager 管理 dsh 网页 UI 子进程的生命周期。
//
// 并发模型：mu 保护状态字段；慢操作（起进程、等就绪、停进程）**不在锁内**。
// 幂等靠状态机而不是"先查再改"：start 时状态已是 starting/running 就直接返回，
// 不会产生第二个进程。
type dshManager struct {
	mu     sync.Mutex
	run    *dshRun
	state  string
	plan   dshLaunchPlan
	node   dshNodeInfo
	sigCh  chan os.Signal
	logMu  sync.Mutex
	log    []string
	last   string
	starts int
	// startDone 在"一次 start 流程"结束时关闭，供并发调用者等待。
	startDone chan struct{}
}

// newDSHManager 构造管理器并**立即**探测一次 node。
//
// 为什么在构造时探：nodeInfo 要跑一个子进程（node --version），
// 放进状态锁里会让 status 接口偶发卡住；放在构造时（每次 Server 只一次）最干净。
func newDSHManager() *dshManager {
	m := &dshManager{state: dshStateStopped}
	m.node = detectNode()
	return m
}

// detectNode 用 exec.LookPath("node") 探测系统已有的 Node.js。
//
// 刻意**不下载** Node.js：几十 MB 的下载在沙箱/内网里不可靠，
// 而且用户机器上往往已经有（Trae 客户端、nvm、桌面版 Harness 都自带）。
func detectNode() dshNodeInfo {
	path, err := exec.LookPath("node")
	if err != nil {
		return dshNodeInfo{Found: false, Error: "PATH 上没有找到 node"}
	}
	info := dshNodeInfo{Found: true, Path: path}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, path, "--version").Output(); err == nil {
		info.Version = strings.TrimSpace(string(out))
	}
	return info
}

// dshAgent 取（必要时懒创建）dsh 管理器。
//
// 懒创建让测试里直接构造的 Server（不跑 New）也能用这些端点，
// 也让"从不碰 Agent 标签页"的部署完全不为它付出代价。
func (s *Server) dshAgent() *dshManager {
	s.dshMu.Lock()
	defer s.dshMu.Unlock()
	if s.dsh == nil {
		s.dsh = newDSHManager()
	}
	return s.dsh
}

// Start 启动 dsh 网页 UI。**幂等**：已在运行或正在启动时直接返回当前状态。
func (m *dshManager) Start() (map[string]any, error) {
	m.mu.Lock()
	if m.state == dshStateRunning && m.run != nil {
		st := m.statusLocked()
		m.mu.Unlock()
		return st, nil
	}
	if m.state == dshStateStarting {
		done := m.startDone
		m.mu.Unlock()
		if done != nil {
			<-done
		}
		return m.Status(), nil
	}

	plan, err := dshLaunchResolver()
	if err != nil {
		m.state = dshLaunchErrorState(err)
		m.last = err.Error()
		st := m.statusLocked()
		m.mu.Unlock()
		return st, err
	}

	done := make(chan struct{})
	m.startDone = done
	m.state = dshStateStarting
	m.plan = plan
	m.last = ""
	m.mu.Unlock()

	run, lerr := m.spawn(plan)
	if lerr != nil && run != nil {
		// spawn 的每条失败路径都已经收掉了进程；这里再兜一次，是为了
		// "以后新增失败分支时忘了回收"不会退化成孤儿。对已死进程是空操作。
		m.killRun(run)
	}

	m.mu.Lock()
	if lerr != nil {
		m.run = nil
		m.state = dshStateFailed
		m.last = lerr.Error()
	} else {
		m.run = run
		m.state = dshStateRunning
		m.last = ""
		m.starts++
	}
	close(done)
	st := m.statusLocked()
	m.mu.Unlock()

	if lerr == nil {
		go m.supervise(run)
		m.installSignalGuard()
	}
	return st, lerr
}

// Stop 优雅停止 dsh。**幂等**：没在跑时直接返回当前状态。
func (m *dshManager) Stop() (map[string]any, error) {
	m.mu.Lock()
	run := m.run
	if run == nil {
		if m.state == dshStateStarting {
			// 启动还没落地：等它结束再停，避免"启动刚成功就变成孤儿"。
			done := m.startDone
			m.mu.Unlock()
			if done != nil {
				<-done
			}
			return m.Stop()
		}
		// 只有"运行态"才需要落到 stopped；node_missing 这类"缺依赖"状态要保留，
		// 否则用户 stop 一下就把"缺什么"的信息擦掉了。
		if m.state == dshStateRunning || m.state == dshStateStopping {
			m.state = dshStateStopped
		}
		st := m.statusLocked()
		m.mu.Unlock()
		return st, nil
	}
	if m.state == dshStateStopping {
		exited := run.exited
		m.mu.Unlock()
		<-exited
		return m.Status(), nil
	}
	m.state = dshStateStopping
	m.mu.Unlock()

	terr := m.terminate(run)

	m.mu.Lock()
	if m.run == run {
		m.run = nil
	}
	m.state = dshStateStopped
	if terr != nil {
		m.last = terr.Error()
	}
	st := m.statusLocked()
	m.mu.Unlock()
	return st, terr
}

// Status 返回结构化状态（可直接 JSON 序列化）。
func (m *dshManager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// statusLocked 拼状态。调用方必须持有 m.mu。
func (m *dshManager) statusLocked() map[string]any {
	running := m.state == dshStateRunning && m.run != nil
	st := map[string]any{
		"object":   "dsh.status",
		"model":    dshModelID,
		"state":    m.state,
		"running":  running,
		"launcher": m.plan.Kind,
		"note":     m.plan.Note,
		"node":     m.node,
		"starts":   m.starts,
		"hint":     dshStateHint(m.state),
		// 如实标注未实现的部分，避免"看到 dsh 就以为能对话"。
		"chat_forwarding": map[string]any{
			"supported": false,
			"reason": "dsh 的对话走它自己的 WebSocket/RPC 通道，网关未实现转发；" +
				"model=dsh 的 /v1/chat/completions 会被明确拒绝（不会静默透传给上游）。",
		},
	}
	if m.run != nil {
		st["pid"] = m.run.pid
		st["host"] = m.run.host
		st["port"] = m.run.port
		st["token"] = m.run.token
		st["url"] = m.run.url
		st["command"] = strings.Join(m.run.plan.Argv, " ")
		st["trusted_hosts"] = m.run.trustedHosts
		st["uptime_seconds"] = int(time.Since(m.run.startedAt).Seconds())
	}
	if m.last != "" {
		st["last_error"] = m.last
	}
	m.logMu.Lock()
	st["log_tail"] = append([]string(nil), m.log...)
	m.logMu.Unlock()
	return st
}

// spawn 起进程并等到"可以访问"（拿到地址，或端口真的在监听）。
//
// 失败时也把 run 一起返回（进程已回收）：调用方据此能确认"没留下孤儿"，
// 测试也正是靠这一点断言超时路径真的收了进程。
func (m *dshManager) spawn(plan dshLaunchPlan) (*dshRun, error) {
	if len(plan.Argv) == 0 {
		return nil, fmt.Errorf("dsh: 启动计划是空的")
	}
	cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
	cmd.Env = append(os.Environ(), plan.Env...)

	// 用 io.Writer 而不是 StdoutPipe：StdoutPipe 要求"读完之后才能 Wait"，
	// 而我们要在等就绪的同时调 Wait（判进程早退）—— 那是有数据竞争的用法。
	// 交给 exec 内部拷贝协程，Wait 会等拷贝结束，顺带把输出收全。
	urlCh := make(chan string, 4)
	cmd.Stdout = &dshLogWriter{m: m, urlCh: urlCh}
	cmd.Stderr = &dshLogWriter{m: m, urlCh: urlCh}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("dsh: 启动失败（%s）: %w", strings.Join(plan.Argv, " "), err)
	}
	// 把子进程收养进 kill-on-close 的 Job Object（Windows；其它平台是空操作）。
	//
	// 为什么必须在 Start() 之后**立刻**做：网关可能在任何时刻被强杀
	//（`Stop-Process -Force` / 任务管理器 / 崩溃走的是 TerminateProcess，
	// 没有任何清理代码有机会运行）。一旦被强杀，`stopForExit()` 就永远不执行，
	// dsh 子进程会变成孤儿 —— 实测发生过。
	//
	// 收养之后，无论网关怎么死，OS 都会在关闭 job 句柄时连带杀掉 dsh。
	//
	// 失败**不阻止启动**：这是加固而非功能本身，因为加固失败就拒绝启动是拿
	// 可用性换整洁。记 WARN 让用户知情，正常清理路径（stopForExit）仍然有效。
	if err := adoptProcessIntoKillJob(cmd.Process); err != nil {
		m.logf("WARN: dsh 未能加入 kill-on-close job（网关被强杀时可能留下 dsh 孤儿进程）: %v", err)
	}
	run := &dshRun{
		cmd: cmd, plan: plan, pid: cmd.Process.Pid,
		startedAt: time.Now(), exited: make(chan struct{}),
		trustedHosts: plan.TrustedHosts,
	}
	// 唯一回收点：cmd.Wait() 必须被调用，否则子进程会变成僵尸。
	go func() {
		run.exitErr = cmd.Wait()
		close(run.exited)
	}()

	if plan.PortFromStdout {
		select {
		case raw := <-urlCh:
			host, port, token, err := splitDSHURL(raw)
			if err != nil {
				m.killRun(run)
				return run, fmt.Errorf("dsh: 解析访问地址失败（%q）: %w", raw, err)
			}
			run.host, run.port, run.token, run.url = host, port, token, raw
		case <-run.exited:
			return run, fmt.Errorf("dsh: 进程在打印访问地址之前就退出了（%s）%s",
				strings.Join(plan.Argv, " "), m.logTailString())
		case <-time.After(dshStartWait):
			m.killRun(run)
			return run, fmt.Errorf("dsh: %v 内没有等到访问地址%s", dshStartWait, m.logTailString())
		}
	} else {
		if err := waitDSHPort(plan.Port, dshStartWait, run.exited); err != nil {
			m.killRun(run)
			return run, fmt.Errorf("dsh: 端口 %d 未就绪: %w%s", plan.Port, err, m.logTailString())
		}
		run.host = dshHost
		run.port = plan.Port
		run.url = fmt.Sprintf("http://%s:%d/", dshHost, plan.Port)
	}

	// 再确认一次进程还活着：dsh 有可能打印了地址又立刻崩掉。
	select {
	case <-run.exited:
		return run, fmt.Errorf("dsh: 打印地址后立即退出%s", m.logTailString())
	default:
	}
	return run, nil
}

// supervise 盯着子进程退出，把状态改回来。
func (m *dshManager) supervise(run *dshRun) {
	<-run.exited
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run != run {
		return // 已经被 Stop 处理过了
	}
	m.run = nil
	if m.state == dshStateStopping {
		m.state = dshStateStopped
		return
	}
	m.state = dshStateStopped
	if run.exitErr != nil {
		m.state = dshStateFailed
		m.last = "dsh 进程意外退出: " + run.exitErr.Error()
	} else {
		m.last = "dsh 进程已退出（退出码 0）"
	}
}

// terminate 先优雅后强杀，并等到进程真的被回收。
func (m *dshManager) terminate(run *dshRun) error {
	select {
	case <-run.exited:
		return nil
	default:
	}
	if err := softKill(run.cmd.Process); err != nil {
		// 平台不支持优雅停止（Windows 没有 SIGTERM）→ 直接强杀，
		// 不白等一个 grace 周期。这不是"跳过优雅"，是"优雅在这里不可用"。
		killNow(run)
	} else {
		select {
		case <-run.exited:
			return nil
		case <-time.After(dshStopGrace):
			killNow(run)
		}
	}
	select {
	case <-run.exited:
		return nil
	case <-time.After(dshStopKillWait):
		return fmt.Errorf("dsh: 进程 %d 在强制结束后仍未回收", run.pid)
	}
}

// stopForExit 是"网关自己正在退出"时的回收路径。
//
// 与 Stop() 的关键区别：**不等优雅窗口**。
//
// 为什么必须这样：网关退出时留给我们的时间不是 5 秒，而是**几十毫秒**
// （cmd/server 收到信号后立刻 httpSrv.Shutdown → ListenAndServe 返回 → 进程退出）。
// 实测就是栽在这里：走完整的 Stop()（先 softKill 再等 grace）时，
// 进程已经退出，回收动作根本没来得及生效，dsh 变成了孤儿。
//
// 所以这条路径只做一件事：用**同步系统调用**把子进程立刻杀掉。
func (m *dshManager) stopForExit() {
	m.mu.Lock()
	run := m.run
	if run == nil || m.state == dshStateStopping {
		m.mu.Unlock()
		return
	}
	m.state = dshStateStopping
	m.mu.Unlock()

	killNow(run)

	m.mu.Lock()
	if m.run == run {
		m.run = nil
	}
	m.state = dshStateStopped
	m.mu.Unlock()
}

// killNow 立刻回收一个子进程：**同步生效 + 尽力杀整棵树**。
//
// 顺序是刻意的，反过来会输在竞态上：
//
//  1. 先把 `taskkill /T /F` **发出去**（Start 而不是 Run）。它是独立进程，
//     即使网关下一毫秒就退出，它也会继续跑完，把 dsh 派生的子进程（语言服务、
//     终端等）一并收掉。Run 会阻塞几十毫秒 —— 那点时间我们往往已经没有。
//  2. 再在**进程内** Kill 直接子进程。这是同步的系统调用，不依赖任何外部进程，
//     是唯一能在"网关马上要死"的窗口里保证生效的动作。
//
// 已知残留风险（如实记录，见 notes.md）：如果网关是被**强杀**的
// （任务管理器结束任务 / job object 回收），这段代码根本不会执行，
// 子进程会变孤儿。要根治需要 Windows Job Object（KILL_ON_JOB_CLOSE）
// 或 Unix 的 Pdeathsig，而这两者都是平台专属 API，必须写在带构建标签的
// 独立文件里 —— 那超出了本角色允许改动的文件范围。
func killNow(run *dshRun) {
	if run == nil {
		return
	}
	if runtime.GOOS == "windows" && run.pid > 0 {
		c := exec.Command("taskkill", "/PID", strconv.Itoa(run.pid), "/T", "/F")
		if err := c.Start(); err == nil {
			// 刻意不 Wait：网关可能马上就没了，让 taskkill 自己跑完。
			// Windows 没有僵尸进程的概念，不需要回收句柄。
			_ = c.Process.Release()
		}
	}
	if run.cmd != nil && run.cmd.Process != nil {
		_ = run.cmd.Process.Kill()
	}
}

// killRun 是启动失败路径上的清理（超时/地址解析失败都要把进程收掉，不留孤儿）。
func (m *dshManager) killRun(run *dshRun) {
	_ = m.terminate(run)
}

// installSignalGuard 在 dsh 运行期间挂一个**附加**的停机信号监听，用于回收子进程。
//
// 为什么需要它：网关正常退出时子进程会变孤儿（Windows 上父进程死了子进程照样活）。
// 这个监听只做"回收子进程"，不改变进程的退出行为 —— cmd/server 自己也有
// signal.NotifyContext，两者互不影响（Go 的信号会分发给所有注册者）。
//
// 一个已知的边角：如果宿主**没有**自己的信号处理，第一个 Ctrl+C 会只停掉 dsh、
// 不杀进程（因为注册了监听就不再走默认处置）。处理完立刻 signal.Stop，
// 第二个 Ctrl+C 恢复正常语义。可用 TRAE2API_DSH_NO_SIGNAL_GUARD=1 关掉这个守卫。
func (m *dshManager) installSignalGuard() {
	if os.Getenv("TRAE2API_DSH_NO_SIGNAL_GUARD") == "1" {
		return
	}
	m.mu.Lock()
	if m.sigCh != nil {
		m.mu.Unlock()
		return
	}
	ch := make(chan os.Signal, 1)
	m.sigCh = ch
	m.mu.Unlock()

	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		// 立刻记录一行：这条日志是"守卫到底有没有被触发"的唯一证据。
		// 实测踩过坑 —— 走完整的 Stop()（含 5 秒 grace）时网关早就退出了，
		// 回收动作根本没生效，而日志里什么都看不出来。
		m.logf("收到停机信号，立刻回收 dsh 子进程…")
		// 先摘掉自己：这样后续信号不会再进这个通道，也不会被这个协程吞掉。
		signal.Stop(ch)
		m.mu.Lock()
		m.sigCh = nil
		m.mu.Unlock()
		// 不等优雅窗口：这里的时间预算只有几十毫秒。
		m.stopForExit()
	}()
}

// logf 统一的日志前缀（与 Server.logf 分开：管理器不依赖 Server）。
func (m *dshManager) logf(format string, args ...any) {
	prefix := "[dsh] "
	fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
}

// appendLog 记录一行子进程输出（只保留尾巴）。
func (m *dshManager) appendLog(line string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	m.log = append(m.log, line)
	if len(m.log) > dshLogTailMax {
		m.log = append([]string(nil), m.log[len(m.log)-dshLogTailMax:]...)
	}
}

// logTailString 把日志尾巴拼成一段错误后缀（启动失败时最有用）。
func (m *dshManager) logTailString() string {
	m.logMu.Lock()
	tail := append([]string(nil), m.log...)
	m.logMu.Unlock()
	if len(tail) == 0 {
		return ""
	}
	return "；最近输出: " + strings.Join(tail, " | ")
}

// dshLogWriter 按行切分子进程输出：收集日志尾巴 + 找访问地址。
type dshLogWriter struct {
	m     *dshManager
	urlCh chan<- string
	buf   []byte
}

// maxLineBuffer 限制"没有换行的超长行"占用（dsh 打进度条时可能没有换行）。
const maxLineBuffer = 64 * 1024

func (w *dshLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		w.observe(line)
	}
	if len(w.buf) > maxLineBuffer {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-maxLineBuffer:]...)
	}
	return len(p), nil
}

func (w *dshLogWriter) observe(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.m.appendLog(line)
	if w.urlCh == nil {
		return
	}
	if u, ok := parseDSHURL(line); ok {
		select {
		case w.urlCh <- u:
		default:
		}
	}
}

// ---- 平台相关的停止语义 ----

// softKill 请求子进程"优雅退出"。
//
// Unix：SIGTERM。Windows：没有 SIGTERM，os.Process.Signal 直接返回
// "not supported by windows"；退而求其次用不带 /F 的 taskkill（向进程窗口
// 发关闭请求）。控制台子进程通常没有窗口，这一步大概率失败 —— 失败就
// 立刻进强杀，不白等 grace。
func softKill(p *os.Process) error {
	if p == nil {
		return errors.New("dsh: 没有进程可停止")
	}
	if runtime.GOOS == "windows" {
		return exec.Command("taskkill", "/PID", strconv.Itoa(p.Pid), "/T").Run()
	}
	return p.Signal(syscall.SIGTERM)
}

// waitDSHPort 等端口真的能连上（PortFromStdout=false 时用）。
func waitDSHPort(port int, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(dshHost, strconv.Itoa(port))
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("进程已退出")
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待 %v 后仍连不上", timeout)
}

// ---- 状态文案 ----

// dshLaunchErrorState 把"启动方式解析失败"映射成状态。
func dshLaunchErrorState(err error) string {
	switch {
	case errors.Is(err, errDSHNodeMissing):
		return dshStateNodeMissing
	case errors.Is(err, errDSHNotFound):
		return dshStateNotFound
	}
	return dshStateFailed
}

// dshStateHint 给每个状态一句"我该做什么"。
//
// 为什么状态里要带这个：用户看到 node_missing 只知道"缺东西"，
// 不知道缺什么、去哪儿装。提示直接写在状态里，前端不必维护一份文案表。
func dshStateHint(state string) string {
	switch state {
	case dshStateNodeMissing:
		return "本机没有 Node.js：dsh 需要它。安装方式（任选其一）：" +
			"① 官网 https://nodejs.org 下载 LTS；" +
			"② Windows 用 nvm-windows（安装后 node 在 C:\\nvm4w\\nodejs\\node.exe）；" +
			"③ 装了 DeepSeek Harness 桌面版的话，它的 dsh 自带运行时，无需单独装 node。" +
			"装完重启网关即可。"
	case dshStateNotFound:
		return "本机没有找到 dsh：装 DeepSeek Harness 桌面版，" +
			"或把 dsh 加进 PATH，或用环境变量 TRAE2API_DSH_CMD 指定完整启动命令" +
			"（命令里可写 {port} 占位符）。"
	case dshStateRunning:
		return "dsh 网页 UI 已就绪：点「内嵌显示」用 iframe 直接操作，" +
			"或点「在新窗口打开」用独立标签页。"
	case dshStateStarting:
		return "正在启动（冷启动可能要十几秒）…"
	case dshStateStopping:
		return "正在停止…"
	case dshStateFailed:
		return "上次启动/运行失败，看 last_error 与 log_tail。"
	}
	return "dsh 未运行。点「启动 dsh」拉起本机的 DeepSeek Harness 网页 UI。"
}

// ---- HTTP 端点 ----

// handleDSHStart POST /admin/dsh/start —— 启动 dsh 网页 UI（幂等）。
func (s *Server) handleDSHStart(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	st, err := s.dshAgent().Start()
	if err != nil {
		st["ok"] = false
		st["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, st)
		return
	}
	st["ok"] = true
	writeJSON(w, http.StatusOK, st)
}

// handleDSHStop POST /admin/dsh/stop —— 优雅停止 dsh（幂等）。
func (s *Server) handleDSHStop(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	st, err := s.dshAgent().Stop()
	if err != nil {
		st["ok"] = false
		st["error"] = err.Error()
		writeJSON(w, http.StatusInternalServerError, st)
		return
	}
	st["ok"] = true
	writeJSON(w, http.StatusOK, st)
}

// handleDSHStatus GET /admin/dsh/status —— 结构化状态。
//
// 恒返 200：状态本身（含"缺 node""启动失败"）是**正常结果**，
// 用 5xx 表达会让控制台把"没装 node"当成接口故障。
func (s *Server) handleDSHStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.dshAgent().Status())
}
