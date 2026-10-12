package docparse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setup 建一个允许读取的根目录，里面放几个测试文件。
func setup(t *testing.T) (root string, e *Expander) {
	t.Helper()
	root = t.TempDir()
	// 解析一次：macOS 上 /var 是指向 /private/var 的软链，
	// 不解析会让"合法路径被判越界"的假失败
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\nfunc main(){}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "b.md"), []byte("# 标题\n内容"), 0o600)
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0xff, 0xfe}, 0o600)
	os.WriteFile(filepath.Join(root, "fake.txt"), []byte{0x00, 0x01, 0x02}, 0o600) // 扩展名像文本但内容是二进制
	// 1x1 PNG
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	os.WriteFile(filepath.Join(root, "pic.png"), png, 0o600)
	os.MkdirAll(filepath.Join(root, "sub"), 0o700)
	os.WriteFile(filepath.Join(root, "sub", "c.txt"), []byte("nested"), 0o600)

	e = New(Options{Enabled: true, AllowedRoots: []string{root}})
	return root, e
}

// TestDisabledByDefault 默认必须关闭（fail-closed）。
func TestDisabledByDefault(t *testing.T) {
	e := New(DefaultOptions())
	if e.Enabled() {
		t.Fatal("默认不应启用")
	}
	if _, err := e.Resolve("any/path"); err == nil {
		t.Error("未启用时应拒绝一切路径")
	}
}

// TestEnabledWithoutRootsIsClosed 启用但没配根目录 → **强制关闭**。
//
// 这是最重要的一条安全语义：配置写漏了必须变成"功能不可用"，
// 而不是"开放整个文件系统"（fail-closed，不是 fail-open）。
func TestEnabledWithoutRootsIsClosed(t *testing.T) {
	e := New(Options{Enabled: true}) // 故意不配 AllowedRoots
	if e.Enabled() {
		t.Fatal("启用但无根目录时必须强制关闭")
	}
	if _, err := e.Resolve("/etc/passwd"); err == nil {
		t.Error("无根目录时不应能解析任何路径")
	}
	if len(e.Roots()) != 0 {
		t.Errorf("不应有生效的根目录: %v", e.Roots())
	}
}

// TestResolveInsideRoot 根目录内的文件可解析。
//
// 相对路径按**进程 CWD** 解析（不是按根目录）—— 多根目录时"相对哪个根"有歧义，
// 所以采用标准语义：先转绝对路径，再校验是否落在某个根内。
func TestResolveInsideRoot(t *testing.T) {
	root, e := setup(t)
	got, err := e.Resolve(filepath.Join(root, "a.go"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasSuffix(got, "a.go") {
		t.Errorf("解析结果不符: %q", got)
	}

	// 相对路径：切到根目录后再用相对写法
	oldwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Skipf("无法切换工作目录: %v", err)
	}
	defer os.Chdir(oldwd)

	got2, err := e.Resolve("a.go")
	if err != nil {
		t.Fatalf("相对路径 Resolve: %v", err)
	}
	if got2 != got {
		t.Errorf("相对与绝对路径解析结果不一致: %q vs %q", got2, got)
	}
}

// TestRelativePathOutsideRootRejected CWD 在根外时，相对路径也要被拒。
func TestRelativePathOutsideRootRejected(t *testing.T) {
	_, e := setup(t) // CWD 是包目录，不在临时根内
	if _, err := e.Resolve("docparse.go"); err == nil {
		t.Error("CWD 不在允许根内时，相对路径应被拒绝")
	}
}

// TestRejectOutsideRoot 根目录外必须拒绝。
func TestRejectOutsideRoot(t *testing.T) {
	root, e := setup(t)
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	defer os.Remove(outside)

	if _, err := e.Resolve(outside); err == nil {
		t.Error("根目录外的文件应被拒绝")
	}
}

// TestRejectTraversal 用 `..` 穿越必须被拒绝。
func TestRejectTraversal(t *testing.T) {
	root, e := setup(t)
	// 构造一个能穿越出去的路径
	esc := filepath.Join(root, "..", "..", "..", "..", "windows", "win.ini")
	if runtime.GOOS != "windows" {
		esc = filepath.Join(root, "..", "..", "..", "..", "etc", "passwd")
	}
	if _, err := e.Resolve(esc); err == nil {
		t.Errorf("穿越路径 %q 应被拒绝", esc)
	}
	// 更直接的写法
	if _, err := e.Resolve(filepath.Join(root, "..", "..")); err == nil {
		t.Error(".. 穿越应被拒绝")
	}
}

// TestRejectSymlinkEscape 符号链接逃逸必须被拒绝。
//
// 这是最容易漏的一条：只看字符串前缀的实现会放行
// `allowed/link -> /etc/passwd`。所以必须先 EvalSymlinks 再校验。
func TestRejectSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要管理员权限或开发者模式")
	}
	root, e := setup(t)
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	os.WriteFile(secret, []byte("top secret"), 0o600)
	defer os.Remove(secret)

	link := filepath.Join(root, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if _, err := e.Resolve(link); err == nil {
		t.Error("指向根目录外的符号链接必须被拒绝（前缀校验不够，必须先解析软链）")
	}

	// 目录软链同理
	dirLink := filepath.Join(root, "dirlink")
	if err := os.Symlink(filepath.Dir(root), dirLink); err == nil {
		if _, err := e.Resolve(filepath.Join(dirLink, "secret.txt")); err == nil {
			t.Error("经由目录软链的逃逸必须被拒绝")
		}
	}
}

// TestSiblingPrefixNotConfused 前缀相似的兄弟目录不能被误判为在根内。
//
// `/data/allowed` 与 `/data/allowed-evil`：字符串前缀会误判，filepath.Rel 不会。
func TestSiblingPrefixNotConfused(t *testing.T) {
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	allowed := filepath.Join(base, "allowed")
	evil := filepath.Join(base, "allowed-evil")
	os.MkdirAll(allowed, 0o700)
	os.MkdirAll(evil, 0o700)
	os.WriteFile(filepath.Join(evil, "x.txt"), []byte("evil"), 0o600)

	e := New(Options{Enabled: true, AllowedRoots: []string{allowed}})
	if _, err := e.Resolve(filepath.Join(evil, "x.txt")); err == nil {
		t.Error("前缀相似的兄弟目录不应被当作在根内")
	}
}

// TestRejectDirectory 目录应被拒绝（不支持整目录展开）。
func TestRejectDirectory(t *testing.T) {
	root, e := setup(t)
	if _, err := e.Resolve(filepath.Join(root, "sub")); err == nil {
		t.Error("目录应被拒绝")
	}
}

// TestLoadText 文本文件展开成带来源标注的文本片段。
func TestLoadText(t *testing.T) {
	root, e := setup(t)
	p, n, err := e.Load(filepath.Join(root, "a.go"), 1<<20)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Type != "text" {
		t.Fatalf("类型 = %q want text", p.Type)
	}
	if !strings.Contains(p.Text, "<file ") || !strings.Contains(p.Text, "</file>") {
		t.Errorf("应带来源标注: %q", p.Text)
	}
	if !strings.Contains(p.Text, "language=\"go\"") {
		t.Errorf("应标注语言: %q", p.Text)
	}
	if !strings.Contains(p.Text, "func main(){}") {
		t.Error("文件内容缺失")
	}
	if n <= 0 {
		t.Error("应返回消耗的字节数")
	}
}

// TestLoadImage 图片展开成 base64 片段。
func TestLoadImage(t *testing.T) {
	root, e := setup(t)
	p, _, err := e.Load(filepath.Join(root, "pic.png"), 1<<20)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Type != "image" || p.MimeType != "image/png" {
		t.Fatalf("图片片段不符: %+v", p)
	}
	if p.Base64 == "" {
		t.Error("base64 数据为空")
	}
}

// TestRejectUnsupportedExt 不支持的扩展名应拒绝（不做二进制猜测）。
func TestRejectUnsupportedExt(t *testing.T) {
	root, e := setup(t)
	if _, _, err := e.Load(filepath.Join(root, "bin.dat"), 1<<20); err == nil {
		t.Error("未知扩展名应被拒绝")
	}
}

// TestRejectBinaryDisguisedAsText 扩展名像文本但内容是二进制 → 拒绝。
//
// 这是"不做编码猜测"的具体体现：把乱码塞进上下文会浪费 token 且误导模型。
func TestRejectBinaryDisguisedAsText(t *testing.T) {
	root, e := setup(t)
	if _, _, err := e.Load(filepath.Join(root, "fake.txt"), 1<<20); err == nil {
		t.Error("内容为二进制时应被拒绝，即使扩展名是 .txt")
	}
}

// TestFileSizeLimit 单文件上限。
func TestFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	big := strings.Repeat("x", 5000)
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o600)

	e := New(Options{Enabled: true, AllowedRoots: []string{root}, MaxFileBytes: 1000})
	if _, _, err := e.Load(filepath.Join(root, "big.txt"), 1<<20); err == nil {
		t.Error("超过单文件上限应被拒绝")
	}
}

// TestBudgetLimit 累计预算上限。
func TestBudgetLimit(t *testing.T) {
	root, e := setup(t)
	// 预算只剩 1 字节 → 拒绝
	if _, _, err := e.Load(filepath.Join(root, "a.go"), 1); err == nil {
		t.Error("超过累计预算应被拒绝")
	}
}

// TestRefNormalization 各种客户端写法都要能认。
func TestRefNormalization(t *testing.T) {
	root, e := setup(t)
	target := filepath.Join(root, "a.go")
	for _, ref := range []string{
		target,
		"`" + target + "`",
		`"` + target + `"`,
		"'" + target + "'",
		"file://" + target,
		"  " + target + "  ",
	} {
		if _, err := e.Resolve(ref); err != nil {
			t.Errorf("引用 %q 应能解析: %v", ref, err)
		}
	}
}

// TestEmptyRef 空引用应报错而不是 panic。
func TestEmptyRef(t *testing.T) {
	_, e := setup(t)
	for _, ref := range []string{"", "   ", "``", `""`, "file://"} {
		if _, err := e.Resolve(ref); err == nil {
			t.Errorf("空引用 %q 应报错", ref)
		}
	}
}

// TestRootItselfIsAllowed 根目录自身（rel == "."）的判定不应误伤子文件。
func TestRootItselfIsAllowed(t *testing.T) {
	root, e := setup(t)
	if !e.withinRoots(root) {
		t.Error("根目录自身应在范围内")
	}
	if !e.withinRoots(filepath.Join(root, "a.go")) {
		t.Error("根目录下的文件应在范围内")
	}
	if e.withinRoots(filepath.Dir(root)) {
		t.Error("根目录的父目录不应在范围内")
	}
}

// TestNilExpanderSafe nil 展开器不应 panic。
func TestNilExpanderSafe(t *testing.T) {
	var e *Expander
	if e.Enabled() {
		t.Error("nil 不应报告启用")
	}
	if e.Roots() != nil {
		t.Error("nil 的 Roots 应为 nil")
	}
	if _, err := e.Resolve("x"); err == nil {
		t.Error("nil 应报错")
	}
	if _, _, err := e.Load("x", 1); err == nil {
		t.Error("nil 的 Load 应报错")
	}
}

// TestDefaultsFilled 零值配置应被填上安全默认。
func TestDefaultsFilled(t *testing.T) {
	o := Options{}.withDefaults()
	if o.MaxFileBytes <= 0 || o.MaxTotalBytes <= 0 || o.MaxFiles <= 0 {
		t.Errorf("默认值未填: %+v", o)
	}
	if o.Enabled {
		t.Error("零值配置不应启用")
	}
}
