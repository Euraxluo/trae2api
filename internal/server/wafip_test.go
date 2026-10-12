package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"trae2api/internal/pool"
	"trae2api/internal/traectl"
)

func testGate() *wafIPGate {
	return newWAFIPGate(wafIPConfig{
		Window: 60 * time.Second, MinDistinctUIDs: 2, ActiveFor: 60 * time.Second,
	})
}

// TestSingleUIDDoesNotActivate 是本题最重要的一条：
// **同一账号**反复被 403 **不该**激活 IP 级闸。
//
// 理由：单个账号反复吃 403 说明是那个账号的问题（被限、被封）；
// 只有**多个不同账号**在同一短窗口内都吃 403，才说明问题出在共同的那一层
// —— 也就是来源 IP。搞错这一点会把账号级问题误判成 IP 被封，
// 进而把整个池的健康账号都停掉。
func TestSingleUIDDoesNotActivate(t *testing.T) {
	g := testGate()
	now := time.Now()
	for i := 0; i < 10; i++ {
		active, justActivated := g.NoteWAF("acct-A", now.Add(time.Duration(i)*time.Second))
		if active || justActivated {
			t.Fatalf("第 %d 次同一账号 403 不应激活闸", i+1)
		}
	}
	if g.Active(now.Add(10 * time.Second)) {
		t.Error("同一账号连续 403 后闸仍不应激活")
	}
}

// TestTwoDistinctUIDsActivate 两个**不同**账号在窗口内都对上 403 → 激活。
func TestTwoDistinctUIDsActivate(t *testing.T) {
	g := testGate()
	now := time.Now()

	active, just := g.NoteWAF("acct-A", now)
	if active || just {
		t.Fatal("第一个账号不应激活")
	}
	active, just = g.NoteWAF("acct-B", now.Add(5*time.Second))
	if !active || !just {
		t.Fatal("第二个不同账号应激活闸")
	}
	if !g.Active(now.Add(10 * time.Second)) {
		t.Error("激活后 Active 应为 true")
	}
}

// TestActivationExpires 激活是有时限的。
func TestActivationExpires(t *testing.T) {
	g := newWAFIPGate(wafIPConfig{Window: time.Minute, MinDistinctUIDs: 2, ActiveFor: 10 * time.Second})
	now := time.Now()
	g.NoteWAF("a", now)
	g.NoteWAF("b", now.Add(time.Second))
	if !g.Active(now.Add(5 * time.Second)) {
		t.Fatal("窗口内应激活")
	}
	if g.Active(now.Add(20 * time.Second)) {
		t.Error("超过 ActiveFor 后应自动失效")
	}
}

// TestEventsOutsideWindowIgnored 窗口外的事件不参与判定。
func TestEventsOutsideWindowIgnored(t *testing.T) {
	g := newWAFIPGate(wafIPConfig{Window: 10 * time.Second, MinDistinctUIDs: 2, ActiveFor: time.Minute})
	base := time.Now()
	g.NoteWAF("a", base)
	// 20 秒后再来第二个账号：第一个事件已出窗 → 不应激活
	active, _ := g.NoteWAF("b", base.Add(20*time.Second))
	if active {
		t.Error("窗口外的事件不应参与判定")
	}
}

// TestRepeatDuringActiveExtendsButNotReannounce 激活期内再收到 403：续期但不再报"刚激活"。
//
// 为什么需要：日志里每次 403 都打一行"已激活"会刷屏，掩盖真正有用的信息。
func TestRepeatDuringActiveExtendsButNotReannounce(t *testing.T) {
	g := newWAFIPGate(wafIPConfig{Window: time.Minute, MinDistinctUIDs: 2, ActiveFor: 30 * time.Second})
	now := time.Now()
	g.NoteWAF("a", now)
	g.NoteWAF("b", now)

	// 20 秒后再来一次：应续期到 20+30=50s，但不报 justActivated
	active, just := g.NoteWAF("c", now.Add(20*time.Second))
	if !active {
		t.Fatal("激活期内应保持 active")
	}
	if just {
		t.Error("激活期内不应重复报 justActivated")
	}
	// 续期生效：40 秒时仍在激活（原 30s 已过）
	if !g.Active(now.Add(40 * time.Second)) {
		t.Error("激活期内的 403 应延长激活窗口")
	}
}

// TestReset 手动解除。
func TestReset(t *testing.T) {
	g := testGate()
	now := time.Now()
	g.NoteWAF("a", now)
	g.NoteWAF("b", now)
	if !g.Active(now) {
		t.Fatal("应已激活")
	}
	g.Reset()
	if g.Active(time.Now()) {
		t.Error("Reset 后应失效")
	}
	// 重置后事件也清空 → 需要重新凑够不同账号才能再激活
	active, _ := g.NoteWAF("a", time.Now())
	if active {
		t.Error("Reset 后单个账号不应立即激活")
	}
}

// TestStatus 状态自述完整（供 /status 与控制台判断"离触发有多近"）。
func TestStatus(t *testing.T) {
	g := testGate()
	now := time.Now()
	g.NoteWAF("a", now)
	st := g.Status()
	if st["active"] != false {
		t.Error("尚未激活")
	}
	if n, ok := st["activations_total"].(int64); !ok || n != 0 {
		t.Errorf("激活次数应为 0，得到 %v(%T)", st["activations_total"], st["activations_total"])
	}
	if n, ok := st["events_in_window"].(int); !ok || n != 1 {
		t.Errorf("events_in_window = %v", st["events_in_window"])
	}
	if n, ok := st["distinct_uids"].(int); !ok || n != 1 {
		t.Errorf("distinct_uids = %v", st["distinct_uids"])
	}
	if n, ok := st["min_distinct_uids"].(int); !ok || n != 2 {
		t.Errorf("阈值未透出: %v", st["min_distinct_uids"])
	}

	g.NoteWAF("b", now)
	st = g.Status()
	if st["active"] != true {
		t.Error("应已激活")
	}
	if n, ok := st["activations_total"].(int64); !ok || n != 1 {
		t.Errorf("激活次数 = %v want 1", st["activations_total"])
	}
	if _, ok := st["active_remain_seconds"]; !ok {
		t.Error("激活时应透出剩余秒数")
	}
	if st["last_reason"] == "" {
		t.Error("激活时应透出原因")
	}
}

// TestIsWAFBlock 只认 403；其他状态码不算 WAF。
func TestIsWAFBlock(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusForbidden, true},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, false}, // 429 是账号级限流，不是 IP 级封禁
		{http.StatusBadGateway, false},
		{http.StatusBadRequest, false},
	}
	for _, c := range cases {
		err := &traectl.UpstreamError{Status: c.status, Body: `{"code":0}`}
		if got := isWAFBlock(err); got != c.want {
			t.Errorf("isWAFBlock(HTTP %d) = %v want %v", c.status, got, c.want)
		}
	}
	// 包装过的也要能识别
	wrapped := fmt.Errorf("上层: %w", &traectl.UpstreamError{Status: http.StatusForbidden})
	if !isWAFBlock(wrapped) {
		t.Error("包装后的 403 应被识别")
	}
	// 非上游错误
	if isWAFBlock(errors.New("boom")) {
		t.Error("普通错误不应被判为 WAF")
	}
	if isWAFBlock(nil) {
		t.Error("nil 不应被判为 WAF")
	}
}

// TestWAFDoesNotCoolAccount 403 **不冷却账号也不冷却模型**。
//
// 理由：403 既不是账号的问题、也不是模型的问题，而是来源 IP 被拦。
// 在这里冷却只会污染池状态 —— 该由 IP 级闸负责。
func TestWAFDoesNotCoolAccount(t *testing.T) {
	s, a := newTestServer(t)
	ch := &Channel{Realm: "trae"}
	err := &traectl.UpstreamError{Status: http.StatusForbidden, Body: "Forbidden: request blocked by WAF"}
	s.noteFailure(ch, a, "glm-5.2", err)

	st := stOf(t, s)
	if st.Cooling {
		t.Error("403 不应冷却账号")
	}
	if len(st.ModelCooling) != 0 {
		t.Errorf("403 不应冷却模型: %v", st.ModelCooling)
	}
	if st.Fails != 0 {
		t.Errorf("403 不应记失败计数: %d", st.Fails)
	}
	if !st.Healthy {
		t.Error("403 后账号应仍为健康（问题不在账号）")
	}
}

// TestWAFGateIntegration 闸与池的协作：激活后不再取号。
//
// 这里直接验证闸的语义（取号循环的集成由 E2E 覆盖）：
// 激活期间 Active() 为 true，调用方据此 break。
func TestWAFGateIntegration(t *testing.T) {
	g := testGate()
	now := time.Now()
	g.NoteWAF("a", now)
	g.NoteWAF("b", now)

	p := pool.New(pool.Options{MaxInFlight: 1})
	p.Add(&pool.Account{UID: "c", Realm: "trae", Owner: "public"})

	// 闸激活时，调用方应跳过取号 —— 这里模拟该判断
	picked := 0
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && g.Active(time.Now()) {
			break
		}
		if p.Pick("trae", "m", nil, nil) != nil {
			picked++
		}
	}
	if picked != 1 {
		t.Errorf("闸激活后应只取号 1 次（首轮），实际 %d 次", picked)
	}
}
