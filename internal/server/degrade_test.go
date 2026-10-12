package server

import (
	"testing"
	"time"
)

// TestNextMidnightCST 零点计算的边界语义。
//
// 用固定 +08:00 偏移而**不是** time.LoadLocation("Asia/Shanghai")：
// 容器与精简发行版常缺 tzdata，LoadLocation 会失败并回落到 UTC，
// 于是"次日零点"算错 8 小时，降级期在用户看来是随机结束的。
func TestNextMidnightCST(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"23:59 → 次日 00:00（几秒后）",
			time.Date(2026, 10, 9, 23, 59, 30, 0, cst),
			time.Date(2026, 10, 10, 0, 0, 0, 0, cst),
		},
		{
			"00:00:00 → **次日** 00:00（刚过零点，下一个零点是明天）",
			time.Date(2026, 10, 9, 0, 0, 0, 0, cst),
			time.Date(2026, 10, 10, 0, 0, 0, 0, cst),
		},
		{
			"12:00 → 次日 00:00",
			time.Date(2026, 10, 9, 12, 0, 0, 0, cst),
			time.Date(2026, 10, 10, 0, 0, 0, 0, cst),
		},
		{
			"00:00:01 → 次日 00:00",
			time.Date(2026, 10, 9, 0, 0, 1, 0, cst),
			time.Date(2026, 10, 10, 0, 0, 0, 0, cst),
		},
	}
	for _, c := range cases {
		got := nextMidnightCST(c.now)
		if !got.Equal(c.want) {
			t.Errorf("%s\n  got  %v\n  want %v", c.name, got, c.want)
		}
		if !got.After(c.now) {
			t.Errorf("%s：结果必须严格晚于 now", c.name)
		}
	}
}

// TestNextMidnightCSTIndependentOfLocalZone 结果不依赖进程本地时区。
//
// 这是"用 FixedZone 而不是 LoadLocation"的直接收益：无论 TZ 环境变量怎么设，
// 算出来的零点都必须是 +08:00 的零点。
func TestNextMidnightCSTIndependentOfLocalZone(t *testing.T) {
	// 同一时刻用不同时区表示，算出的**绝对时刻**必须相同
	utc := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC) // = CST 23:00
	cst := utc.In(time.FixedZone("CST", 8*60*60))
	a := nextMidnightCST(utc)
	b := nextMidnightCST(cst)
	if !a.Equal(b) {
		t.Errorf("同一时刻的不同时区表示算出了不同结果：%v vs %v", a, b)
	}
	// 期望是 CST 的次日 00:00 = UTC 16:00
	want := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	if !a.Equal(want) {
		t.Errorf("got %v want %v", a.UTC(), want)
	}
}

// TestDegradeGateTriggerAndActive 触发后进入降级期。
func TestDegradeGateTriggerAndActive(t *testing.T) {
	var g degradeGate
	if g.Active() {
		t.Fatal("初始不该处于降级期")
	}
	g.Trigger()
	if !g.Active() {
		t.Fatal("触发后应处于降级期")
	}
	st := g.Status()
	if st["active"] != true {
		t.Errorf("状态未反映激活: %v", st)
	}
	if st["triggered_total"] != int64(1) {
		t.Errorf("触发计数 = %v want 1", st["triggered_total"])
	}
	if _, ok := st["until"]; !ok {
		t.Error("激活时应给出 until")
	}
	if _, ok := st["remain_seconds"]; !ok {
		t.Error("激活时应给出剩余秒数")
	}
}

// TestDegradeGateDoesNotExtend 降级期内重复触发**不续期**。
//
// ⚠️ 这是本闸最重要的一条语义。若每次都续期，连续误报会让降级期无限延长，
// 变成"永远在用降级提示词"—— 而用户完全不知道为什么自己的 system 提示词失效了。
func TestDegradeGateDoesNotExtend(t *testing.T) {
	var g degradeGate
	g.Trigger()
	first := g.until
	if first.IsZero() {
		t.Fatal("未设置 until")
	}

	// 多次触发（模拟连续误报）
	for i := 0; i < 10; i++ {
		g.Trigger()
	}
	if !g.until.Equal(first) {
		t.Errorf("重复触发不应改变 until：%v → %v（会被无限延长）", first, g.until)
	}
	// 但触发计数会累加（诊断用）
	if got := g.Status()["triggered_total"]; got != int64(1) {
		t.Errorf("降级期内的触发不应增加计数（它没改变状态）: %v", got)
	}
}

// TestDegradeGateReset 手动解除。
func TestDegradeGateReset(t *testing.T) {
	var g degradeGate
	g.Trigger()
	if !g.Active() {
		t.Fatal("应已激活")
	}
	g.Reset()
	if g.Active() {
		t.Error("Reset 后不该激活")
	}
	if g.Status()["active"] != false {
		t.Error("状态未反映解除")
	}
	// 解除后可以重新触发
	g.Trigger()
	if !g.Active() {
		t.Error("解除后应可重新触发")
	}
}

// TestDegradeGateZeroValueUsable 零值可直接用（Server 里就是 &degradeGate{}）。
func TestDegradeGateZeroValueUsable(t *testing.T) {
	g := &degradeGate{}
	if g.Active() {
		t.Error("零值不该激活")
	}
	g.Trigger()
	if !g.Active() {
		t.Error("零值触发后应激活")
	}
	// Status 必须可序列化且无 nil map
	if m := g.Status(); m == nil {
		t.Error("Status 不该为 nil")
	}
}

// TestDegradeGateExpiry 过期后自动失效（用一个已过期的 until 模拟）。
func TestDegradeGateExpiry(t *testing.T) {
	g := &degradeGate{until: time.Now().Add(-time.Second)}
	if g.Active() {
		t.Error("已过期的 until 不该报告激活")
	}
	// 过期后触发应设置新的 until
	g.Trigger()
	if !g.Active() {
		t.Error("过期后触发应重新激活")
	}
}
