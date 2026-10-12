package server

import (
	"sync"
	"time"
)

// 本文件实现**提示词降级闸**。
//
// 要解决的问题：上游的内容策略会拦截某些 system 提示词（多为指纹误报）。
// 一旦命中，请求返回 400；若每次都"先撞 400 再用降级提示词重试"，
// 那每个请求都要白挨一次失败 —— 既慢又浪费额度。
//
// 所以加一道闸：**一旦确认被拦，就直接切到中性提示词，直到次日零点**。
// 降级期内的请求不再先撞 400。
//
// 定位：**误报处理**，不是对抗框架。拦截多为 system 来源的指纹误报，
// 换一段最小中性提示词即可绕开。
type degradeGate struct {
	mu    sync.Mutex
	until time.Time
	// triggeredAt 最近一次触发时刻（诊断用）。
	triggeredAt time.Time
	// count 累计触发次数（诊断用）。
	count int64
}

// Active 报告当前是否处于降级期。
func (g *degradeGate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Trigger 触发降级，直到次日 00:00（+08:00）。
//
// ⚠️ **已在降级期内则不续期** —— 保持"最早触发点的零点重置"语义。
// 否则连续误报会把降级期无限延长，变成"永远在用降级提示词"，
// 而用户完全不知道为什么自己的 system 提示词失效了。
func (g *degradeGate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if !now.Before(g.until) {
		g.until = nextMidnightCST(now)
		g.triggeredAt = now
		g.count++
	}
}

// Reset 手动解除降级（控制台用）。
func (g *degradeGate) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Time{}
}

// Status 返回降级状态（供 /status 与控制台）。
func (g *degradeGate) Status() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	m := map[string]any{
		"active":          now.Before(g.until),
		"triggered_total": g.count,
	}
	if now.Before(g.until) {
		m["until"] = g.until.Format(time.RFC3339)
		m["remain_seconds"] = int(time.Until(g.until).Seconds()) + 1
	}
	if !g.triggeredAt.IsZero() {
		m["last_triggered_at"] = g.triggeredAt.Format(time.RFC3339)
	}
	return m
}

// nextMidnightCST 返回 now 之后最近的 +08:00 零点。
//
// ⚠️ 用 `time.FixedZone("CST", 8*3600)` 而**不是** `time.LoadLocation("Asia/Shanghai")`：
// 容器与精简发行版里常常没有 tzdata，LoadLocation 会失败并回落到 UTC ——
// 于是"次日零点"会算错 8 小时，降级期在用户看来是随机结束的。
// 固定偏移不依赖任何外部数据。
//
// 边界语义（纯函数，便于单测）：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → **次日** 00:00（刚过零点，下一个零点是明天）
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	// 循环而不是"加一天"：处理 now 恰好是 00:00:00 的边界
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
