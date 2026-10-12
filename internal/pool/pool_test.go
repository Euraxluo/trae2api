package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	return New(Options{
		MaxInFlight:      2,
		BreakerThreshold: 3,
		BreakerCooldown:  time.Minute,
		IdlePerHour:      0.5,
		IdleMax:          5,
		PickGap:          0, // 测试不引入人为延迟
		SoftRateBase:     100 * time.Millisecond,
		SoftRateMax:      time.Second,
		ModelBase:        100 * time.Millisecond,
	})
}

func addAcct(p *Pool, uid, realm, owner string) *Account {
	a := &Account{UID: uid, Realm: realm, Owner: owner, Label: uid}
	p.Add(a)
	return a
}

// TestPickRealmFilter 选号必须严格按域过滤（trae 的账号不能被 solo 请求选中）。
func TestPickRealmFilter(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "trae-1", "trae", "public")
	addAcct(p, "solo-1", "solo", "public")

	for i := 0; i < 20; i++ {
		if got := p.Pick("trae", "m", nil, nil); got == nil || got.Realm != "trae" {
			t.Fatalf("Pick(trae) 选到了 %v", got)
		}
		if got := p.Pick("solo", "m", nil, nil); got == nil || got.Realm != "solo" {
			t.Fatalf("Pick(solo) 选到了 %v", got)
		}
	}
}

// TestPickOwnerIsolation 多租户隔离：非 public 账号只归其 owner。
func TestPickOwnerIsolation(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "alice-acct", "trae", "alice")
	addAcct(p, "bob-acct", "trae", "bob")

	for i := 0; i < 20; i++ {
		if got := p.Pick("trae", "m", []string{"alice"}, nil); got == nil || got.UID != "alice-acct" {
			t.Fatalf("alice 选到了 %v（越权）", got)
		}
		if got := p.Pick("trae", "m", []string{"bob"}, nil); got == nil || got.UID != "bob-acct" {
			t.Fatalf("bob 选到了 %v（越权）", got)
		}
	}
	// public 账号对任何 owner 都可用
	addAcct(p, "shared", "trae", "public")
	if got := p.Pick("trae", "m", []string{"carol"}, nil); got == nil || got.UID != "shared" {
		t.Fatalf("carol 应能选到 public 账号，得到 %v", got)
	}
	// 一个用户可同时以 ID 与用户名标识自己（两者都算归属）
	addAcct(p, "alice-by-name", "trae", "Alice")
	if got := p.Pick("trae", "m", []string{"alice-id", "Alice"}, nil); got == nil {
		t.Fatal("应能按用户名（大小写不敏感）匹配到归属账号")
	}
}

// TestPickTriedExclusion 轮换：tried 中的账号必须被排除。
func TestPickTriedExclusion(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "a", "trae", "public")
	addAcct(p, "b", "trae", "public")

	tried := map[string]bool{"a": true}
	for i := 0; i < 10; i++ {
		if got := p.Pick("trae", "m", nil, tried); got == nil || got.UID != "b" {
			t.Fatalf("tried 排除失效，选到 %v", got)
		}
	}
	tried["b"] = true
	if got := p.Pick("trae", "m", nil, tried); got != nil {
		t.Fatalf("全部 tried 时应返回 nil，得到 %v", got.UID)
	}
}

// TestCooldownHardExcludedFromFallback 硬冷却（凭证失效）绝不参与兜底。
func TestCooldownHardExcludedFromFallback(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "dead", "trae", "public")
	p.Cooldown("dead", CoolHard, time.Hour, "凭证失效")

	if got := p.Pick("trae", "m", nil, nil); got != nil {
		t.Fatalf("硬冷却账号不应被选中，得到 %v", got.UID)
	}
	if p.Servable("trae") {
		t.Error("Servable 应为 false")
	}
}

// TestCooldownSoftEligibleAsFallback 软冷却（限流）仍可作为兜底候选 ——
// 因为可能已恢复，失败成本仅一轮换。
func TestCooldownSoftEligibleAsFallback(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "limited", "trae", "public")
	p.Cooldown("limited", CoolSoft, time.Hour, "4011 限流")

	if got := p.Pick("trae", "m", nil, nil); got == nil || got.UID != "limited" {
		t.Fatalf("软冷却账号应可作为兜底，得到 %v", got)
	}
}

// TestModelCooldownScopedToModel 模型级冷却只影响该模型（换模型即豁免）。
func TestModelCooldownScopedToModel(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "a", "trae", "public")
	p.CooldownModel("a", "glm-5.2", time.Hour, "4001")

	if got := p.Pick("trae", "glm-5.2", nil, nil); got != nil {
		t.Fatalf("该模型应被冷却，却选到 %v", got.UID)
	}
	if got := p.Pick("trae", "kimi-k3", nil, nil); got == nil || got.UID != "a" {
		t.Fatalf("换模型应豁免，得到 %v", got)
	}
	// 硬冷却会清空模型级豁免
	p.Cooldown("a", CoolHard, time.Hour, "凭证失效")
	a := p.Get("a")
	a.mu.Lock()
	n := len(a.modelCooling)
	a.mu.Unlock()
	if n != 0 {
		t.Errorf("硬冷却后模型级豁免应被清空，仍有 %d 条", n)
	}
}

// TestInFlightLease 并发租约：达到上限后不再被选中，Release 后恢复。
func TestInFlightLease(t *testing.T) {
	p := newTestPool(t) // maxInFlight = 2
	addAcct(p, "a", "trae", "public")

	if !p.Acquire("a") || !p.Acquire("a") {
		t.Fatal("前两次 Acquire 应成功")
	}
	if p.Acquire("a") {
		t.Fatal("第三次 Acquire 应失败（上限 2）")
	}
	if got := p.Pick("trae", "m", nil, nil); got != nil {
		t.Fatalf("在途已满不应被选中，得到 %v", got.UID)
	}
	p.Release("a")
	if !p.Acquire("a") {
		t.Fatal("Release 后应可再次 Acquire")
	}
	// 多余 Release 不应把计数压成负数
	p.Release("a")
	p.Release("a")
	p.Release("a")
	a := p.Get("a")
	a.mu.Lock()
	inflight := a.inFlight
	a.mu.Unlock()
	if inflight < 0 {
		t.Errorf("inFlight 不应为负: %d", inflight)
	}
}

// TestBreakerAfterThreshold 连续失败达阈值触发熔断。
func TestBreakerAfterThreshold(t *testing.T) {
	p := newTestPool(t) // threshold = 3
	addAcct(p, "a", "trae", "public")

	p.NoteError("a")
	p.NoteError("a")
	if st := p.List()[0]; st.Breaker {
		t.Fatal("未达阈值不应熔断")
	}
	p.NoteError("a")
	if st := p.List()[0]; !st.Breaker {
		t.Fatal("达到阈值应熔断")
	}
	// 成功后应清除熔断
	p.NoteSuccess("a")
	if st := p.List()[0]; st.Breaker {
		t.Fatal("成功后应清除熔断")
	}
}

// TestSoftRateBackoff 软冷却按连续次数指数退避并封顶。
func TestSoftRateBackoff(t *testing.T) {
	p := newTestPool(t) // base 100ms, max 1s
	addAcct(p, "a", "trae", "public")
	d1 := p.CooldownSoftRate("a", "4011")
	d2 := p.CooldownSoftRate("a", "4011")
	d3 := p.CooldownSoftRate("a", "4011")
	if !(d1 < d2 && d2 < d3) {
		t.Fatalf("冷却应递增: %v %v %v", d1, d2, d3)
	}
	for i := 0; i < 10; i++ {
		p.CooldownSoftRate("a", "4011")
	}
	if d := p.CooldownSoftRate("a", "4011"); d > time.Second {
		t.Errorf("冷却应封顶在 1s，得到 %v", d)
	}
	// 成功后连击清零
	p.NoteSuccess("a")
	if d := p.CooldownSoftRate("a", "4011"); d != 100*time.Millisecond {
		t.Errorf("成功后连击应清零，得到 %v", d)
	}
}

// TestClearCooldown 一键解除应同时清掉账号级、熔断与模型级。
func TestClearCooldown(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "a", "trae", "public")
	p.Cooldown("a", CoolSoft, time.Hour, "x")
	p.CooldownModel("a", "m1", time.Hour, "y")
	p.NoteError("a")
	p.NoteError("a")
	p.NoteError("a")

	if !p.ClearCooldown("a") {
		t.Fatal("ClearCooldown 应报告有变更")
	}
	st := p.List()[0]
	if st.Cooling || st.Breaker || len(st.ModelCooling) > 0 {
		t.Fatalf("解除后仍有残留: %+v", st)
	}
	if p.ClearCooldown("a") {
		t.Error("再次解除应报告无变更")
	}
}

// TestSetDisabled 手动禁用应让账号完全退出选号。
func TestSetDisabled(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "a", "trae", "public")
	if !p.SetDisabled("a", true, "维护中") {
		t.Fatal("SetDisabled 应成功")
	}
	if got := p.Pick("trae", "m", nil, nil); got != nil {
		t.Fatalf("禁用账号不应被选中，得到 %v", got.UID)
	}
	if st := p.List()[0]; !st.Disabled || st.DisableReason != "维护中" {
		t.Errorf("禁用状态未反映: %+v", st)
	}
	p.SetDisabled("a", false, "")
	if got := p.Pick("trae", "m", nil, nil); got == nil {
		t.Fatal("启用后应可被选中")
	}
}

// TestCounts 汇总口径。
func TestCounts(t *testing.T) {
	p := newTestPool(t)
	addAcct(p, "ok1", "trae", "public")
	addAcct(p, "ok2", "solo", "public")
	addAcct(p, "cool", "trae", "public")
	addAcct(p, "off", "trae", "public")
	p.Cooldown("cool", CoolSoft, time.Hour, "x")
	p.SetDisabled("off", true, "x")

	total, healthy, cooling, disabled, _ := p.Counts()
	if total != 4 || healthy != 2 || cooling != 1 || disabled != 1 {
		t.Errorf("Counts = total=%d healthy=%d cooling=%d disabled=%d want 4/2/1/1",
			total, healthy, cooling, disabled)
	}
	tt, th, tc, td, _ := p.CountsForRealm("trae")
	if tt != 3 || th != 1 || tc != 1 || td != 1 {
		t.Errorf("CountsForRealm(trae) = %d/%d/%d/%d want 3/1/1/1", tt, th, tc, td)
	}
	_, sh, _, _, _ := p.CountsForRealm("solo")
	if sh != 1 {
		t.Errorf("CountsForRealm(solo) healthy=%d want 1", sh)
	}
}

// TestPersistenceRoundTrip 状态落盘与恢复（冷却/熔断/禁用/计数）。
func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	p1 := New(Options{MaxInFlight: 1, BreakerThreshold: 3, BreakerCooldown: time.Minute,
		SoftRateBase: time.Minute, SoftRateMax: time.Hour, ModelBase: time.Minute,
		StateFile: stateFile})
	addAcct(p1, "a", "trae", "public")
	addAcct(p1, "b", "trae", "public")
	p1.Cooldown("a", CoolSoft, time.Hour, "限流")
	p1.CooldownModel("a", "glm-5.2", time.Hour, "4001")
	p1.SetDisabled("b", true, "维护")
	p1.NoteError("b")
	p1.SetCredits("a", 1234)

	if err := p1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("状态文件未生成: %v", err)
	}

	p2 := New(Options{MaxInFlight: 1, BreakerThreshold: 3, BreakerCooldown: time.Minute,
		SoftRateBase: time.Minute, SoftRateMax: time.Hour, ModelBase: time.Minute,
		StateFile: stateFile})
	addAcct(p2, "a", "trae", "public")
	addAcct(p2, "b", "trae", "public")
	if err := p2.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	byUID := map[string]Status{}
	for _, st := range p2.List() {
		byUID[st.UID] = st
	}
	if !byUID["a"].Cooling || byUID["a"].CoolReason != "限流" {
		t.Errorf("账号 a 冷却未恢复: %+v", byUID["a"])
	}
	if len(byUID["a"].ModelCooling) == 0 {
		t.Error("账号 a 模型级冷却未恢复")
	}
	if byUID["a"].Credits == nil || *byUID["a"].Credits != 1234 {
		t.Errorf("账号 a 余额未恢复: %v", byUID["a"].Credits)
	}
	if !byUID["b"].Disabled || byUID["b"].DisableReason != "维护" {
		t.Errorf("账号 b 禁用状态未恢复: %+v", byUID["b"])
	}
	if byUID["b"].Fails != 1 {
		t.Errorf("账号 b 失败计数未恢复: %d", byUID["b"].Fails)
	}
}

// TestRestoreUnknownUIDIgnored 状态里有已移除的账号时不应报错（便于换号）。
func TestRestoreUnknownUIDIgnored(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "state.json")
	if err := os.WriteFile(f, []byte(`{"version":1,"items":{"ghost":{"disabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Options{StateFile: f, MaxInFlight: 1, SoftRateBase: time.Minute, SoftRateMax: time.Hour, ModelBase: time.Minute})
	addAcct(p, "real", "trae", "public")
	if err := p.Restore(); err != nil {
		t.Fatalf("未知 UID 不应导致报错: %v", err)
	}
	if st := p.List()[0]; st.Disabled {
		t.Error("未知 UID 的状态不应被误应用到其他账号")
	}
}

// TestRestoreMissingFileOK 首次启动（状态文件不存在）应静默通过。
func TestRestoreMissingFileOK(t *testing.T) {
	p := New(Options{StateFile: filepath.Join(t.TempDir(), "nope.json"), MaxInFlight: 1,
		SoftRateBase: time.Minute, SoftRateMax: time.Hour, ModelBase: time.Minute})
	if err := p.Restore(); err != nil {
		t.Fatalf("缺失状态文件应静默通过: %v", err)
	}
}

// TestPickNoAccounts 空池必须返回 nil（而不是 panic）。
func TestPickNoAccounts(t *testing.T) {
	p := newTestPool(t)
	if got := p.Pick("trae", "m", nil, nil); got != nil {
		t.Fatalf("空池应返回 nil，得到 %v", got)
	}
	if p.Servable("trae") {
		t.Error("空池 Servable 应为 false")
	}
	if p.Acquire("ghost") {
		t.Error("对不存在账号 Acquire 应返回 false")
	}
	p.Release("ghost") // 不应 panic
}

// 说明：原 TestAccountUIDUniqueness 已删除。
//
// 它是同义反复 —— 在测试里自己把 p.Key + "-<userId>" 拼一遍，再断言两个
// 字符串字面量不相等，**从不调用** AccountUID，因此没有覆盖任何生产代码。
//
// 真正调用 AccountUID 的版本在 internal/server/identity_test.go。不能放在本包：
// AccountUID 属于 package server，而 package server 已经 import package pool，
// 在 package pool 里调 server.AccountUID 会构成 pool → server → pool 的
// import cycle，根本编译不过。详见
// .dsh-team/trae2api-gaps/work/account-identity/notes.md。
