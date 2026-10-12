//go:build windows

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOptionsDefaults 默认值填充。
func TestOptionsDefaults(t *testing.T) {
	raw := Options{}
	o := raw.withDefaults()
	if o.Width <= 0 || o.Height <= 0 {
		t.Errorf("尺寸未填默认: %+v", o)
	}
	if o.Title == "" {
		t.Error("标题未填默认")
	}
	// 已给的不能被覆盖
	raw2 := Options{Width: 100, Height: 200, Title: "X"}
	o2 := raw2.withDefaults()
	if o2.Width != 100 || o2.Height != 200 || o2.Title != "X" {
		t.Errorf("已有值被覆盖: %+v", o2)
	}
}

// TestLoaderCandidatesIncludeExeDir 候选路径必须**优先**是 exe 同目录。
//
// 这是用户唯一能自己控制的位置：把 WebView2Loader.dll 拷到 exe 旁边即可启用内嵌窗口。
func TestLoaderCandidatesIncludeExeDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("无法取得 exe 路径")
	}
	want := filepath.Join(filepath.Dir(exe), "WebView2Loader.dll")
	found := false
	for _, c := range webview2LoaderCandidates() {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("候选路径里没有 exe 同目录的 %s；实际：%v", want, webview2LoaderCandidates())
	}
}

// TestLoaderEnvOverride TRAE2API_WEBVIEW2_LOADER 环境变量应被优先采用。
func TestLoaderEnvOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "MyLoader.dll")
	t.Setenv("TRAE2API_WEBVIEW2_LOADER", custom)
	cands := webview2LoaderCandidates()
	if len(cands) == 0 || cands[0] != custom {
		t.Errorf("环境变量指定的路径应排在首位，实际：%v", cands)
	}
}

// TestLoadWebView2ErrorIsActionable 找不到 DLL 时错误信息要**可操作**。
//
// 不能只说"加载失败" —— 用户需要知道该把文件放哪儿。
func TestLoadWebView2ErrorIsActionable(t *testing.T) {
	// 指向一个确定不存在的路径，并清掉系统搜索路径的命中可能
	t.Setenv("TRAE2API_WEBVIEW2_LOADER", filepath.Join(t.TempDir(), "nope.dll"))
	// 让 exe 同目录的候选也落空：无法改 exe 目录，所以只验证错误文案
	_, err := loadWebView2()
	if err == nil {
		t.Skip("本机居然能找到 WebView2Loader.dll，跳过文案检查")
	}
	msg := err.Error()
	if !strings.Contains(msg, "WebView2Loader.dll") {
		t.Errorf("错误信息应点明缺哪个文件: %v", err)
	}
	if !strings.Contains(msg, "已尝试") {
		t.Errorf("错误信息应列出尝试过的位置: %v", err)
	}
}

// TestFindBrowserFindsEdge 本机应能找到 Edge（Windows 10/11 自带）。
func TestFindBrowserFindsEdge(t *testing.T) {
	p := findBrowser()
	if p == "" {
		t.Skip("本机未找到 Edge/Chrome（无头 CI 环境常见）")
	}
	if !strings.HasSuffix(strings.ToLower(p), ".exe") {
		t.Errorf("返回的不是可执行文件路径: %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("返回的路径不存在: %q", p)
	}
	t.Logf("找到浏览器: %s", p)
}

// TestAvailableReportsMode Available 必须给出模式与原因，且不实际开窗。
func TestAvailableReportsMode(t *testing.T) {
	mode, note := Available()
	switch mode {
	case ModeWebView2:
		if note != "" {
			t.Logf("WebView2 可用，附带说明: %s", note)
		}
	case ModeBrowserApp, ModeNone:
		if note == "" {
			t.Error("降级时必须给出原因，否则用户不知道该怎么办")
		}
		t.Logf("模式=%s 原因=%s", mode, note)
	default:
		t.Errorf("未知模式 %q", mode)
	}
}

// TestLocalURLNotUsedHere 只是提醒：本包的 Run 会阻塞，不适合在单元测试里调用。
// 真实启动验证放在 run/ 下的端到端脚本里做。
func TestRunIsBlocking(t *testing.T) {
	t.Skip("Run 会阻塞并创建真实窗口，由端到端脚本验证")
}
