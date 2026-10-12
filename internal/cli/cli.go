// Package cli 提供命令行工具的共享设施：退出码、网关客户端、表格渲染。
//
// 设计约定（对齐 workbuddy2api 的 cmd/* 口径）：
//
//	退出码 0 = 成功
//	退出码 1 = 运行期失败（网络、上游拒绝等）
//	退出码 2 = 用法错误（参数缺失/非法）
//
// 为什么把 2 单独分出来：脚本里 `if cmd; then` 只能区分成败，而
// "参数写错了"和"运行失败了"需要不同的处置（前者重试无用，后者可以重试）。
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
)

// 退出码。
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// UsageError 表示用法错误（调用方应据此返回 ExitUsage）。
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Usagef 构造用法错误。
func Usagef(format string, a ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, a...)}
}

// Fail 打印错误并以 ExitError 退出。
func Fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(ExitError)
}

// UsageFail 打印用法错误并以 ExitUsage 退出。
func UsageFail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "用法错误: "+format+"\n", a...)
	os.Exit(ExitUsage)
}

// RunArgs 与 Run 相同，但**返回**退出码而不是直接退出进程。
//
// 为什么需要它：单一可执行文件里，子命令不该自己 `os.Exit` ——
// 那样会跳过总入口的收尾（如日志刷新、临时文件清理），而且让子命令无法被测试。
// 退出码统一由总入口处理。
func RunArgs(fn func() error) int {
	if err := fn(); err != nil {
		var ue *UsageError
		if errors.As(err, &ue) {
			fmt.Fprintf(os.Stderr, "用法错误: %s\n", ue.Msg)
			return ExitUsage
		}
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// Run 是各命令的统一入口：处理用法错误与退出码。
//
// 返回 error 时按类型决定退出码 —— 这样每个命令的 main 只需一行。
func Run(fn func() error) {
	os.Exit(RunArgs(fn))
}

// ---- 网关客户端 ----

// Client 访问网关的 HTTP API。
type Client struct {
	base string
	key  string
	hc   *http.Client
}

// NewClient 构造客户端。base 形如 http://127.0.0.1:7870。
func NewClient(base, key string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		key:  strings.TrimSpace(key),
		hc:   &http.Client{Timeout: 120 * time.Second},
	}
}

// APIError 是网关返回的结构化错误。
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// do 发一次请求并把响应解析到 out（out 可为 nil）。
func (c *Client) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接网关 %s 失败: %w（网关在运行吗？）", c.base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 网关的错误信封是 OpenAI 风格：{"error":{"code","message"}}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
			return &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
		}
		return &APIError{Status: resp.StatusCode, Message: Truncate(string(raw), 300)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析网关响应失败: %w（原始内容：%s）", err, Truncate(string(raw), 200))
	}
	return nil
}

// Get 发 GET 请求。
func (c *Client) Get(path string, out any) error { return c.do(http.MethodGet, path, nil, out) }

// Post 发 POST 请求。
func (c *Client) Post(path string, body, out any) error {
	return c.do(http.MethodPost, path, body, out)
}

// Delete 发 DELETE 请求。
func (c *Client) Delete(path string, out any) error {
	return c.do(http.MethodDelete, path, nil, out)
}

// Base 返回网关地址。
func (c *Client) Base() string { return c.base }

// ---- 显示宽度（CJK 对齐的关键）----

// RuneWidth 返回一个字符在终端里占的列数。
//
// 为什么要自己算：中文字符占 2 列，而 `len()` 与 `utf8.RuneCountInString`
// 都按 1 算 —— 用它们对齐表格，只要出现中文就会整列错位。
//
// 关于 East Asian Width = Ambiguous 的字符（如 U+2500 ─、U+00A0）：
// 本项目**按 1 列**处理（即与多数终端在西文 locale 下的行为一致），
// 并且表格分隔线只用纯 ASCII（`-`、`|`、`+`）而不用制表符 ——
// 制表符在中文终端里常被渲染成 2 列，会导致分隔线与内容对不上。
func RuneWidth(r rune) int {
	if r == 0 {
		return 0
	}
	// 组合字符（重音等）不占位
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	if isWide(r) {
		return 2
	}
	return 1
}

// isWide 判定是否为宽字符（CJK、全角标点、韩文等）。
//
// ⚠️ **刻意不把 emoji 算作宽字符**。理由：emoji 的终端显示宽度是
// **实现相关**的 —— 有的终端渲染成 2 列，有的 1 列，还有的会因字体回退
// 而宽度不定。我们无法在运行时探测，猜错就会让整张表错位。
//
// 所以本项目在**表格单元格里不使用 emoji**，只用确定宽度的中文（2 列）
// 与 ASCII（1 列）。emoji 只出现在非表格的散文式输出里，那里不需要对齐。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0x303E,   // CJK 部首 + 康熙部首 + CJK 符号
		r >= 0x3041 && r <= 0x33FF,   // 平假名/片假名/注音/CJK 兼容
		r >= 0x3400 && r <= 0x4DBF,   // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK 统一表意文字
		r >= 0xA000 && r <= 0xA4CF,   // 彝文
		r >= 0xAC00 && r <= 0xD7A3,   // 韩文音节
		r >= 0xF900 && r <= 0xFAFF,   // CJK 兼容表意
		r >= 0xFE10 && r <= 0xFE19,   // 竖排标点
		r >= 0xFE30 && r <= 0xFE6F,   // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60,   // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6,   // 全角符号
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B+
		return true
	}
	return false
}

// DisplayWidth 返回字符串在终端里占的列数。
func DisplayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// Pad 按显示宽度右侧补空格（左对齐）。
//
// 若内容已超宽，**不截断**（截断会把中文切半变成乱码），原样返回。
func Pad(s string, width int) string {
	w := DisplayWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// PadLeft 按显示宽度左侧补空格（右对齐，数字列用）。
func PadLeft(s string, width int) string {
	w := DisplayWidth(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

// Truncate 按显示宽度截断并加省略号。
//
// 用于错误信息等"必须截断"的场景；表格内容不要用它。
func Truncate(s string, maxWidth int) string {
	if DisplayWidth(s) <= maxWidth {
		return s
	}
	var sb strings.Builder
	w := 0
	for _, r := range s {
		rw := RuneWidth(r)
		if w+rw > maxWidth-1 {
			break
		}
		sb.WriteRune(r)
		w += rw
	}
	return sb.String() + "…"
}

// ---- 表格 ----

// Table 是纯 ASCII 的表格渲染器。
//
// 分隔线只用 `-` `|` `+`：制表符（U+2500 系）在中文终端里常被当成 2 列宽，
// 与按 1 列算的内容对不上，表现为"表格线错位"。
type Table struct {
	Headers []string
	Rows    [][]string
}

// Add 追加一行。
func (t *Table) Add(cells ...string) { t.Rows = append(t.Rows, cells) }

// Render 渲染成字符串。
func (t *Table) Render() string {
	ncol := len(t.Headers)
	for _, r := range t.Rows {
		if len(r) > ncol {
			ncol = len(r)
		}
	}
	widths := make([]int, ncol)
	for i, h := range t.Headers {
		widths[i] = DisplayWidth(h)
	}
	for _, r := range t.Rows {
		for i, c := range r {
			if w := DisplayWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var sb strings.Builder
	sep := func() {
		sb.WriteByte('+')
		for _, w := range widths {
			sb.WriteString(strings.Repeat("-", w+2))
			sb.WriteByte('+')
		}
		sb.WriteByte('\n')
	}
	row := func(cells []string) {
		sb.WriteByte('|')
		for i := 0; i < ncol; i++ {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			sb.WriteByte(' ')
			sb.WriteString(Pad(c, widths[i]))
			sb.WriteString(" |")
		}
		sb.WriteByte('\n')
	}

	sep()
	if len(t.Headers) > 0 {
		row(t.Headers)
		sep()
	}
	for _, r := range t.Rows {
		row(r)
	}
	sep()
	return sb.String()
}

// ---- 其它 ----

// JSON 以缩进 JSON 打印到 stdout。
func JSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		Fail("序列化失败: %v", err)
	}
	fmt.Println(string(b))
}

// Bool 把布尔渲染成便于阅读的符号。
func Bool(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// Duration 把秒数渲染成人话。
func Duration(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh%dm", sec/3600, (sec%3600)/60)
	}
}

// GatewayFlags 注册所有命令都支持的网关连接参数。
//
// 默认值来自环境变量，这样脚本里可以 `export TRAE2API_URL=...` 一次配好。
func GatewayFlags(fs *flag.FlagSet) (base, key *string) {
	def := os.Getenv("TRAE2API_URL")
	if def == "" {
		def = "http://127.0.0.1:7870"
	}
	base = fs.String("url", def, "网关地址（或环境变量 TRAE2API_URL）")
	key = fs.String("key", os.Getenv("TRAE2API_KEY"), "API Key（或环境变量 TRAE2API_KEY）")
	return
}
