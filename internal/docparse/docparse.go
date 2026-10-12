// Package docparse 把客户端消息里的**文件引用**展开成上游能理解的内容片段。
//
// 定位：很多 OpenAI 兼容客户端只会发纯文本消息，不能像 IDE 那样把工作区文件
// 附进上下文。本包让网关侧承担这一步 —— 客户端只要写一个路径引用，
// 网关读文件、判类型、转成文本片段或图片片段。
//
// ⚠️ 安全前提（这是本包最重要的一段）：
//
//	从 API 请求里读**任意路径**等于把服务器文件系统开放给调用方 ——
//	一个 `@/etc/shadow` 或 `@~/.ssh/id_rsa` 就能把私钥读走，
//	而且会跟着请求发到上游模型。所以：
//	  1. 必须显式配置 allowed_roots；**默认空 = 功能关闭**（fail-closed）。
//	  2. 每个路径都要 ResolveSymlink 后做**前缀**校验，防 `../../` 与符号链接逃逸。
//	  3. 单文件与总大小都有上限，防止一次请求把内存打满。
//	  4. 二进制文件直接拒绝（不做"猜编码"），避免把乱码塞进上下文。
package docparse

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Options 是展开器的配置。
type Options struct {
	// Enabled 是否启用。默认 false。
	Enabled bool
	// AllowedRoots 允许读取的根目录（**绝对路径**）。空 = 拒绝一切。
	//
	// 这是唯一的安全边界，不能省。用户想读哪儿就配哪儿。
	AllowedRoots []string
	// MaxFileBytes 单文件上限，默认 1 MiB。
	MaxFileBytes int64
	// MaxTotalBytes 单次请求累计上限，默认 4 MiB。
	MaxTotalBytes int64
	// MaxFiles 单次请求文件数上限，默认 16。
	MaxFiles int
}

// DefaultOptions 返回安全的默认值（**关闭**）。
func DefaultOptions() Options {
	return Options{
		Enabled:       false,
		MaxFileBytes:  1 << 20,
		MaxTotalBytes: 4 << 20,
		MaxFiles:      16,
	}
}

func (o Options) withDefaults() Options {
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = 1 << 20
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = 4 << 20
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 16
	}
	return o
}

// Expander 按配置展开文件引用。并发安全（无可变状态）。
type Expander struct {
	opt Options
	// roots 是规范化后的绝对根目录（含尾部分隔符），便于前缀比较。
	roots []string
}

// New 构造展开器。
//
// 若 Enabled 为 true 但 AllowedRoots 为空，**强制关闭** —— 那是配置错误，
// 而"配错了就开放整个文件系统"是最糟的失败模式（fail-closed 而非 fail-open）。
func New(opt Options) *Expander {
	opt = opt.withDefaults()
	e := &Expander{opt: opt}
	for _, r := range opt.AllowedRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		// 解析符号链接：否则用户把 root 设成一个指向 / 的软链，
		// 前缀校验就会被绕过
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		e.roots = append(e.roots, abs)
	}
	if e.opt.Enabled && len(e.roots) == 0 {
		e.opt.Enabled = false
	}
	return e
}

// Enabled 报告是否真的会展开（配置合法且启用）。
func (e *Expander) Enabled() bool { return e != nil && e.opt.Enabled }

// MaxTotalBytes 返回单次请求的累计字节上限。
func (e *Expander) MaxTotalBytes() int64 {
	if e == nil {
		return 0
	}
	return e.opt.MaxTotalBytes
}

// MaxFiles 返回单次请求的文件数上限。
func (e *Expander) MaxFiles() int {
	if e == nil {
		return 0
	}
	return e.opt.MaxFiles
}

// Roots 返回生效的根目录（诊断用）。
func (e *Expander) Roots() []string {
	if e == nil {
		return nil
	}
	return append([]string(nil), e.roots...)
}

// Part 是一个已展开的内容片段。
type Part struct {
	// Type "text" 或 "image"。
	Type string
	// Text Type=="text" 时的内容（已带来源标注）。
	Text string
	// MimeType 图片的 MIME 类型。
	MimeType string
	// Base64 图片的 base64 数据。
	Base64 string
	// Source 原始引用（诊断用）。
	Source string
}

// Stats 是一次展开的统计（供日志与前端回显）。
type Stats struct {
	Files     int      `json:"files"`
	Bytes     int64    `json:"bytes"`
	Images    int      `json:"images"`
	Skipped   []string `json:"skipped,omitempty"`
	Truncated []string `json:"truncated,omitempty"`
}

// 文本类扩展名白名单（按"确定是文本"来收录，不做二进制猜测）。
var textExts = map[string]string{
	".txt": "text", ".md": "markdown", ".markdown": "markdown",
	".json": "json", ".jsonc": "json", ".yaml": "yaml", ".yml": "yaml",
	".toml": "toml", ".ini": "ini", ".csv": "csv", ".tsv": "tsv",
	".xml": "xml", ".html": "html", ".htm": "html", ".css": "css",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "tsx", ".jsx": "jsx",
	".go": "go", ".py": "python", ".rb": "ruby", ".rs": "rust",
	".java": "java", ".kt": "kotlin", ".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp", ".cs": "csharp",
	".php": "php", ".swift": "swift", ".scala": "scala",
	".sh": "shell", ".bash": "shell", ".zsh": "shell",
	".ps1": "powershell", ".cmd": "batch", ".bat": "batch",
	".sql": "sql", ".graphql": "graphql", ".proto": "protobuf",
	".dockerfile": "dockerfile", ".gitignore": "text", ".env": "text",
	".log": "text", ".conf": "text", ".cfg": "text", ".properties": "text",
	".vue": "vue", ".svelte": "svelte", ".lua": "lua", ".r": "r",
	".dart": "dart", ".ex": "elixir", ".exs": "elixir", ".erl": "erlang",
	".clj": "clojure", ".hs": "haskell", ".ml": "ocaml", ".zig": "zig",
	".nim": "nim", ".jl": "julia", ".pl": "perl", ".groovy": "groovy",
}

// 无扩展名但常见的文本文件名。
var textNames = map[string]bool{
	"dockerfile": true, "makefile": true, "license": true, "readme": true,
	"changelog": true, "authors": true, "notice": true, "go.mod": true,
	"go.sum": true, "requirements.txt": true, "procfile": true,
}

// 图片扩展名 → MIME。
var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
}

// Resolve 校验并解析一个路径引用。
//
// 返回绝对路径；任何越界、不存在、非普通文件的情况都返回错误。
func (e *Expander) Resolve(ref string) (string, error) {
	if e == nil || !e.opt.Enabled {
		return "", fmt.Errorf("docparse: 文件解析未启用")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("docparse: 空路径")
	}
	// 去掉 file:// 前缀与包裹的引号/反引号（客户端写法五花八门）
	ref = strings.TrimPrefix(ref, "file://")
	ref = strings.Trim(ref, "`'\"")
	if ref == "" {
		return "", fmt.Errorf("docparse: 空路径")
	}

	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", fmt.Errorf("docparse: 解析路径 %q: %w", ref, err)
	}
	// 先解析符号链接再校验：否则 `allowed/link -> /etc` 就能读到 /etc。
	// 注意 EvalSymlinks 要求路径**存在**，这本身也是一道校验。
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("docparse: 路径不存在或无法解析 %q", ref)
	}
	if !e.withinRoots(real) {
		return "", fmt.Errorf("docparse: 路径 %q 不在允许的根目录内", ref)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("docparse: 无法读取 %q", ref)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("docparse: %q 是目录（不支持整目录展开）", ref)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("docparse: %q 不是普通文件", ref)
	}
	return real, nil
}

// withinRoots 判断路径是否落在某个允许的根目录下。
//
// 用 filepath.Rel 而不是字符串前缀：字符串前缀会把 `/data/allowed-evil`
// 误判为落在 `/data/allowed` 内。
func (e *Expander) withinRoots(p string) bool {
	for _, root := range e.roots {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}

// Load 读取并转换单个文件引用。
//
// budget 是剩余总字节预算；返回实际消耗的字节数。
func (e *Expander) Load(ref string, budget int64) (Part, int64, error) {
	path, err := e.Resolve(ref)
	if err != nil {
		return Part{}, 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Part{}, 0, err
	}
	if fi.Size() > e.opt.MaxFileBytes {
		return Part{}, 0, fmt.Errorf("docparse: %q 超过单文件上限（%d > %d 字节）",
			ref, fi.Size(), e.opt.MaxFileBytes)
	}
	if fi.Size() > budget {
		return Part{}, 0, fmt.Errorf("docparse: %q 超过本次请求的累计上限", ref)
	}

	ext := strings.ToLower(filepath.Ext(path))
	name := strings.ToLower(filepath.Base(path))

	// 图片 → base64 片段
	if mimeType, ok := imageExts[ext]; ok {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Part{}, 0, fmt.Errorf("docparse: 读取 %q: %w", ref, err)
		}
		return Part{
			Type: "image", MimeType: mimeType,
			Base64: base64.StdEncoding.EncodeToString(raw),
			Source: ref,
		}, int64(len(raw)), nil
	}

	// PDF → 自己解析出文本（零第三方依赖，实现在 pdf.go）。
	//
	// 注意顺序：PDF 是二进制格式，不能走下面的"UTF-8 文本"分支
	//（会被 NUL 检查拒掉），所以必须在文本白名单之前分派。
	if ext == ".pdf" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Part{}, 0, fmt.Errorf("docparse: 读取 %q: %w", ref, err)
		}
		text, err := ExtractPDFText(raw)
		if err != nil {
			// **不返回空文本**：空文本会让用户以为"文件是空的"，
			// 而真实原因往往是"扫描件/加密/不支持的编码"。
			return Part{}, 0, fmt.Errorf("docparse: 解析 PDF %q: %w", ref, err)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "<file path=%q language=\"pdf\">\n", ref)
		sb.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("</file>")
		return Part{Type: "text", Text: sb.String(), Source: ref}, int64(len(raw)), nil
	}

	// 文本类
	lang, isText := textExts[ext]
	if !isText && textNames[name] {
		isText, lang = true, "text"
	}
	if !isText {
		return Part{}, 0, fmt.Errorf("docparse: 不支持的文件类型 %q（只支持文本类与图片，"+
			"不做二进制猜测）", ext)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Part{}, 0, fmt.Errorf("docparse: 读取 %q: %w", ref, err)
	}
	// 二进制检查：即使扩展名像文本，内容也可能是二进制（如被改名的 exe）。
	// 用"是否合法 UTF-8 且无 NUL"判定 —— 拒绝比猜编码安全。
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return Part{}, 0, fmt.Errorf("docparse: %q 内容不是 UTF-8 文本（已拒绝）", ref)
	}

	text := string(raw)
	truncated := false
	// 再按字节截一次：上面的检查用的是 fi.Size()，但文件可能在
	// stat 与 read 之间变大（TOCTOU），所以以实际读到的长度为准。
	if int64(len(raw)) > e.opt.MaxFileBytes {
		text = string(raw[:e.opt.MaxFileBytes])
		truncated = true
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<file path=%q language=%q>\n", ref, lang)
	sb.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		sb.WriteString("\n")
	}
	if truncated {
		sb.WriteString("…（文件过大，已截断）\n")
	}
	sb.WriteString("</file>")

	return Part{Type: "text", Text: sb.String(), Source: ref}, int64(len(raw)), nil
}
