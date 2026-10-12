//go:build !windows

package stats

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// termSize 通过 TIOCGWINSZ 取终端尺寸。
func termSize() (cols, rows int, ok bool) {
	type winsize struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(syscall.Stdout), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		// 退化方案：问 stty（几乎所有 Unix 都有）
		out, err := exec.Command("stty", "size").Output()
		if err != nil {
			return 0, 0, false
		}
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) != 2 {
			return 0, 0, false
		}
		r, _ := strconv.Atoi(parts[0])
		c, _ := strconv.Atoi(parts[1])
		return c, r, c > 0 && r > 0
	}
	return int(ws.Col), int(ws.Row), true
}

// enableVT 在 Unix 上恒真（ANSI 是原生行为）。
func enableVT() bool { return true }

// clearScreen 清屏并归位。
//
// 用 ED(0) 而不是 ED(2) —— 与 Windows 侧保持一致的行为，
// 避免在某些终端模拟器里把内容推进 scrollback。
func clearScreen() { print("\x1b[H\x1b[0J") }

// 保证 os 被使用（isTTY 在 main 里用）。
var _ = os.Stdout
