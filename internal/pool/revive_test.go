package pool

import (
	"testing"
	"time"
)

// TestReviveClearsAllFiveStates 是 pool.Revive 的单元级回归测试：
// 复活必须一次清掉**五样** —— 账号级冷却 / 熔断 / 已分类连败计数 fails /
// 连败降权 degradeUntil(+consecutiveFails) / 模型级冷却。
//
// 为什么这条必须有（它不只是 server 层测试的重复）：
// ClearCooldown 与 Revive 的差别**全部**藏在 Account 的非导出字段里
// （degradeUntil、consecutiveFails），从 server 包看不出来。
// 只要有人日后把 Revive 改成"转调 ClearCooldown"，server 层的
// TestAccountsReviveClearsAllFive 会红 —— 这条测试则直接钉住 pool 自己的语义：
// 半复活（清了三样、留着降权）在这里就报错，不必等到 HTTP 层。
func TestReviveClearsAllFiveStates(t *testing.T) {
	p := New(Options{MaxInFlight: 1, DegradeThreshold: 2, DegradeCooldown: time.Minute})
	p.Add(&Account{UID: "trae-cn-revive", Realm: "trae", Owner: "public"})

	now := time.Now()
	cooling := now.Add(30 * time.Minute)
	breaker := now.Add(20 * time.Minute)
	degrade := now.Add(10 * time.Minute)
	if !p.SetRuntimeForTest("trae-cn-revive", RuntimeOverride{
		CoolingUntil: &cooling, CoolKind: CoolHard, CoolReason: "测试预置",
		BreakerUntil: &breaker, Fails: 3,
		DegradeUntil: &degrade, Consecutive: 5,
		ModelCooling: map[string]time.Time{"glm-5.2": now.Add(5 * time.Minute)},
	}) {
		t.Fatal("账号不在池内")
	}

	// 前置：五样都真的卡住了
	before := p.Get("trae-cn-revive").Status(now)
	if !before.Cooling || !before.Breaker || !before.Degraded ||
		before.Fails != 3 || before.UnclassifiedFails != 5 || len(before.ModelCooling) != 1 {
		t.Fatalf("前置状态不满足: %+v", before)
	}

	ch, ok := p.Revive("trae-cn-revive")
	if !ok {
		t.Fatal("Revive 报告账号不存在")
	}
	if !ch.Changed() {
		t.Fatal("前置五样俱全，Changed() 必须为 true")
	}

	after := p.Get("trae-cn-revive").Status(time.Now())
	if after.Cooling || after.Breaker || after.Degraded ||
		after.Fails != 0 || after.UnclassifiedFails != 0 || len(after.ModelCooling) != 0 {
		t.Errorf("复活后仍有残留状态: %+v", after)
	}
	if !after.Healthy {
		t.Errorf("复活后仍不 healthy: %+v", after)
	}
	// 必须真的能被选中（前几项是"资格"，这一条是"结果"）
	if got := p.Pick("trae", "glm-5.2", nil, nil); got == nil {
		t.Error("复活后账号仍选不中")
	}
}

// TestReviveUnknownUID 不存在的 uid 必须报 ok=false（而不是"没有变更"），
// 否则调用方无法区分"账号不存在"与"账号本来就是好的"。
func TestReviveUnknownUID(t *testing.T) {
	p := New(Options{MaxInFlight: 1})
	if _, ok := p.Revive("trae-cn-不存在"); ok {
		t.Error("不存在的 uid 应返回 ok=false")
	}
}

// TestReviveExpiredCooldownNotReportedAsChanged 已经自然过期的冷却不算"我救活了它"。
//
// 否则控制台会显示"已复活 1 个账号"，而那个账号本来就能用 —— 用户会以为
// 复活按钮有副作用，进而不敢点。
func TestReviveExpiredCooldownNotReportedAsChanged(t *testing.T) {
	p := New(Options{MaxInFlight: 1})
	p.Add(&Account{UID: "trae-cn-expired", Realm: "trae", Owner: "public"})
	past := time.Now().Add(-time.Hour)
	p.SetRuntimeForTest("trae-cn-expired", RuntimeOverride{
		CoolingUntil: &past, BreakerUntil: &past, DegradeUntil: &past,
		ModelCooling: map[string]time.Time{"glm-5.2": past},
	})
	ch, ok := p.Revive("trae-cn-expired")
	if !ok {
		t.Fatal("账号不在池内")
	}
	if ch.Cooldown || ch.Breaker || ch.Degrade {
		t.Errorf("已过期的冷却不该被报成已清除: %+v", ch)
	}
}
