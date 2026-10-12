package server

import (
	"testing"
	"time"

	"trae2api/internal/pool"
	"trae2api/internal/realm"
	"trae2api/internal/traectl"
)

// 本文件对应 plan.md 的 C9「同邮箱跨渠道不互相覆盖」。
//
// ⚠️ 先读这条结论（完整取证过程见
// .dsh-team/trae2api-gaps/work/account-identity/notes.md）：
//
//	Trae 上游凭据里**没有**登录渠道（Google / Twitter / 邮箱密码）字段，
//	所以「按登录渠道区分同一邮箱」在本项目的上游数据里**无法实现**。
//	本仓库实际可用的唯一渠道维度是 Product.Key（trae-cn / solo-cn / …），
//	而它已经通过 AccountUID 并进了 UID。
//
// 因此本文件断言的是**这个真实存在的机制**，而不是编造一个上游不存在的渠道
// 字段来假装实现了渠道区分。这正是「同邮箱不互相覆盖」在本项目的真实落点：
// 同一个 userId 在两个产品下拿到两个不同 UID，池里是两个独立账号。
//
// 为什么放在 internal/server 而不是 internal/pool：
// AccountUID 属于 package server，而 package server **已经 import package pool**
// （channel.go / chat.go / …）。若把本测试放进 package pool 去调 server.AccountUID，
// 就形成 pool → server → pool 的 import cycle，根本无法编译。所以这个测试
// 只能在 server 侧，pool_test.go 里的那份同义反复已随之删除。
//
// 本文件是**包内**测试，直接调用生产函数 AccountUID / RealmOfProduct，
// 不复制它们的实现逻辑 —— 因此实现一变，这里的断言就会变红。

// identityUserID 是实测中 Trae CN 与 TRAE SOLO CN 在本机的同一个 userId。
//
// 刻意用固定字面量而不是去读本机凭据：测试必须能在**没装 Trae 的 CI 机器**上
// 通过，不能依赖 %APPDATA% 里存在真实登录态。
const identityUserID = "916348159159136"

// identityProduct 取一个**已注册**的产品；取不到就直接失败。
//
// 不用 ProductByKeyOrZero：零值 Product 的 Key 为空串，AccountUID 会退化成
// 只有 userId（正好是 C9 要防的那个 bug），测试会因此失去意义却仍然"绿"。
func identityProduct(t *testing.T, key string) traectl.Product {
	t.Helper()
	p, ok := traectl.ProductByKey(key)
	if !ok {
		t.Fatalf("产品 %q 未注册，无法进行身份隔离断言", key)
	}
	if p.Key == "" {
		t.Fatalf("产品 %q 的 Key 为空，AccountUID 会退化成只有 userId", key)
	}
	return p
}

// identityPool 构造一个只服务本文件断言的池（无状态文件、无人为选号延迟）。
func identityPool() *pool.Pool {
	return pool.New(pool.Options{
		MaxInFlight:      2,
		BreakerThreshold: 3,
		BreakerCooldown:  time.Minute,
		SoftRateBase:     100 * time.Millisecond,
		SoftRateMax:      time.Second,
		ModelBase:        100 * time.Millisecond,
	})
}

// identityAccount 按生产装配方式（channel.go 的 BuildChannels 里的写法）
// 造一个池内账号：UID 来自 AccountUID，Realm 来自 RealmOfProduct。
func identityAccount(p traectl.Product, userID string) *pool.Account {
	return &pool.Account{
		UID:     AccountUID(p, userID),
		Realm:   RealmOfProduct(p.Key),
		Label:   p.Label,
		Product: p,
		Owner:   "public",
	}
}

// uidOf 把（可能为 nil 的）账号渲染成 UID，让失败信息可读且不 panic。
func uidOf(a *pool.Account) string {
	if a == nil {
		return "<nil>"
	}
	return a.UID
}

// TestAccountUIDNamespacesProductKey 是 C9 的根断言：
// 同一 userId 在两个产品下必须得到**不同**的 UID。
func TestAccountUIDNamespacesProductKey(t *testing.T) {
	trae := identityProduct(t, "trae-cn")
	solo := identityProduct(t, "solo-cn")

	gotTrae := AccountUID(trae, identityUserID)
	gotSolo := AccountUID(solo, identityUserID)

	// (1) 返回值必须是「产品 Key + '-' + userId」这个约定形状。
	// 期望值用字面量拼出来（而不是再调一次 AccountUID），否则就是同义反复。
	if want := "trae-cn-" + identityUserID; gotTrae != want {
		t.Errorf("AccountUID(trae-cn) = %q, want %q", gotTrae, want)
	}
	if want := "solo-cn-" + identityUserID; gotSolo != want {
		t.Errorf("AccountUID(solo-cn) = %q, want %q", gotSolo, want)
	}

	// (2) C9 的核心：UID 必须不同，否则池里会互相覆盖。
	if gotTrae == gotSolo {
		t.Fatalf("同 userId %s 在两个产品下得到相同 UID %q —— 账号会互相覆盖",
			identityUserID, gotTrae)
	}

	// (3) 反向对照：UID 不能等于裸 userId，否则前缀就是装饰品。
	if gotTrae == identityUserID || gotSolo == identityUserID {
		t.Errorf("UID 退化为裸 userId（%q）—— 产品前缀没有生效", identityUserID)
	}
}

// TestAccountUIDPairwiseDistinctAcrossProducts 对**当前已注册的全部产品**
// 断言两两不撞号（国际版若被 ConfigureIntl 注册进来也一并覆盖）。
func TestAccountUIDPairwiseDistinctAcrossProducts(t *testing.T) {
	seen := map[string]string{} // uid → product key
	for _, p := range traectl.Products {
		uid := AccountUID(p, identityUserID)
		if prev, dup := seen[uid]; dup {
			t.Errorf("产品 %q 与 %q 生成同一个 UID %q", prev, p.Key, uid)
		}
		seen[uid] = p.Key
	}
	if len(seen) < 2 {
		t.Fatalf("已注册产品不足 2 个（实际 %d），无法断言跨产品唯一性", len(seen))
	}
}

// TestAccountUIDDependsOnlyOnProductKey 把「可用渠道维度仅为 Product.Key」
// 这条结论**固化成断言**：两个只在 Label / ClientID / Function / DataDir 等
// 字段上不同、但 Key 相同的 Product，AccountUID 必须给出同一个 UID ——
// 即 UID 里不含 Key 以外的任何渠道信息；而只改 Key 必须改 UID。
func TestAccountUIDDependsOnlyOnProductKey(t *testing.T) {
	base := identityProduct(t, "trae-cn")

	clone := base
	clone.Label = "另一个标签"
	clone.DataDir = "Other Dir"
	clone.ClientID = "someotherclientid"
	clone.PlatformCode = "SOLO_PC"
	clone.AuthFrom = "solo"
	clone.Function = "other_function"

	if got, want := AccountUID(clone, identityUserID), AccountUID(base, identityUserID); got != want {
		t.Errorf("Key 相同的两个 Product 得到不同 UID: %q vs %q —— UID 里混入了 Key 以外的信息",
			got, want)
	}

	other := clone
	other.Key = base.Key + "-x"
	if AccountUID(other, identityUserID) == AccountUID(base, identityUserID) {
		t.Error("只改 Product.Key 未反映到 UID —— 产品维度没有真正生效")
	}
}

// TestIdentitySameEmailAcrossProductsCoexist 是 C9 的落点断言：
// 同一 userId 跨两个产品 → 池里是**两个**账号，UID 不同，各自独立可寻址。
func TestIdentitySameEmailAcrossProductsCoexist(t *testing.T) {
	trae := identityProduct(t, "trae-cn")
	solo := identityProduct(t, "solo-cn")

	p := identityPool()
	aTrae := identityAccount(trae, identityUserID)
	aSolo := identityAccount(solo, identityUserID)
	p.Add(aTrae)
	p.Add(aSolo)

	// pool.Add 对重复 UID 是**覆盖**语义（见 pool.go「Add 注册账号（重复 UID 覆盖）」），
	// 所以「池里有 2 个」就是「没有互相覆盖」的直接证据。
	if got := len(p.All()); got != 2 {
		t.Fatalf("同邮箱跨产品应并存 2 个账号，实际 %d 个 —— 发生了覆盖", got)
	}
	if p.Get(aTrae.UID) == nil {
		t.Errorf("按 UID %q 取不到 trae 账号", aTrae.UID)
	}
	if p.Get(aSolo.UID) == nil {
		t.Errorf("按 UID %q 取不到 solo 账号", aSolo.UID)
	}
	if p.Get(aTrae.UID) == p.Get(aSolo.UID) {
		t.Fatal("两个不同 UID 取回了同一个账号对象")
	}

	// 两个产品落在不同域 → 选号严格分域，互不串号。
	rTrae := RealmOfProduct(trae.Key)
	rSolo := RealmOfProduct(solo.Key)
	if rTrae == rSolo {
		t.Fatalf("trae-cn 与 solo-cn 的 realm 都是 %q，跨产品隔离无从谈起", rTrae)
	}
	if rTrae != realm.Trae || rSolo != realm.Solo {
		t.Fatalf("realm 映射异常: trae-cn=%q solo-cn=%q", rTrae, rSolo)
	}
	for i := 0; i < 20; i++ {
		if got := p.Pick(rTrae, "m", nil, nil); got == nil || got.UID != aTrae.UID {
			t.Fatalf("第 %d 次 Pick(%s) = %s, want %s", i, rTrae, uidOf(got), aTrae.UID)
		}
		if got := p.Pick(rSolo, "m", nil, nil); got == nil || got.UID != aSolo.UID {
			t.Fatalf("第 %d 次 Pick(%s) = %s, want %s", i, rSolo, uidOf(got), aSolo.UID)
		}
	}
}

// TestIdentitySameEmailAcrossProductsIndependentState 断言两个账号的**运行时状态
// 各自独立**：冷却、在途并发、禁用都互不串扰（参考实现所说的"独立实例、独立并发计数"）。
func TestIdentitySameEmailAcrossProductsIndependentState(t *testing.T) {
	trae := identityProduct(t, "trae-cn")
	solo := identityProduct(t, "solo-cn")
	rTrae := RealmOfProduct(trae.Key)
	rSolo := RealmOfProduct(solo.Key)

	p := identityPool()
	aTrae := identityAccount(trae, identityUserID)
	aSolo := identityAccount(solo, identityUserID)
	p.Add(aTrae)
	p.Add(aSolo)

	// (1) trae 硬冷却不影响 solo
	p.Cooldown(aTrae.UID, pool.CoolHard, time.Hour, "凭证失效")
	if p.Servable(rTrae) {
		t.Error("trae 硬冷却后 Servable(trae) 应为 false")
	}
	if !p.Servable(rSolo) {
		t.Error("trae 冷却不应影响 solo")
	}
	if got := p.Pick(rSolo, "m", nil, nil); got == nil || got.UID != aSolo.UID {
		t.Errorf("trae 冷却后 solo 仍应可选，得到 %s", uidOf(got))
	}
	if got := p.Pick(rTrae, "m", nil, nil); got != nil {
		t.Errorf("硬冷却的 trae 账号不应被选中，得到 %s", uidOf(got))
	}
	p.ClearCooldown(aTrae.UID)

	// (2) 在途租约各自独立（池 maxInFlight = 2）
	if !p.Acquire(aTrae.UID) || !p.Acquire(aTrae.UID) {
		t.Fatal("trae 账号应有 2 个在途额度")
	}
	if got := p.Pick(rTrae, "m", nil, nil); got != nil {
		t.Errorf("trae 在途占满后不应再被选中，得到 %s", uidOf(got))
	}
	if got := p.Pick(rSolo, "m", nil, nil); got == nil || got.UID != aSolo.UID {
		t.Errorf("trae 在途占满不应影响 solo 选号，得到 %s", uidOf(got))
	}
	p.Release(aTrae.UID)
	p.Release(aTrae.UID)

	// (3) 禁用 solo 不影响 trae
	p.SetDisabled(aSolo.UID, true, "维护")
	if got := p.Pick(rTrae, "m", nil, nil); got == nil || got.UID != aTrae.UID {
		t.Errorf("禁用 solo 不应影响 trae 选号，得到 %s", uidOf(got))
	}
	if got := p.Pick(rSolo, "m", nil, nil); got != nil {
		t.Errorf("已禁用的 solo 账号不应被选中，得到 %s", uidOf(got))
	}
}

// TestIdentityBareUserIDWouldCollide 是上面「并存」断言的**反例对照**：
// 若 UID 退化成裸 userId（即 AccountUID 丢掉 Product.Key），pool.Add 的
// 「重复 UID 覆盖」语义会让两个账号塌缩成一个 —— 那正是 C9 要防的"互相覆盖"。
//
// 这条测试的作用是证明「池里有 2 个」不是恒真断言：它同时钉住 pool.Add 的
// 覆盖语义，一旦该语义改变，这里会红，提示上面那条断言的前提需要重写。
func TestIdentityBareUserIDWouldCollide(t *testing.T) {
	trae := identityProduct(t, "trae-cn")
	solo := identityProduct(t, "solo-cn")

	p := identityPool()
	// 反例：两个产品都用裸 userId 当 UID
	p.Add(&pool.Account{UID: identityUserID, Realm: RealmOfProduct(trae.Key),
		Product: trae, Owner: "public"})
	p.Add(&pool.Account{UID: identityUserID, Realm: RealmOfProduct(solo.Key),
		Product: solo, Owner: "public"})

	if got := len(p.All()); got != 1 {
		t.Fatalf("裸 userId 作 UID 时应塌缩为 1 个账号（Add 的覆盖语义），实际 %d 个；"+
			"若这里不是 1，说明 pool.Add 的覆盖语义已变，上面「并存」断言的前提需要重写", got)
	}
	// 且活下来的是后注册的那个（solo）—— 覆盖是真的发生了，不是"两个都留着"。
	a := p.Get(identityUserID)
	if a == nil {
		t.Fatal("覆盖后应仍能按裸 userId 取到账号")
	}
	if a.Realm != realm.Solo {
		t.Errorf("覆盖后应只剩后注册的 solo 账号，实际 realm = %q", a.Realm)
	}
}
