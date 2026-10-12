//go:build windows

package server

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// 本文件验证 dsh 的**防孤儿加固**：Windows Job Object。
//
// ## 为什么必须有这组测试
//
// 加固完全建立在 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 这个**内核行为**上：
// job 的最后一个句柄关闭时，OS 杀掉 job 内的所有进程。
//
// 如果这个前提不成立（结构体布局写错一个字段、常量写错、权限不足），
// 整套加固会**静默失效**：代码在、日志无异常、孤儿照样产生。而"网关被强杀"
// 这个场景在人工测试里很难碰到，所以只能靠测试把机制本身钉住。
//
// ## 测试策略
//
// 不测"启动网关再强杀"（那需要杀掉测试进程自己，做不到），而是**直接复现机制**：
// 建一个自己的 job → 收养一个长命子进程 → 关掉 job 句柄 → 断言子进程真的死了。
//
// 它验证"关句柄会杀成员"这个事实。"网关被强杀时会关句柄"由 OS 保证
//（进程终止必然释放句柄），不需要也不该由测试来验。

// jobTestChild 起一个长命的**直接**子进程，返回 cmd 与一个等待通道。
//
// 用测试二进制自己当子进程（`-test.run=TestDSHHelperProcess` + `noprint` 模式，
// 那个模式会 sleep 60s）—— 这是本包已有的、已在其它测试里验证可行的做法。
//
// ⚠️ 不要用 `cmd /c ping ...` 之类的包装：那样 job 里是 cmd.exe，杀掉它之后
// 真正的 ping.exe 仍在（它是 cmd 的子进程，不在我们的 job 里），
// 而且 cmd.Wait() 会一直等 —— 实测这条让测试挂到超时。
// 真实 dsh 是**直接** spawn 的（`DeepSeek Harness.exe …`），测试必须等价。
func jobTestChild(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestDSHHelperProcess")
	cmd.Env = append(os.Environ(), "TRAE2API_DSH_FAKE=noprint")
	if err := cmd.Start(); err != nil {
		t.Fatalf("起测试子进程失败: %v", err)
	}
	// 用 **closed channel** 而不是"发一个值"：
	// 值通道只能被消费一次，而调用方（测试主体）与 Cleanup 都会等它 ——
	// 主体消费掉之后 Cleanup 就永远读不到，会误报"未回收"。
	// close 是幂等的，多方等待都成立。
	done := make(chan struct{})
	go func() { defer close(done); _ = cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-done:
			return // 已经退出了
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Logf("警告：子进程 %d 在 Kill 后 5s 仍未回收", cmd.Process.Pid)
		}
	})
	return cmd, done
}

// TestKillOnJobCloseActuallyKillsChild 关掉 job 句柄必须真的杀掉成员进程。
//
// 这是整组加固的**根本前提**。它失败意味着 dsh 的防孤儿是假的。
func TestKillOnJobCloseActuallyKillsChild(t *testing.T) {
	job, err := newKillJob()
	if err != nil {
		t.Fatalf("建 kill-on-close job 失败: %v", err)
	}
	cmd, done := jobTestChild(t)
	pid := cmd.Process.Pid

	if err := assignToJob(job, cmd.Process); err != nil {
		syscall.CloseHandle(job)
		t.Fatalf("收养子进程失败: %v", err)
	}
	// 先确认它真的活着 —— 否则后面的"死了"证明不了任何事
	if !processAlive(pid) {
		syscall.CloseHandle(job)
		t.Fatalf("子进程 %d 在收养后立刻就不在了，测试无意义", pid)
	}

	// 关掉 job 的最后一个句柄 → OS 应当杀掉成员进程
	syscall.CloseHandle(job)

	select {
	case <-done:
		if processAlive(pid) {
			t.Errorf("cmd.Wait 返回了但 pid %d 仍存活，状态不一致", pid)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("关闭 job 句柄 15s 后子进程 %d 仍活着 —— "+
			"JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 没生效，dsh 的防孤儿加固是假的", pid)
	}
}

// TestAdoptProcessIntoKillJobSucceeds 生产调用链（单例 job）要能走通。
//
// 与上一条互补：上一条验"机制真的会杀"，这条验"我们的调用没报错"。
// 两条都过，才能说加固有效。
func TestAdoptProcessIntoKillJobSucceeds(t *testing.T) {
	cmd, _ := jobTestChild(t)
	if err := adoptProcessIntoKillJob(cmd.Process); err != nil {
		t.Fatalf("adoptProcessIntoKillJob 失败: %v", err)
	}
	// 幂等：重复收养同一进程不该报错
	if err := adoptProcessIntoKillJob(cmd.Process); err != nil {
		t.Errorf("重复收养应当幂等，却报错: %v", err)
	}
	if !processAlive(cmd.Process.Pid) {
		t.Error("收养不是杀进程，子进程不该消失")
	}
}

// TestAdoptNilProcessErrors nil 进程要明确报错，而不是 panic。
func TestAdoptNilProcessErrors(t *testing.T) {
	if err := adoptProcessIntoKillJob(nil); err == nil {
		t.Error("nil 进程应当报错")
	}
	if err := assignToJob(0, nil); err == nil {
		t.Error("assignToJob(nil) 应当报错")
	}
}

// TestNewKillJobIsIndependent 每次 newKillJob 建的是**独立** job。
//
// 测试必须建自己的 job —— 否则关句柄会把单例 job 关掉，连带影响
// 同包其它测试与真实 dsh。
func TestNewKillJobIsIndependent(t *testing.T) {
	a, err := newKillJob()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newKillJob()
	if err != nil {
		syscall.CloseHandle(a)
		t.Fatal(err)
	}
	defer syscall.CloseHandle(b)
	if a == b {
		t.Fatal("两次 newKillJob 返回同一句柄 —— 不是独立的 job，关一个会影响另一个")
	}
	// 关掉 a 之后 b 仍应可用
	syscall.CloseHandle(a)
	cmd, _ := jobTestChild(t)
	if err := assignToJob(b, cmd.Process); err != nil {
		t.Errorf("关掉 a 之后 b 应当仍可用，却报错: %v", err)
	}
}

// TestDshKillJobIsSingleton 生产用的 job 是单例（sync.Once）。
//
// 多个 job 也能工作，但没必要；而且单例让"关句柄"的时机确定
// （进程结束时），语义更清楚。
func TestDshKillJobIsSingleton(t *testing.T) {
	h1, err := dshKillJob()
	if err != nil {
		t.Fatalf("dshKillJob 失败: %v", err)
	}
	h2, err := dshKillJob()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("dshKillJob 应当是单例，得到两个不同句柄 %v / %v", h1, h2)
	}
	if h1 == 0 {
		t.Error("句柄不该为 0")
	}
	// ⚠️ 刻意**不**关这个句柄：它必须活到进程结束，机制才成立。
}

// processAlive 报告 pid 是否仍存活。
func processAlive(pid int) bool {
	const stillActive = 259 // STILL_ACTIVE
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
