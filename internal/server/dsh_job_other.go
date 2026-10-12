//go:build !windows

package server

import "os"

// adoptProcessIntoKillJob 在非 Windows 平台上是**空操作**。
//
// ## 为什么这里没有对等实现
//
// Windows 用 Job Object（见 dsh_job_windows.go）解决"父进程被强杀后子进程成孤儿"。
// Unix 上对应的机制是**进程组**：`Setpgid` 建新进程组，父进程死时给整个组发信号。
// 但两者的语义并不等价：
//
//   - Job Object 由**内核**在句柄关闭时自动执行，父进程**不需要**有机会运行代码；
//   - Unix 的进程组需要有人**主动** `kill(-pgid)` —— 父进程被 SIGKILL 时同样
//     没有机会运行，孤儿照样存在。要真正等价得靠 `PR_SET_PDEATHSIG`（Linux）或
//     `kqueue`/`procctl`（BSD），各平台 API 都不同。
//
// 而本项目的桌面 GUI 是 Windows 专属（WebView2），dsh 内嵌的主要使用场景也在
// Windows。与其在 Unix 上写一个**看起来有、实际不生效**的实现（那比没有更危险：
// 读者会以为孤儿问题已经解决了），不如在这里**明确返回 nil 并在注释里说清楚**，
// 让 Unix 用户知道这一层加固不存在。
//
// Unix 上的实际行为：网关**优雅退出**（SIGTERM/SIGINT）时 `stopForExit()` 会
// 正常清理 dsh；被 SIGKILL 强杀时会留下孤儿 —— 这是已知且已记录的差异。
func adoptProcessIntoKillJob(p *os.Process) error {
	return nil
}
