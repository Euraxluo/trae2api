package server

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"trae2api/internal/traectl"
)

// 本文件实现 **WAF IP 级拦截的 fail-fast 闸**。
//
// 要解决的问题：当上游按**来源 IP**（而不是按账号）拦截时，
// 逐个换号重试是**反效果**的 —— 每换一个号就多打一次上游，请求量被放大，
// 只会让封禁窗口延长。此时正确的动作是**立刻停手**，而不是继续轮转。
//
// 判定难点在哪：单个账号吃 403 有很多原因（该账号被限、该模型无权限、
// 请求不合法），这些都不该触发 IP 级闸。只有**多个不同账号在短窗口内
// 都吃 403** 才说明问题出在共同的那一层 —— 也就是来源 IP。
//
// 所以触发条件是「**不同** UID 数」而非「事件数」：
// 同一账号连续 403 两次是账号问题；两个不同账号各 403 一次才是 IP 问题。
//
// 与账号冷却的关系：**两者独立**。闸只负责终止本轮轮转（省下无谓的上游请求），
// 该账号该记的冷却照记 —— 否则闸一解除，坏账号又会被选中。
//
// 状态是**进程内、重启清零**：IP 封禁是短时现象，没必要持久化
// （持久化反而会让重启后带着一个过期的"已封禁"判断启动）。
type wafIPGate struct {
	mu  sync.Mutex
	cfg wafIPConfig

	// events 是窗口内的 WAF 事件（惰性清理，不启动定时器）。
	events []wafEvent
	// activeUntil 闸的激活截止时间；零值表示未激活。
	activeUntil time.Time
	// lastReason 最近一次激活的原因（供诊断）。
	lastReason string
	// activations 累计激活次数（供诊断）。
	activations int64
	// lastActivatedAt 最近一次激活时刻。
	lastActivatedAt time.Time
}

// wafEvent 是一次 WAF 拦截记录。
type wafEvent struct {
	uid string
	at  time.Time
}

// wafIPConfig 是闸的参数。
type wafIPConfig struct {
	// Window 观察窗口（默认 60s）。
	Window time.Duration
	// MinDistinctUIDs 窗口内触发激活所需的**不同账号**数（默认 2）。
	MinDistinctUIDs int
	// ActiveFor 激活后持续多久（默认 60s）。
	ActiveFor time.Duration
}

// defaultWAFIPConfig 返回默认参数。
//
// 取值理由（对齐 workbuddy2api 的实测结论）：
//   - 窗口 60s：IP 级频控窗口通常是分钟级，太短会漏判、太长会把不相干的请求算进来。
//   - 不同账号 2 个：1 个不够（可能只是单个账号问题），3 个太迟
//     （那时已经多打了两次上游，放大效应已经发生）。
//   - 激活 60s：与观察窗口同量级，让频控窗口滑过即可。
func defaultWAFIPConfig() wafIPConfig {
	return wafIPConfig{Window: 60 * time.Second, MinDistinctUIDs: 2, ActiveFor: 60 * time.Second}
}

func newWAFIPGate(cfg wafIPConfig) *wafIPGate {
	if cfg.Window <= 0 {
		cfg.Window = 60 * time.Second
	}
	if cfg.MinDistinctUIDs <= 0 {
		cfg.MinDistinctUIDs = 2
	}
	if cfg.ActiveFor <= 0 {
		cfg.ActiveFor = 60 * time.Second
	}
	return &wafIPGate{cfg: cfg}
}

// Active 报告闸当前是否处于激活状态（顺带清理过期事件）。
func (g *wafIPGate) Active(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trimLocked(now)
	return now.Before(g.activeUntil)
}

// trimLocked 剔除窗口外的事件（调用方持锁）。
func (g *wafIPGate) trimLocked(now time.Time) {
	if len(g.events) == 0 {
		return
	}
	cutoff := now.Add(-g.cfg.Window)
	out := g.events[:0]
	for _, e := range g.events {
		if e.at.After(cutoff) {
			out = append(out, e)
		}
	}
	g.events = out
}

// NoteWAF 记录一次 WAF 拦截。
//
// 返回 (闸是否激活, 是否本次刚触发激活)。
// justActivated 用于让调用方在日志里只打一次"已激活"而不是每次 403 都打。
func (g *wafIPGate) NoteWAF(uid string, now time.Time) (active bool, justActivated bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.trimLocked(now)
	g.events = append(g.events, wafEvent{uid: uid, at: now})

	// 已激活期内再收到 403：只续期，不重复报"刚激活"
	if now.Before(g.activeUntil) {
		g.activeUntil = now.Add(g.cfg.ActiveFor)
		return true, false
	}

	// 统计窗口内的**不同** UID 数
	distinct := map[string]struct{}{}
	for _, e := range g.events {
		distinct[e.uid] = struct{}{}
	}
	if len(distinct) < g.cfg.MinDistinctUIDs {
		return false, false
	}

	// 触发激活
	g.activeUntil = now.Add(g.cfg.ActiveFor)
	g.lastReason = fmt.Sprintf("%s 内 %d 个不同账号均被 WAF 拦截",
		g.cfg.Window, len(distinct))
	g.activations++
	g.lastActivatedAt = now
	return true, true
}

// Status 返回闸的状态（供 /status 与控制台）。
func (g *wafIPGate) Status() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.trimLocked(now)

	distinct := map[string]struct{}{}
	for _, e := range g.events {
		distinct[e.uid] = struct{}{}
	}
	m := map[string]any{
		"active":             now.Before(g.activeUntil),
		"window_seconds":     int(g.cfg.Window.Seconds()),
		"min_distinct_uids":  g.cfg.MinDistinctUIDs,
		"active_for_seconds": int(g.cfg.ActiveFor.Seconds()),
		"events_in_window":   len(g.events),
		"distinct_uids":      len(distinct),
		"activations_total":  g.activations,
	}
	if rem := time.Until(g.activeUntil); rem > 0 {
		m["active_remain_seconds"] = int(rem.Seconds()) + 1
		m["last_reason"] = g.lastReason
	}
	if !g.lastActivatedAt.IsZero() {
		m["last_activated_at"] = g.lastActivatedAt.Format(time.RFC3339)
	}
	return m
}

// Reset 清零闸状态（控制台"解除封禁"用）。
func (g *wafIPGate) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = nil
	g.activeUntil = time.Time{}
	g.lastReason = ""
}

// ---- WAF 错误识别 ----

// isWAFBlock 判定该错误是否属于"上游按来源 IP 拦截"。
//
// 对 Trae：判定为 HTTP 403。为什么不用关键词匹配：
// 关键词会漏（各 CDN/WAF 的文案千差万别），而 403 是这类拦截唯一稳定的信号；
// 反过来说，把"所有 403"当成 WAF 也是**刻意的保守**——403 本身不是可重试的错误，
// 停在闸上比继续换号打上游更安全。
//
// 注意：本函数**只服务于闸的激活判定**。账号级冷却仍由 noteFailure 按
// 各自的语义处置（403 不冷却账号，因为它不是账号的问题）。
func isWAFBlock(err error) bool {
	var ue *traectl.UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == http.StatusForbidden
	}
	return false
}
