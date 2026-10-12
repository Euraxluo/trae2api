//go:build windows

package stats

import (
	"syscall"
	"unsafe"
)

// 本文件用 Windows 控制台 API 取窗口尺寸。
//
// ⚠️ 一个必须注意的点：`LazyProc.Call` 的返回值里，**判断成败要看 r1 而不是 err**。
// Windows 上很多 API 用返回值 0 表示失败，而 `err` 在成功时也常常非 nil
//（syscall 会把"最后一次错误"塞进去，即使调用成功了）。所以这里一律判 `r1 != 0`。
// 判 err 的写法会导致"明明成功却走失败分支"，而且报错信息还是上一个无关错误。

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleScreenBuf = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procGetConsoleMode      = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode      = kernel32.NewProc("SetConsoleMode")
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// termSize 返回控制台窗口的列数与行数。
func termSize() (cols, rows int, ok bool) {
	var info consoleScreenBufferInfo
	r1, _, _ := procGetConsoleScreenBuf.Call(
		uintptr(syscall.Stdout), uintptr(unsafe.Pointer(&info)))
	if r1 == 0 { // ← 判 r1，不判 err
		return 0, 0, false
	}
	// 用 Window 而不是 Size：Size 是**缓冲区**尺寸（可能远大于可见窗口，
	// 例如缓冲区 9999 行），用它会算出巨大的行数导致帧被裁错。
	cols = int(info.Window.Right-info.Window.Left) + 1
	rows = int(info.Window.Bottom-info.Window.Top) + 1
	return cols, rows, cols > 0 && rows > 0
}

// enableVT 尝试开启虚拟终端处理（让 ANSI 转义序列生效）。
//
// Windows 10 1511+ 支持。失败不算致命：我们会退回不用 ANSI 的渲染方式。
func enableVT() bool {
	var mode uint32
	r1, _, _ := procGetConsoleMode.Call(uintptr(syscall.Stdout), uintptr(unsafe.Pointer(&mode)))
	if r1 == 0 {
		return false
	}
	const enableVirtualTerminalProcessing = 0x0004
	r1, _, _ = procSetConsoleMode.Call(uintptr(syscall.Stdout), uintptr(mode|enableVirtualTerminalProcessing))
	return r1 != 0
}

// clearScreen 清屏并把光标移到左上角。
//
// ⚠️ 刻意用 ED(0)（从光标清到屏幕末尾）+ 光标归位，而**不用** ED(2)（清整个屏幕）。
// Windows conhost 在收到 `2J` 时会把当前屏幕内容**滚入 scrollback**，
// 于是每重绘一次就多留一份历史 —— 用户缩放几次窗口后向上滚动会看到叠了好几份表。
// ED(0) 不会触发这个行为。
func clearScreen() {
	print("\x1b[H\x1b[0J")
}
