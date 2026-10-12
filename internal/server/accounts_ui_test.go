package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"trae2api/internal/accountstore"
	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/traectl"
	"trae2api/internal/usermgr"
)

// 本文件是契约 A（账号批量动作）的**行为**测试，不是"路由存在"测试。
//
// 验收标准 C9 的原话是"三个端点均返回 200 且**状态真实变化**"，并明确补了一句
// "不能只断言 HTTP 200"。所以这里每一条都断言到 pool 的 Status / 快照文件系统上：
//   - revive：调之前账号确实在冷却/熔断/降权/模型级冷却里，调之后这些全部消失
//   - control：disabled 由 false→true→false
//   - delete：快照文件真的从磁盘上没了，且账号从池里摘掉
//
// 只断言状态码的测试会放过"端点接了但什么都没做"这个最可能的失败形态 ——
// 那正是本轮返工要修的 B1。

// newAccountsTestServer 构造一个最小 Server：账号池 + 快照目录 + 用户表，
// **不配置 API Key 也不建用户**，因此 withAuth 走"单机免密"分支并注入匿名管理员，
// 批量端点可以直接调（与既有 cooldown_test.go 的做法一致）。
func newAccountsTestServer(t *testing.T) (*Server, *accountstore.Store) {
	t.Helper()
	p := pool.New(pool.Options{
		MaxInFlight: 4, BreakerThreshold: 3, BreakerCooldown: time.Minute,
		SoftRateBase: time.Minute, SoftRateMax: time.Hour,
		ModelBase: time.Minute, PickGap: 0,
		DegradeThreshold: 2, DegradeCooldown: time.Minute,
	})
	store := accountstore.New(t.TempDir())
	s := &Server{
		cfg:      &config.Config{APIKey: ""},
		pool:     p,
		users:    usermgr.New(filepath.Join(t.TempDir(), "users.json")),
		accounts: store,
	}
	return s, store
}

// addTestAccount 往池里塞一个账号并返回它的 uid。
func addTestAccount(p *pool.Pool, key, userID string) string {
	prod, _ := traectl.ProductByKey(key)
	a := &pool.Account{
		UID: AccountUID(prod, userID), Realm: RealmOfProduct(key),
		Label: "test " + userID, Product: prod, Owner: "public",
	}
	p.Add(a)
	return a.UID
}

// testECKeyPEM 生成一份**真正可解析**的 EC 私钥 PEM。
// 快照必须过 CredentialSnapshot.Validate（它会 parseECPrivateKey），
// 随手写 "-----BEGIN..." 之类的假 PEM 会被 accountstore.List 当成坏文件跳过，
// 于是删除测试会"通过"在一个根本没被读到的目录上（假绿）。
func testECKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成测试 EC 私钥: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码 EC 私钥: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// writeTestSnapshot 在快照目录里落一份合法快照，返回文件名。
// 文件名规则与 accountstore.snapshotFileName 一致（product-userID.json）。
func writeTestSnapshot(t *testing.T, store *accountstore.Store, key, userID string) string {
	t.Helper()
	priv := testECKeyPEM(t)
	snap := &traectl.CredentialSnapshot{
		Product: key, UserID: userID, Label: "test " + userID,
		Token: "tok-" + userID, RefreshToken: "ref-" + userID,
		DeviceID: "dev-" + userID, MachineID: "mac-" + userID,
		PrivateKeyPEM: priv, PublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nAA==\n-----END PUBLIC KEY-----",
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("序列化快照: %v", err)
	}
	name := key + "-" + userID + ".json"
	if err := os.WriteFile(filepath.Join(store.Dir(), name), raw, 0o600); err != nil {
		t.Fatalf("写快照 %s: %v", name, err)
	}
	return name
}

// doBatch 直接调 handler（不经过 mux），返回响应码与解析后的 JSON。
//
// 用一个**独立的** withAuth 包装来模拟真实链路（匿名 → 管理员身份注入），
// 这样单测不依赖路由表 —— 路由注册另有 TestAccountsBatchRoutesRegistered 专门断言。
func doBatch(t *testing.T, h http.HandlerFunc, body string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/x", rdr)
	w := httptest.NewRecorder()
	h(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是合法 JSON（HTTP %d）: %s", w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

// ptrTime 给 SetRuntimeForTest 的"显式提供才覆盖"语义用。
func ptrTime(t time.Time) *time.Time { return &t }

// statusOf 取某个 uid 的池内状态；不存在则 Fatal。
func statusOf(t *testing.T, s *Server, uid string) pool.Status {
	t.Helper()
	for _, st := range s.pool.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("池内找不到账号 %s", uid)
	return pool.Status{}
}

// numField 从 JSON 里取一个数字字段（JSON 解出来是 float64）。
func numField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("响应缺少字段 %q: %v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("字段 %q 不是数字: %#v", key, v)
	}
	return int(f)
}

func strSlice(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("响应缺少字段 %q: %v", key, m)
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("字段 %q 不是数组: %#v", key, v)
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		s, ok := it.(string)
		if !ok {
			t.Fatalf("字段 %q 里含非字符串: %#v", key, it)
		}
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------------------
// C9-1 revive
// ---------------------------------------------------------------------------

// TestAccountsReviveClearsAllFive 是本轮的**核心断言**：复活必须清五样，
// 且清的是真状态，不是响应里的几个数字。
//
// 五样：账号级冷却 / 熔断 / 已分类连败计数 fails / 连败降权 / 模型级冷却。
//
// 为什么把"只清冷却"这个漏项做成显式断言：ClearCooldown 只覆盖冷却+熔断+模型级，
// **不动** degradeUntil 与 consecutiveFails。如果 handler 图省事只调它，
// 这个测试会在 revived=1、HTTP=200 的情况下失败在 degraded=true 上 ——
// 那正是"点十次复活按钮账号还是不可用"的真实 bug 形态。
func TestAccountsReviveClearsAllFive(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	uid := addTestAccount(s.pool, "trae-cn", "916348159159136")
	a := s.pool.Get(uid)
	if a == nil {
		t.Fatal("账号未入池")
	}

	now := time.Now()
	if !s.pool.SetRuntimeForTest(uid, pool.RuntimeOverride{
		CoolingUntil: ptrTime(now.Add(30 * time.Minute)),
		CoolKind:     pool.CoolHard,
		CoolReason:   "测试：凭证失效",
		BreakerUntil: ptrTime(now.Add(20 * time.Minute)),
		Fails:        2,
		DegradeUntil: ptrTime(now.Add(15 * time.Minute)),
		Consecutive:  7,
		ModelCooling: map[string]time.Time{"glm-5.2": now.Add(10 * time.Minute)},
	}) {
		t.Fatal("SetRuntimeForTest 失败：账号不在池内")
	}

	// 前置：五样**全部**处于"卡住"状态。若前置不成立，后面的断言毫无意义。
	before := statusOf(t, s, uid)
	if !before.Cooling || !before.Breaker || !before.Degraded || before.Fails == 0 {
		t.Fatalf("前置状态不满足（冷却=%v 熔断=%v 降权=%v fails=%d）",
			before.Cooling, before.Breaker, before.Degraded, before.Fails)
	}
	if before.UnclassifiedFails == 0 {
		t.Fatalf("前置连败计数为 0")
	}
	if _, ok := before.ModelCooling["glm-5.2"]; !ok {
		t.Fatalf("前置模型级冷却缺失: %v", before.ModelCooling)
	}

	code, resp := doBatch(t, s.withAuth(s.handleAccountsRevive),
		`{"uids":["`+uid+`"],"all":false}`)
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d want 200，body=%v", code, resp)
	}
	if got := numField(t, resp, "revived"); got != 1 {
		t.Errorf("revived = %d want 1", got)
	}
	if uids := strSlice(t, resp, "uids"); len(uids) != 1 || uids[0] != uid {
		t.Errorf("uids = %v want [%s]", uids, uid)
	}

	// **状态真实变化**：逐样断言，一个都不能还在。
	after := statusOf(t, s, uid)
	if after.Cooling {
		t.Errorf("复活后仍在冷却（cool_remain=%d reason=%q）", after.CoolRemainSec, after.CoolReason)
	}
	if after.Breaker {
		t.Errorf("复活后仍在熔断（breaker_remain=%d）", after.BreakerRemain)
	}
	if after.Degraded || after.DegradeRemain != 0 {
		t.Errorf("复活后仍在降权（degraded=%v remain=%d）—— 典型的‘只清了冷却’漏项",
			after.Degraded, after.DegradeRemain)
	}
	if after.Fails != 0 {
		t.Errorf("复活后已分类连败计数 = %d want 0", after.Fails)
	}
	if after.UnclassifiedFails != 0 {
		t.Errorf("复活后未分类连败计数 = %d want 0", after.UnclassifiedFails)
	}
	if len(after.ModelCooling) != 0 {
		t.Errorf("复活后模型级冷却未清: %v —— 这是最常见的漏项", after.ModelCooling)
	}
	if !after.Healthy {
		t.Errorf("复活后账号仍不 healthy")
	}
	// 账号必须真的能被选中（前四样是"资格"，这一条是"结果"）
	if got := s.pool.Pick("trae", "glm-5.2", nil, nil); got == nil || got.UID != uid {
		t.Errorf("复活后账号仍选不中，Pick = %v", got)
	}

	// cleared 明细：五样各记 1 次
	cleared, ok := resp["cleared"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 cleared 明细: %v", resp)
	}
	for _, k := range []string{"cooldown", "breaker", "fails", "degrade", "model_cooldown", "unclassified_fails"} {
		v, ok := cleared[k].(float64)
		if !ok || int(v) != 1 {
			t.Errorf("cleared[%s] = %v want 1", k, cleared[k])
		}
	}
}

// TestAccountsReviveAllAndMissing revive 的 all=true 分支与"不在池内"的 uid。
//
// 关键：不存在的 uid 不能把整批请求打成失败（批量动作里混进一个已删账号是常态），
// 也不能被算进 revived（否则前端会显示"复活了 3 个"而其实只有 2 个）。
func TestAccountsReviveAllAndMissing(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "u1")
	u2 := addTestAccount(s.pool, "solo-cn", "u2")
	for _, uid := range []string{u1, u2} {
		s.pool.Cooldown(uid, pool.CoolSoft, time.Hour, "测试预置")
		if !statusOf(t, s, uid).Cooling {
			t.Fatalf("%s 预置冷却失败", uid)
		}
	}

	code, resp := doBatch(t, s.withAuth(s.handleAccountsRevive), `{"all":true}`)
	if code != http.StatusOK {
		t.Fatalf("all=true HTTP = %d want 200", code)
	}
	if got := numField(t, resp, "revived"); got != 2 {
		t.Errorf("revived = %d want 2", got)
	}
	for _, uid := range []string{u1, u2} {
		if statusOf(t, s, uid).Cooling {
			t.Errorf("%s 复活后仍在冷却", uid)
		}
	}

	// 混一个不存在的 uid
	code, resp = doBatch(t, s.withAuth(s.handleAccountsRevive),
		`{"uids":["trae-cn-不存在的账号","`+u1+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("含缺失 uid 时 HTTP = %d want 200", code)
	}
	if got := numField(t, resp, "revived"); got != 1 {
		t.Errorf("revived = %d want 1（缺失的 uid 不该计入）", got)
	}
	if missing := strSlice(t, resp, "missing"); len(missing) != 1 {
		t.Errorf("missing = %v want 1 项", missing)
	}
}

// TestAccountsReviveBadRequest 空目标必须 400（而不是"成功了 0 个"）。
func TestAccountsReviveBadRequest(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	addTestAccount(s.pool, "trae-cn", "u1")
	for _, body := range []string{`{}`, `{"uids":[],"all":false}`, `{"uids":["  "]}`} {
		code, resp := doBatch(t, s.withAuth(s.handleAccountsRevive), body)
		if code != http.StatusBadRequest {
			t.Errorf("body=%s HTTP = %d want 400 (resp=%v)", body, code, resp)
		}
	}
	code, _ := doBatch(t, s.withAuth(s.handleAccountsRevive), `{"uids":[`)
	if code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，得到 %d", code)
	}
}

// ---------------------------------------------------------------------------
// C9-2 control
// ---------------------------------------------------------------------------

// TestAccountsControlDisableEnable 批量启停，断言 disabled 真的翻转。
func TestAccountsControlDisableEnable(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "u1")
	u2 := addTestAccount(s.pool, "solo-cn", "u2")

	for _, uid := range []string{u1, u2} {
		if statusOf(t, s, uid).Disabled {
			t.Fatalf("%s 前置状态不该是 disabled", uid)
		}
	}

	body := `{"uids":["` + u1 + `","` + u2 + `"],"all":false,"action":"disable"}`
	code, resp := doBatch(t, s.withAuth(s.handleAccountsControl), body)
	if code != http.StatusOK {
		t.Fatalf("disable HTTP = %d want 200，resp=%v", code, resp)
	}
	if got := numField(t, resp, "changed"); got != 2 {
		t.Errorf("changed = %d want 2", got)
	}
	if act, _ := resp["action"].(string); act != "disable" {
		t.Errorf("action = %q want disable", act)
	}
	if got := strSlice(t, resp, "uids"); len(got) != 2 {
		t.Errorf("uids = %v want 2 项", got)
	}
	for _, uid := range []string{u1, u2} {
		st := statusOf(t, s, uid)
		if !st.Disabled {
			t.Errorf("%s disable 后 disabled=false —— 状态没变", uid)
		}
		if st.DisableReason == "" {
			t.Errorf("%s disable 后没有记录原因", uid)
		}
	}

	// 停用的账号不得被选中
	if got := s.pool.Pick("trae", "glm-5.2", nil, nil); got != nil {
		t.Errorf("停用后仍被选中: %v", got.UID)
	}

	// 再批量启用
	body = `{"uids":["` + u1 + `","` + u2 + `"],"all":false,"action":"enable"}`
	code, resp = doBatch(t, s.withAuth(s.handleAccountsControl), body)
	if code != http.StatusOK {
		t.Fatalf("enable HTTP = %d want 200", code)
	}
	if got := numField(t, resp, "changed"); got != 2 {
		t.Errorf("changed = %d want 2", got)
	}
	for _, uid := range []string{u1, u2} {
		if statusOf(t, s, uid).Disabled {
			t.Errorf("%s enable 后 disabled=true —— 状态没变", uid)
		}
	}
}

// TestAccountsControlAll 与 all=true（批量启停全部）。
func TestAccountsControlAll(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "u1")
	u2 := addTestAccount(s.pool, "solo-cn", "u2")
	code, resp := doBatch(t, s.withAuth(s.handleAccountsControl), `{"all":true,"action":"disable"}`)
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d want 200", code)
	}
	if got := numField(t, resp, "changed"); got != 2 {
		t.Errorf("changed = %d want 2", got)
	}
	for _, uid := range []string{u1, u2} {
		if !statusOf(t, s, uid).Disabled {
			t.Errorf("%s 未被批量停用", uid)
		}
	}
}

// TestAccountsControlBadRequest action 非法 / 缺目标必须 400。
func TestAccountsControlBadRequest(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "u1")
	cases := []string{
		`{"uids":["` + u1 + `"],"action":"revive"}`, // 动作名不在契约里
		`{"uids":["` + u1 + `"]}`,                   // 缺 action
		`{"uids":["` + u1 + `"],"action":""}`,
		`{"all":false,"action":"disable"}`, // 缺目标
	}
	for _, body := range cases {
		code, resp := doBatch(t, s.withAuth(s.handleAccountsControl), body)
		if code != http.StatusBadRequest {
			t.Errorf("body=%s HTTP = %d want 400 (resp=%v)", body, code, resp)
		}
	}
	// 合法性校验不得留下副作用
	if statusOf(t, s, u1).Disabled {
		t.Error("非法请求不该改变 disabled")
	}
}

// ---------------------------------------------------------------------------
// C9-3 delete
// ---------------------------------------------------------------------------

// TestAccountsDeleteRemovesSnapshot 批量删除必须真的把快照文件从磁盘删掉，
// 并把账号从池里摘掉（否则"删了的账号"下一轮还会被选中）。
func TestAccountsDeleteRemovesSnapshot(t *testing.T) {
	s, store := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "u1")
	u2 := addTestAccount(s.pool, "solo-cn", "u2")
	f1 := writeTestSnapshot(t, store, "trae-cn", "u1")
	f2 := writeTestSnapshot(t, store, "solo-cn", "u2")
	// 快照必须真的能被 store 读出来（证明测试夹具合法，不是被跳过的坏文件）
	if entries, skipped := store.List(); len(entries) != 2 || len(skipped) != 0 {
		t.Fatalf("夹具无效: entries=%d skipped=%v", len(entries), skipped)
	}

	body := `{"uids":["` + u1 + `"],"all":false}`
	code, resp := doBatch(t, s.withAuth(s.handleAccountsDelete), body)
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d want 200，resp=%v", code, resp)
	}
	if got := numField(t, resp, "deleted"); got != 1 {
		t.Errorf("deleted = %d want 1", got)
	}
	if got := strSlice(t, resp, "uids"); len(got) != 1 || got[0] != u1 {
		t.Errorf("uids = %v want [%s]", got, u1)
	}
	if _, ok := resp["errors"]; !ok {
		t.Errorf("响应缺少 errors 字段（契约要求存在，前端读 r.errors.length）: %v", resp)
	}
	if errs, ok := resp["errors"].([]any); !ok || len(errs) != 0 {
		t.Errorf("errors 应为空数组，得到 %#v", resp["errors"])
	}

	// **状态真实变化**：文件没了、池里没了、另一个还在
	if _, err := os.Stat(filepath.Join(store.Dir(), f1)); !os.IsNotExist(err) {
		t.Errorf("快照 %s 仍在磁盘上（err=%v）", f1, err)
	}
	if s.pool.Get(u1) != nil {
		t.Errorf("账号 %s 仍在池内", u1)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), f2)); err != nil {
		t.Errorf("不该动到 %s: %v", f2, err)
	}
	if s.pool.Get(u2) == nil {
		t.Errorf("不该动到账号 %s", u2)
	}
	// 渠道表也必须摘掉（否则 Pick 还能选到它）
	for _, ch := range s.channels {
		ch.mu.RLock()
		_, still := ch.accounts[u1]
		ch.mu.RUnlock()
		if still {
			t.Errorf("渠道 %s 仍持有已删除账号", ch.Realm)
		}
	}
}

// TestAccountsDeleteAll 批量删除全部勾选（含"本来就不存在"的幂等语义与报错回报）。
func TestAccountsDeleteAll(t *testing.T) {
	s, store := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "u1")
	u2 := addTestAccount(s.pool, "solo-cn", "u2")
	writeTestSnapshot(t, store, "trae-cn", "u1")
	writeTestSnapshot(t, store, "solo-cn", "u2")

	code, resp := doBatch(t, s.withAuth(s.handleAccountsDelete), `{"uids":["`+u1+`","`+u2+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d want 200", code)
	}
	if got := numField(t, resp, "deleted"); got != 2 {
		t.Errorf("deleted = %d want 2", got)
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Errorf("快照目录应清空，仍剩 %d 份", len(left))
	}
	if n := len(s.pool.List()); n != 0 {
		t.Errorf("池应清空，仍有 %d 个账号", n)
	}

	// 对同一个 uid 再删一次：幂等成功（目标状态已达成），计 deleted 并记进 missing
	code, resp = doBatch(t, s.withAuth(s.handleAccountsDelete), `{"uids":["`+u1+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("重复删除 HTTP = %d want 200（幂等）", code)
	}
	if got := numField(t, resp, "deleted"); got != 1 {
		t.Errorf("重复删除 deleted = %d want 1（幂等）", got)
	}
	if got := strSlice(t, resp, "missing"); len(got) != 1 {
		t.Errorf("missing = %v want 1 项（说明该账号本来就不存在）", got)
	}
}

// TestAccountsDeleteErrorReporting 池内有账号但**没有快照文件**时必须报错，
// 而不是假装删成功。
//
// 场景：账号来自 config.trae.accounts（指向客户端 storage.json），没有快照。
// 删快照端点删不掉它 —— 静默返回 deleted=1 会让用户以为账号已经处理掉了。
func TestAccountsDeleteErrorReporting(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	u1 := addTestAccount(s.pool, "trae-cn", "no-snapshot")
	code, resp := doBatch(t, s.withAuth(s.handleAccountsDelete), `{"uids":["`+u1+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d want 200（部分成功也要 200）", code)
	}
	if got := numField(t, resp, "deleted"); got != 0 {
		t.Errorf("deleted = %d want 0", got)
	}
	raw, ok := resp["errors"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("errors 应含 1 条失败: %v", resp["errors"])
	}
	item, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("errors[0] 不是对象: %#v", raw[0])
	}
	if item["uid"] != u1 {
		t.Errorf("errors[0].uid = %v want %s", item["uid"], u1)
	}
	if msg, _ := item["error"].(string); msg == "" {
		t.Error("errors[0] 缺少原因说明")
	}
	// 池内注册必须保留（快照还在就意味着这个账号还能用）
	if s.pool.Get(u1) == nil {
		t.Error("删除失败时不该把账号从池里摘掉")
	}
}

// TestAccountsDeleteBadRequest 空目标 400。
func TestAccountsDeleteBadRequest(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	code, resp := doBatch(t, s.withAuth(s.handleAccountsDelete), `{"uids":[],"all":false}`)
	if code != http.StatusBadRequest {
		t.Errorf("HTTP = %d want 400 (resp=%v)", code, resp)
	}
}

// ---------------------------------------------------------------------------
// 路由注册
// ---------------------------------------------------------------------------

// TestAccountsBatchRoutesRegistered 三条路由必须真的注册在真实 mux 上。
//
// 为什么单独测：本轮返工修的 B1 就是"handler 写了但没注册"这一类
// （上一轮更极端：连 handler 都没有，实测三个端点全是 405）。
// 405 是因为 DELETE /admin/accounts/{name} 把 revive 当成了路径参数，
// 所以"404 之外的状态码"不足以证明注册成功 —— 必须**明确排除 405**。
func TestAccountsBatchRoutesRegistered(t *testing.T) {
	s, _ := newAccountsTestServer(t)
	addTestAccount(s.pool, "trae-cn", "u1")
	h := s.Handler()

	for _, path := range []string{
		"/admin/accounts/revive", "/admin/accounts/control", "/admin/accounts/delete",
	} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"all":true,"action":"disable"}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Errorf("POST %s → 404：路由未注册", path)
		}
		if w.Code == http.StatusMethodNotAllowed {
			t.Errorf("POST %s → 405：方法/路径没匹配上（{name} 通配把它吃了？）", path)
		}
		if w.Code != http.StatusOK {
			t.Errorf("POST %s → %d want 200，body=%s", path, w.Code, w.Body.String())
		}
	}

	// 既有路由不得被这三条挤掉（{name} 的 DELETE 必须仍然可达）
	req := httptest.NewRequest(http.MethodDelete, "/admin/accounts/definitely-missing.json", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("DELETE /admin/accounts/{name} 对不存在的快照应 404，得到 %d", w.Code)
	}
}
