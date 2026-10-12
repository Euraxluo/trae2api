package server

// 本文件提供**管理中枢**（`/console`）的页面：视觉与功能面来自参考实现
// （workbuddy2api 的 dashboard.html，6 标签 + 5 模态），**数据层整体改绑**
// 到 Trae2Api 自己的端点（见 console_assets/app.js 顶部的映射表）。
//
// ## 为什么把资源 embed 进来、并在响应时内联
//
//  1. **零外部资源**（硬约束）。参考页面用 `cdn.tailwindcss.com` +
//     `fonts.googleapis.com`；本项目控制台必须**自包含、离线可用**，而且
//     CSP 是 `default-src 'self'`（见 security.go）—— CDN 脚本会被浏览器直接
//     拦掉，症状是"页面加载了但样式全无、JS 不执行"，控制台自己还不报错。
//     所以：用 Tailwind CLI 按页面实际用到的类**本地编译**一份 CSS，
//     与 JS 一起 `//go:embed` 进二进制，页面只引用本地内容。
//  2. **不改路由表**。资源不走 `/console/app.js` 这类子路径 —— 那需要在
//     server.go 里新注册路由，而 server.go 是并行角色的共享文件。
//     这里在 `init()` 里把 CSS/JS 注入到 index.html 的两个占位符上，
//     `/console` 仍然只返回**一个**自包含 HTML 响应。
//
// ## 许可归属
//
// console_assets/console.css 由 Tailwind CSS v3.4.17（MIT License，
// Copyright (c) Tailwind Labs, Inc.）编译产出。为了满足"页面零外域引用"，
// 编译产物横幅里的 URL 已被去掉（见 work/console-ui/build-css.mjs），
// 完整许可文本记录在 work/console-ui/notes.md。
//
// ## 字体
//
// 不引外网字体：font-sans 降级到
// `system-ui, -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif`，
// font-mono 用 `ui-monospace, Consolas, monospace`（在 tailwind.config.js 里定义）。

import (
	_ "embed"
	"strings"
)

//go:embed console_assets/index.html
var consoleIndexHTML string

//go:embed console_assets/app.js
var consoleAppJS string

//go:embed console_assets/console.css
var consoleAppCSS string

// 注入占位符。用两个不可能出现在 CSS/JS 里的 token，避免误替换。
const (
	consoleCSSPlaceholder = "/*__CONSOLE_CSS__*/"
	consoleJSPlaceholder  = "/*__CONSOLE_JS__*/"
)

// consoleHTML 是最终返回给浏览器的完整页面（构建期注入，运行期只读）。
//
// 变量而不是常量：`server.go` 里 handleConsole 直接 `io.WriteString(w, consoleHTML)`，
// 保持这个名字与类型可用，就不必去改共享的路由文件。
var consoleHTML = buildConsoleHTML()

func buildConsoleHTML() string {
	// 防御：占位符必须各出现一次，否则说明资源与页面不同步。
	if strings.Count(consoleIndexHTML, consoleCSSPlaceholder) != 1 ||
		strings.Count(consoleIndexHTML, consoleJSPlaceholder) != 1 {
		// 不 panic：一个页面拼装问题不该让整个网关起不来。
		// 但仍然要能被测试抓到（见 console_test.go 的 TestConsolePageAssembly）。
		return consoleIndexHTML
	}
	page := strings.Replace(consoleIndexHTML, consoleCSSPlaceholder, consoleAppCSS, 1)
	page = strings.Replace(page, consoleJSPlaceholder, consoleAppJS, 1)
	return page
}
