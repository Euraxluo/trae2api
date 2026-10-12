package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trae2api/internal/config"
	"trae2api/internal/docparse"
)

// newDocServer 构造一个启用了附件展开的 Server（根目录为临时目录）。
func newDocServer(t *testing.T, root string) *Server {
	t.Helper()
	return &Server{
		cfg:  &config.Config{},
		docs: docparse.New(docparse.Options{Enabled: true, AllowedRoots: []string{root}}),
	}
}

func expand(t *testing.T, s *Server, body string) (string, docparse.Stats) {
	t.Helper()
	nb, st := s.expandFileRefs([]byte(body))
	return string(nb), st
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExpandExplicitFilePart 显式 file 片段展开成文本。
func TestExpandExplicitFilePart(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	p := writeFile(t, root, "main.go", "package main\n")
	s := newDocServer(t, root)

	body := bodyWithFilePart(p)
	out, st := expand(t, s, body)

	if st.Files != 1 {
		t.Fatalf("应展开 1 个文件，实际 %d（跳过：%v）", st.Files, st.Skipped)
	}
	if !strings.Contains(out, "package main") {
		t.Errorf("文件内容未注入: %s", out)
	}
	if !strings.Contains(out, "<file ") {
		t.Error("应带来源标注")
	}
	// 原始 file 片段不应残留
	if strings.Contains(out, `"type":"file"`) {
		t.Error("原始 file 片段应被替换掉")
	}
	// 结构必须仍是合法 JSON
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
}

// TestExpandAtRefInString 文本里的 @路径 展开。
func TestExpandAtRefInString(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	p := writeFile(t, root, "a.md", "# 标题")
	s := newDocServer(t, root)

	body := bodyWithText("请读 @" + p + " 然后总结")
	out, st := expand(t, s, body)

	if st.Files != 1 {
		t.Fatalf("应展开 1 个文件，实际 %d（跳过：%v）", st.Files, st.Skipped)
	}
	if !strings.Contains(out, "# 标题") {
		t.Errorf("内容未注入: %s", out)
	}
}

// bodyWithText 构造一个单条 user 文本消息的请求体。
//
// 用 json.Marshal 而不是手拼字符串：手拼时路径里的引号/反斜杠
// （Windows 路径尤其）会把 JSON 拼坏。
func bodyWithText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": text}},
	})
	return string(b)
}

// bodyWithFilePart 构造一个带显式 file 片段的请求体。
func bodyWithFilePart(path string) string {
	b, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "看看这个"},
				map[string]any{"type": "file", "file": map[string]any{"path": path}},
			},
		}},
	})
	return string(b)
}

// TestAtRefUnresolvableKeptAsIs 解析失败的 @引用必须**原样保留**。
//
// 为什么：`@` 在自然语言里也可能是邮箱或提及。解析不到就保留文本，
// 既方便又不会误伤，也不会泄露任何东西。
func TestAtRefUnresolvableKeptAsIs(t *testing.T) {
	root := t.TempDir()
	s := newDocServer(t, root)

	body := `{"messages":[{"role":"user","content":"联系 me@example.com 或 @notafile 谢谢"}]}`
	out, st := expand(t, s, body)

	if st.Files != 0 {
		t.Errorf("不应展开任何文件，实际 %d", st.Files)
	}
	if !strings.Contains(out, "me@example.com") || !strings.Contains(out, "@notafile") {
		t.Errorf("无法解析的 @ 引用应原样保留: %s", out)
	}
}

// TestExpandOutsideRootBecomesPlaceholder 越界文件变成占位文本 + 记进 Skipped，
// 但**请求本身不能失败**。
func TestExpandOutsideRootBecomesPlaceholder(t *testing.T) {
	root := t.TempDir()
	s := newDocServer(t, root)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("TOP SECRET"), 0o600)

	body := bodyWithFilePart(outside)
	out, st := expand(t, s, body)

	if st.Files != 0 {
		t.Fatal("越界文件不应被读取")
	}
	if len(st.Skipped) == 0 {
		t.Error("应记录跳过原因")
	}
	if strings.Contains(out, "TOP SECRET") {
		t.Fatal("泄露了根目录外的文件内容！")
	}
	if !strings.Contains(out, "附件读取失败") {
		t.Errorf("应有占位文本让用户知道附件没生效: %s", out)
	}
	// 仍是合法 JSON（请求不会因附件问题而 400）
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
}

// TestDisabledExpanderIsNoop 未启用时逐字节不动请求体。
func TestDisabledExpanderIsNoop(t *testing.T) {
	s := &Server{cfg: &config.Config{}, docs: docparse.New(docparse.DefaultOptions())}
	body := `{"messages":[{"role":"user","content":"@/etc/passwd"}]}`
	out, st := expand(t, s, body)
	if out != body {
		t.Errorf("未启用时不该改动请求体:\n%s\n%s", body, out)
	}
	if st.Files != 0 {
		t.Error("未启用时不该展开")
	}
}

// TestNonJSONBodyUnchanged 非 JSON body 原样返回。
func TestNonJSONBodyUnchanged(t *testing.T) {
	root := t.TempDir()
	s := newDocServer(t, root)
	body := "not json"
	out, _ := expand(t, s, body)
	if out != body {
		t.Errorf("非 JSON 不该被改动: %s", out)
	}
}

// TestExpandImagePart 图片引用展开成 image_url 片段。
func TestExpandImagePart(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	p := writeFile(t, root, "x.png", string(png))
	s := newDocServer(t, root)

	body := bodyWithFilePart(p)
	out, st := expand(t, s, body)

	if st.Images != 1 {
		t.Fatalf("应展开 1 张图片，实际 %d（跳过：%v）", st.Images, st.Skipped)
	}
	if !strings.Contains(out, `"type":"image_url"`) || !strings.Contains(out, "data:image/png;base64,") {
		t.Errorf("图片应转成 image_url data URL: %s", out)
	}
}

// TestAtRefImageExplained 文本里的图片 @引用应给出可操作的说明而不是静默丢弃。
func TestAtRefImageExplained(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	p := writeFile(t, root, "x.png", "\x89PNG")
	s := newDocServer(t, root)

	body := bodyWithText("看 @" + p)
	_, st := expand(t, s, body)

	found := false
	for _, sk := range st.Skipped {
		if strings.Contains(sk, "图片") {
			found = true
		}
	}
	if !found {
		t.Errorf("应说明图片需要用片段形式引用: %v", st.Skipped)
	}
}

// TestExpandPreservesOtherFields 展开不能丢请求体的其他字段。
func TestExpandPreservesOtherFields(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	p := writeFile(t, root, "a.txt", "hi")
	s := newDocServer(t, root)

	body := `{"model":"trae:glm-5.2","temperature":0.5,"max_tokens":99,
		"messages":[{"role":"user","content":[{"type":"file","file":{"path":` + mustJSONString(p) + `}}]}]}`
	out, st := expand(t, s, body)
	if st.Files != 1 {
		t.Fatalf("展开失败: %v", st.Skipped)
	}
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	if m["model"] != "trae:glm-5.2" || m["temperature"] != 0.5 || m["max_tokens"] != float64(99) {
		t.Errorf("字段丢失或被改动: %v", m)
	}
}

// mustJSONString 把字符串序列化成 JSON 字面量（含引号）。
func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
