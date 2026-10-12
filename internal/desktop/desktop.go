// Package desktop 提供一个桌面窗口，用来把内置控制台当原生应用打开。
//
// 三级降级（按可用性从高到低）：
//
//  1. **WebView2 内嵌窗口**（Windows）：真正的原生窗口，控制台嵌在同一个进程里。
//     需要 `WebView2Loader.dll` 与 WebView2 运行时。
//  2. **浏览器 App 模式**：用 Edge/Chrome 的 `--app=<url>` 打开一个无地址栏、
//     无标签页的窗口。外观接近原生应用，不需要任何额外文件。
//  3. **只打印 URL**：都没有时退到这一级，让用户手动打开。
//
// 为什么不用第三方 WebView2 库：`jchv/go-webview2` 依赖 `go-winloader` 与
// `golang.org/x/sys` 三个模块，而它要解决的 `WebView2Loader.dll` 加载问题
// **在纯 stdlib 下同样能解决**（`syscall.NewLazyDLL`）。本项目把"零第三方依赖"
// 当作硬约束，所以自己写绑定 —— 代价是约 600 行 COM/Win32 syscall 代码。
package desktop

import (
	"fmt"
	"strings"
)

// Mode 是实际采用的窗口模式（供日志与诊断）。
type Mode string

const (
	// ModeWebView2 内嵌 WebView2 窗口。
	ModeWebView2 Mode = "webview2"
	// ModeBrowserApp 浏览器 App 模式。
	ModeBrowserApp Mode = "browser-app"
	// ModeNone 只打印 URL。
	ModeNone Mode = "none"
)

// Options 是启动桌面窗口的参数。
type Options struct {
	// URL 要打开的地址（通常是 http://127.0.0.1:<port>/）。
	URL string
	// Title 窗口标题。
	Title string
	// Width / Height 初始窗口尺寸（含边框前的客户区）。
	Width  int
	Height int
	// PreferBrowserApp 跳过 WebView2，直接用浏览器 App 模式。
	//
	// 什么时候需要它：某些环境下 WebView2 运行时损坏或版本不兼容，
	// 而用户只想快点看到界面。也给"我就是不想要内嵌"的人一个开关。
	PreferBrowserApp bool
	// OnReady 在窗口就绪后回调（拿到实际模式）。
	OnReady func(Mode)
	// OnClose 在窗口关闭时回调（用于优雅停机）。
	OnClose func()
}

func (o *Options) withDefaults() Options {
	if o.Width <= 0 {
		o.Width = 1180
	}
	if o.Height <= 0 {
		o.Height = 820
	}
	if strings.TrimSpace(o.Title) == "" {
		o.Title = "Trae2Api 控制台"
	}
	return *o
}

// ErrUnsupported 表示当前平台不支持内嵌窗口。
var ErrUnsupported = fmt.Errorf("desktop: 内嵌窗口仅支持 Windows（其他平台会退回浏览器 App 模式）")
