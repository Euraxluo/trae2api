//go:build windows

package server

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// 本文件实现 **Windows Job Object 收养**：把 dsh 子进程绑到一个
// "句柄关闭即杀掉所有成员" 的 Job Object 上。
//
// ## 为什么必须这么做
//
// 原先的清理只靠 `stopForExit()`（在 SIGTERM/SIGINT 或正常退出路径上跑）。
// 但 Windows 上 `Stop-Process -Force` / 任务管理器结束进程 / 崩溃，走的是
// **TerminateProcess** —— 进程被直接销毁，**没有任何清理代码有机会运行**。
// 于是 dsh 子进程被留下，变成孤儿：占着端口、占着内存，用户完全不知道。
//
// 这不是理论问题：实测在网关被强杀后，留下了
// `DeepSeek Harness.exe --expose-internals …cli.js web --host 127.0.0.1 --port 0 …`
// 孤儿进程（父进程已退出）。
//
// ## Job Object 怎么解决
//
// 创建一个带 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 的 Job Object，把子进程
// `AssignProcessToJobObject` 进去。这个标志的语义是：
//
//	**当 job 的最后一个句柄关闭时，OS 杀掉 job 内的所有进程。**
//
// 而进程死亡时 OS 会自动关闭它持有的所有句柄 —— 所以无论父进程是优雅退出、
// 被 TerminateProcess 强杀、还是崩溃，句柄都会关闭，**子进程都会被连带杀掉**。
//
// 句柄**故意泄漏**（不 CloseHandle）：它必须活到进程结束，这正是机制的一部分。
// 用 `sync.Once` 保证整个进程只有一个 job（多个 job 也能工作，但没必要）。
//
// ## 零依赖
//
// 直接调 kernel32 的三个导出函数（`syscall.NewLazyDLL`），不引入
// `golang.org/x/sys/windows` —— 本项目零第三方依赖是硬约束。

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	// JobObjectExtendedLimitInformation 是 JOBOBJECTINFOCLASS 的枚举值。
	jobObjectExtendedLimitInformation = 9
	// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE：job 句柄全关时杀掉成员进程。
	jobObjectLimitKillOnJobClose = 0x2000

	// AssignProcessToJobObject 要求进程句柄至少有这两个权限。
	processSetQuota  = 0x0100
	processTerminate = 0x0001
)

// 下面三个结构体的字段顺序与类型**必须**与 Windows SDK 逐字节一致 ——
// 顺序错一个字段，SetInformationJobObject 会失败（返回 0）而不会告诉你哪里错。
// 这也是这段代码唯一需要小心的地方。

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformationStruct struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                jobObjectIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	killJobOnce sync.Once
	killJob     syscall.Handle
	killJobErr  error
)

// newKillJob 创建一个 kill-on-close 的 Job Object。
//
// 抽成独立函数是为了**可测**：测试要建一个属于自己的 job，关掉它的句柄，
// 再验证成员进程真的被 OS 杀掉 —— 这直接验证了本文件依赖的机制，
// 而不需要杀掉测试进程自己（单例 killJob 的句柄是故意不关的，测不了这条路径）。
func newKillJob() (syscall.Handle, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateJobObjectW 失败: %v", err)
	}
	info := jobObjectExtendedLimitInformationStruct{
		BasicLimitInformation: jobObjectBasicLimitInformation{
			LimitFlags: jobObjectLimitKillOnJobClose,
		},
	}
	r, _, err := procSetInformationJobObject.Call(
		h,
		uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0, fmt.Errorf("SetInformationJobObject 失败: %v", err)
	}
	return syscall.Handle(h), nil
}

// dshKillJob 惰性创建（并缓存）本进程使用的 kill-on-close Job Object。
func dshKillJob() (syscall.Handle, error) {
	killJobOnce.Do(func() {
		killJob, killJobErr = newKillJob()
	})
	return killJob, killJobErr
}

// assignToJob 把一个进程句柄加入指定 job。
func assignToJob(job syscall.Handle, p *os.Process) error {
	if p == nil {
		return fmt.Errorf("进程为空")
	}
	// 注意：必须用 syscall.OpenProcess 另取一个句柄 —— os.Process 的
	// 内部句柄是未导出字段，且 AssignProcessToJobObject 需要
	// PROCESS_SET_QUOTA|PROCESS_TERMINATE 权限，os 包打开的句柄不保证有。
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(pid=%d) 失败: %w", p.Pid, err)
	}
	defer syscall.CloseHandle(h)

	r, _, callErr := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h))
	if r == 0 {
		return fmt.Errorf("AssignProcessToJobObject(pid=%d) 失败: %v", p.Pid, callErr)
	}
	return nil
}

// adoptProcessIntoKillJob 把子进程加入本进程的 kill-on-close job。
//
// 失败时**不阻止启动** —— 返回错误让调用方记日志即可。
// 理由：Job Object 是"防孤儿"的加固，不是功能本身；因为加固失败而拒绝启动
// dsh，是拿可用性换整洁，不划算。启动后仍由 stopForExit 走正常清理路径。
func adoptProcessIntoKillJob(p *os.Process) error {
	job, err := dshKillJob()
	if err != nil {
		return err
	}
	return assignToJob(job, p)
}
