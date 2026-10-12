package pool

import (
	"path/filepath"
	"testing"
	"time"
)

// newDegradePool 构造一个连败阈值很小的池，便于测试。
func newDegradePool(t *testing.T, threshold int, cooldown time.Duration) *Pool {
	t.Helper()
	p := New(Options{
		MaxInFlight: 1, BreakerThreshold: 100, // 熔断阈值设高，避免干扰
		BreakerCooldown: time.Minute,
		SoftRateBase:    50 * time.Millisecond, SoftRateMax: time.Second,
		ModelBase: 50 * time.Millisecond, PickGap: 0,
		DegradeThreshold: threshold, DegradeCooldown: cooldown,
	})
	p.Add(&Account{UID: "a1", Realm: "trae", Owner: "public", Label: "L"})
	return p
}

func st(t *testing.T, p *Pool) Status {
	t.Helper()
	for _, s := range p.List() {
		if s.UID == "a1" {
			return s
		}
	}
	t.Fatal("找不到账号")
	return Status{}
}

// TestDegradeBelowThresholdNoEffect 未达阈值的连败**不该**影响可用性。
//
// 这是"不因一次网络抖动罚号"的保证：单次失败只记数，账号照常可选。
func TestDegradeBelowThresholdNoEffect(t *testing.T) {
	p := newDegradePool(t, 5, time.Minute)
	for i := 0; i < 4; i++ {
		if p.NoteFailures("a1") {
			t.Fatalf("第 %d 次（阈值 5）不该触发降权", i+1)
		}
	}
	if s := st(t, p); s.Degraded {
		t.Error("未达阈值不该降权")
	}
	if got := p.Pick("trae", "", nil, nil); got == nil {
		t.Error("未达阈值时账号应仍可选中")
	}
	if s := st(t, p); s.UnclassifiedFails != 4 {
		t.Errorf("连败计数 = %d want 4", s.UnclassifiedFails)
	}
}

// TestDegradeAtThreshold 达阈后临时出池。
func TestDegradeAtThreshold(t *testing.T) {
	p := newDegradePool(t, 3, time.Minute)
	if p.NoteFailures("a1") {
		t.Fatal("第 1 次不该触发")
	}
	if p.NoteFailures("a1") {
		t.Fatal("第 2 次不该触发")
	}
	if !p.NoteFailures("a1") {
		t.Fatal("第 3 次（达阈）应触发降权")
	}
	s := st(t, p)
	if !s.Degraded {
		t.Error("状态未反映降权")
	}
	if s.DegradeRemain <= 0 {
		t.Errorf("降权剩余秒数应为正: %d", s.DegradeRemain)
	}
	// 出池：选不到
	if got := p.Pick("trae", "", nil, nil); got != nil {
		t.Errorf("降权期内不该被选中，得到 %v", got.UID)
	}
	// 计数已清零（供下一轮重新累计）
	if s.UnclassifiedFails != 0 {
		t.Errorf("达阈后计数应清零，实际 %d", s.UnclassifiedFails)
	}
}

// TestDegradeDoesNotExtend 降权期内再次达阈**不延长**。
//
// 否则用户的重试会把降权越堆越厚，最终账号被无限期排除。
func TestDegradeDoesNotExtend(t *testing.T) {
	p := newDegradePool(t, 2, 2*time.Second)
	p.NoteFailures("a1")
	p.NoteFailures("a1") // 触发

	// 记下当前的截止时刻
	p.mu.RLock()
	first := p.byUID["a1"].degradeUntil
	p.mu.RUnlock()
	if first.IsZero() {
		t.Fatal("未设置降权截止")
	}

	// 再连续达阈多次
	for i := 0; i < 10; i++ {
		p.NoteFailures("a1")
		p.NoteFailures("a1")
	}
	p.mu.RLock()
	after := p.byUID["a1"].degradeUntil
	p.mu.RUnlock()
	if !after.Equal(first) {
		t.Errorf("降权期内重复达阈不该延长：%v → %v", first, after)
	}
}

// TestDegradeClearedBySuccess 成功必须清零连败与降权。
func TestDegradeClearedBySuccess(t *testing.T) {
	p := newDegradePool(t, 2, time.Minute)
	p.NoteFailures("a1")
	p.NoteFailures("a1") // 触发降权
	if !st(t, p).Degraded {
		t.Fatal("应已降权")
	}

	p.NoteSuccess("a1")
	s := st(t, p)
	if s.Degraded {
		t.Error("成功后应清除降权")
	}
	if s.UnclassifiedFails != 0 {
		t.Errorf("成功后连败计数应清零，实际 %d", s.UnclassifiedFails)
	}
	if got := p.Pick("trae", "", nil, nil); got == nil {
		t.Error("成功后应可选中")
	}
}

// TestDegradeDisabledByNegativeThreshold 阈值设成负数表示禁用。
func TestDegradeDisabledByNegativeThreshold(t *testing.T) {
	p := newDegradePool(t, -1, time.Minute)
	for i := 0; i < 100; i++ {
		if p.NoteFailures("a1") {
			t.Fatal("禁用时不该触发降权")
		}
	}
	if st(t, p).Degraded {
		t.Error("禁用时不该降权")
	}
}

// TestDegradeDurationClamped 降权时长被钳到上限（防配置写了个离谱的值把账号永久排除）。
func TestDegradeDurationClamped(t *testing.T) {
	p := New(Options{
		MaxInFlight: 1, BreakerThreshold: 100,
		DegradeThreshold: 1,
		// 故意写一个远超上限的值
		DegradeCooldown: 100 * 24 * time.Hour,
	})
	p.Add(&Account{UID: "a1", Realm: "trae", Owner: "public"})
	if got := p.DegradeCooldown(); got > degradeCooldownMax {
		t.Errorf("降权时长 %v 超过上限 %v（会把账号永久排除）", got, degradeCooldownMax)
	}
	// 默认值也要合理
	p2 := New(Options{MaxInFlight: 1})
	if got := p2.DegradeCooldown(); got != defaultDegradeCooldown {
		t.Errorf("默认降权时长 = %v want %v", got, defaultDegradeCooldown)
	}
}

// TestDegradePersistRoundTrip 降权状态跨重启保留。
//
// 不持久化的话，重启会让"持续坏掉的账号"立刻回到池里，连败兜底形同虚设。
func TestDegradePersistRoundTrip(t *testing.T) {
	f := filepath.Join(t.TempDir(), "state.json")
	p := New(Options{
		MaxInFlight: 1, BreakerThreshold: 100,
		DegradeThreshold: 2, DegradeCooldown: time.Hour,
		StateFile: f,
	})
	p.Add(&Account{UID: "a1", Realm: "trae", Owner: "public"})
	p.NoteFailures("a1")
	p.NoteFailures("a1") // 触发
	if !st(t, p).Degraded {
		t.Fatal("应已降权")
	}
	p.NoteFailures("a1") // 留一个未达阈的计数
	if err := p.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 恢复到新池
	p2 := New(Options{
		MaxInFlight: 1, BreakerThreshold: 100,
		DegradeThreshold: 2, DegradeCooldown: time.Hour,
		StateFile: f,
	})
	p2.Add(&Account{UID: "a1", Realm: "trae", Owner: "public"})
	if err := p2.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	s := st(t, p2)
	if !s.Degraded {
		t.Error("降权状态未恢复（重启后坏账号会立刻回池）")
	}
	if s.UnclassifiedFails != 1 {
		t.Errorf("连败计数未恢复: %d want 1", s.UnclassifiedFails)
	}
	if got := p2.Pick("trae", "", nil, nil); got != nil {
		t.Error("恢复后降权期内不该被选中")
	}
}

// TestDegradeCoexistsWithCooldown 降权与冷却**并列取更长者**，不叠加。
func TestDegradeCoexistsWithCooldown(t *testing.T) {
	p := newDegradePool(t, 1, 10*time.Second)
	// 先触发一个更长的冷却
	p.Cooldown("a1", CoolSoft, time.Hour, "test")
	// 再触发一个更短的降权
	p.NoteFailures("a1")

	s := st(t, p)
	if !s.Cooling {
		t.Error("冷却应仍在")
	}
	if !s.Degraded {
		t.Error("降权也应记录（两者独立记录，取更长者生效）")
	}
	// 清掉冷却后，降权仍应挡住（说明它独立生效）
	p.ClearCooldown("a1")
	if got := p.Pick("trae", "", nil, nil); got != nil {
		t.Error("清掉冷却后降权仍应生效（更短的降权不该被更长的冷却掩盖掉存在）")
	}
}
