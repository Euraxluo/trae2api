package usermgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "users.json"))
}

// seedAdmin 预置一个管理员。
//
// 为什么需要：首个用户必须是管理员（防自锁规则，见 Create 的注释）。
// 凡是要创建普通用户的测试，都得先有一个管理员。
func seedAdmin(t *testing.T, m *Manager) *User {
	t.Helper()
	u, err := m.Create("root", RoleAdmin, "测试预置")
	if err != nil {
		t.Fatalf("预置管理员失败: %v", err)
	}
	return u
}

// TestCreateAndFind 创建后能用 API Key 找到用户。
func TestCreateAndFind(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m) // 首个用户必须是管理员，先占位
	u, err := m.Create("alice", RoleUser, "测试")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID == "" || u.APIKey == "" {
		t.Fatal("ID / APIKey 不应为空")
	}
	if !strings.HasPrefix(u.APIKey, "sk-trae-") {
		t.Errorf("API Key 前缀不符: %q", u.APIKey)
	}
	got, ok := m.FindByAPIKey(u.APIKey)
	if !ok || got.ID != u.ID {
		t.Fatalf("FindByAPIKey 未命中: %+v %v", got, ok)
	}
	if m.Count() != 2 { // 预置管理员 + alice
		t.Errorf("Count = %d want 2", m.Count())
	}
}

// TestCreateValidation 用户名重复/非法角色必须报错。
func TestCreateValidation(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m)
	if _, err := m.Create("", RoleUser, ""); err == nil {
		t.Error("空用户名应报错")
	}
	if _, err := m.Create("bob", "superuser", ""); err == nil {
		t.Error("非法角色应报错")
	}
	if _, err := m.Create("bob", RoleUser, ""); err != nil {
		t.Fatalf("首次创建应成功: %v", err)
	}
	if _, err := m.Create("bob", RoleUser, ""); err == nil {
		t.Error("重复用户名应报错")
	}
	// 大小写不同视为同一用户名（避免 alice / Alice 混淆）
	if _, err := m.Create("BOB", RoleUser, ""); err == nil {
		t.Error("大小写不同的同名用户应报错")
	}
}

// TestFindByAPIKeyWrong 错误 Key / 空 Key / 已禁用用户都不应命中。
func TestFindByAPIKeyWrong(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m)
	u, _ := m.Create("alice", RoleUser, "")
	if _, ok := m.FindByAPIKey("sk-trae-wrong"); ok {
		t.Error("错误 Key 不应命中")
	}
	if _, ok := m.FindByAPIKey(""); ok {
		t.Error("空 Key 不应命中")
	}
	if _, ok := m.FindByAPIKey(u.APIKey + "x"); ok {
		t.Error("前缀相同但不同的 Key 不应命中")
	}
	if err := m.SetDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.FindByAPIKey(u.APIKey); ok {
		t.Error("已禁用用户不应命中")
	}
}

// TestResetKey 重置后旧 Key 立即失效、新 Key 可用。
func TestResetKey(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m)
	u, _ := m.Create("alice", RoleUser, "")
	old := u.APIKey
	nu, err := m.ResetKey(u.ID)
	if err != nil {
		t.Fatalf("ResetKey: %v", err)
	}
	if nu.APIKey == old {
		t.Fatal("新 Key 不应与旧 Key 相同")
	}
	if _, ok := m.FindByAPIKey(old); ok {
		t.Error("旧 Key 应已失效")
	}
	if _, ok := m.FindByAPIKey(nu.APIKey); !ok {
		t.Error("新 Key 应可用")
	}
	if _, err := m.ResetKey("ghost"); err == nil {
		t.Error("不存在的用户应报错")
	}
}

// TestDeleteLastAdminBlocked 不能删掉最后一个管理员（否则无人能管理）。
func TestDeleteLastAdminBlocked(t *testing.T) {
	m := newMgr(t)
	admin, _ := m.Create("root", RoleAdmin, "")
	if err := m.Delete(admin.ID); err == nil {
		t.Fatal("删除最后一个管理员应被拒绝")
	}
	// 再建一个管理员后就可以删了
	admin2, _ := m.Create("root2", RoleAdmin, "")
	if err := m.Delete(admin.ID); err != nil {
		t.Fatalf("有备用管理员时应可删除: %v", err)
	}
	if _, ok := m.ByID(admin.ID); ok {
		t.Error("删除后不应还能查到")
	}
	if _, ok := m.ByID(admin2.ID); !ok {
		t.Error("备用管理员应仍在")
	}
}

// TestDisableLastAdminBlocked 不能禁用最后一个管理员。
func TestDisableLastAdminBlocked(t *testing.T) {
	m := newMgr(t)
	admin, _ := m.Create("root", RoleAdmin, "")
	if err := m.SetDisabled(admin.ID, true); err == nil {
		t.Fatal("禁用最后一个管理员应被拒绝")
	}
	// 普通用户可以随便禁用（不涉及管理面）
	u, _ := m.Create("alice", RoleUser, "")
	if err := m.SetDisabled(u.ID, true); err != nil {
		t.Fatalf("禁用普通用户应成功: %v", err)
	}
}

// TestDeleteUser 删除普通用户并确认索引全部清理。
func TestDeleteUser(t *testing.T) {
	m := newMgr(t)
	m.Create("root", RoleAdmin, "")
	u, _ := m.Create("alice", RoleUser, "")
	if err := m.Delete(u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.ByID(u.ID); ok {
		t.Error("byID 未清理")
	}
	if _, ok := m.FindByAPIKey(u.APIKey); ok {
		t.Error("byKey 未清理")
	}
	// 同名可再次创建（说明 byName 也清理了）
	if _, err := m.Create("alice", RoleUser, ""); err != nil {
		t.Errorf("删除后应能重建同名用户: %v", err)
	}
}

// TestPersistenceRoundTrip 落盘与加载往返。
func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	m1 := New(path)
	admin, _ := m1.Create("root", RoleAdmin, "管理员")
	user, _ := m1.Create("alice", RoleUser, "给 Cline 用")
	m1.SetDisabled(user.ID, true)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("用户文件未生成: %v", err)
	}

	m2 := New(path)
	if err := m2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m2.Count() != 2 {
		t.Fatalf("Count = %d want 2", m2.Count())
	}
	// API Key 必须原样恢复（否则用户配置好的客户端会全部失效）
	if got, ok := m2.FindByAPIKey(admin.APIKey); !ok || got.ID != admin.ID {
		t.Errorf("管理员 Key 未恢复: %+v %v", got, ok)
	}
	u2, ok := m2.ByID(user.ID)
	if !ok {
		t.Fatal("普通用户未恢复")
	}
	if !u2.Disabled {
		t.Error("禁用状态未恢复")
	}
	if u2.Note != "给 Cline 用" {
		t.Errorf("备注未恢复: %q", u2.Note)
	}
	if u2.APIKey != user.APIKey {
		t.Error("普通用户 Key 未原样恢复")
	}
}

// TestLoadMissingFile 文件不存在视为空表（首次运行）。
func TestLoadMissingFile(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "nope.json"))
	if err := m.Load(); err != nil {
		t.Fatalf("缺失文件应静默通过: %v", err)
	}
	if m.Count() != 0 {
		t.Errorf("Count = %d want 0", m.Count())
	}
}

// TestLoadCorruptFile 损坏文件应报错（由调用方决定是否降级）。
func TestLoadCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(path)
	if err := m.Load(); err == nil {
		t.Error("损坏文件应报错")
	}
}

// TestLoadSkipsIncompleteEntries 残缺条目应被跳过而不是让整体加载失败。
func TestLoadSkipsIncompleteEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	body := `[
	  {"id":"u1","username":"ok","api_key":"sk-trae-aaaa","role":"admin"},
	  {"id":"","username":"noid","api_key":"sk-trae-bbbb"},
	  {"id":"u3","username":"nokey","api_key":""},
	  {"id":"u4","username":"badrole","api_key":"sk-trae-cccc","role":"weird"}
	]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(path)
	if err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// u1 与 u4 应保留（u4 的非法角色回落 user），u2/u3 跳过
	if m.Count() != 2 {
		t.Fatalf("Count = %d want 2（残缺条目应被跳过）", m.Count())
	}
	if u, ok := m.ByID("u4"); !ok || u.Role != RoleUser {
		t.Errorf("非法角色应回落 user: %+v", u)
	}
}

// TestMaskKey API Key 打码：不泄露完整 Key，且短 Key 也安全。
func TestMaskKey(t *testing.T) {
	full := "sk-trae-0123456789abcdef0123456789abcdef"
	masked := MaskKey(full)
	if masked == full {
		t.Fatal("打码后不应等于原文")
	}
	if strings.Contains(masked, "456789abcdef0123456") {
		t.Errorf("打码泄露了中段: %q", masked)
	}
	if !strings.HasPrefix(masked, "sk-trae-") {
		t.Errorf("打码应保留可识别前缀: %q", masked)
	}
	for _, short := range []string{"", "abc", "sk-trae-1234"} {
		if got := MaskKey(short); got != "****" {
			t.Errorf("短 Key %q 应整体打码，得到 %q", short, got)
		}
	}
}

// TestListOrdering 列表顺序稳定：管理员在前，其余按创建时间。
func TestListOrdering(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m) // 已有管理员时，普通用户可以随便建
	m.Create("u1", RoleUser, "")
	m.Create("root2", RoleAdmin, "")
	m.Create("u2", RoleUser, "")
	list := m.List()
	if len(list) != 4 {
		t.Fatalf("len = %d want 4", len(list))
	}
	// 管理员必须排在前两个
	if list[0].Role != RoleAdmin || list[1].Role != RoleAdmin {
		t.Errorf("管理员应排最前，得到 %q %q", list[0].Role, list[1].Role)
	}
	// 连续两次调用顺序一致（稳定）
	l2 := m.List()
	for i := range list {
		if list[i].ID != l2[i].ID {
			t.Fatalf("List 顺序不稳定：%v vs %v", list, l2)
		}
	}
}

// TestListIsCopy 返回的是副本，调用方修改不应影响内部状态。
func TestListIsCopy(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m)
	u, _ := m.Create("alice", RoleUser, "")
	list := m.List()
	// 找到 alice 那一项再改（列表顺序按角色/时间排，不能假定下标）
	var idx = -1
	for i := range list {
		if list[i].ID == u.ID {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("列表中找不到 alice")
	}
	list[idx].Username = "mutated"
	got, _ := m.ByID(u.ID)
	if got.Username != "alice" {
		t.Errorf("List 返回的副本被修改后污染了内部状态: %q", got.Username)
	}
}

// TestRoleValid 角色校验。
func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleAdmin, RoleUser} {
		if !r.Valid() {
			t.Errorf("%q 应合法", r)
		}
	}
	for _, r := range []Role{"", "root", "ADMIN"} {
		if r.Valid() {
			t.Errorf("%q 不应合法", r)
		}
	}
}

// TestMemoryFirstOnSaveFailure 落盘失败时用户仍应在内存中生效。
//
// 与 workbuddy2api 的「内存优先」同一考虑：宁可丢持久化，也不能让用户
// 创建后无法立即使用。
func TestMemoryFirstOnSaveFailure(t *testing.T) {
	// 指向一个不存在且无法创建的目录（把文件路径当成目录用，必然失败）
	bad := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(filepath.Join(bad, "users.json")) // bad 是文件，无法在其下建目录
	// 首个用户必须是管理员，故这里建 admin（与落盘失败无关）
	u, err := m.Create("root", RoleAdmin, "")
	if err == nil {
		t.Skip("该环境下落盘竟然成功了，跳过内存优先验证")
	}
	if u == nil {
		t.Fatal("落盘失败时仍应返回已创建的用户（内存优先）")
	}
	if _, ok := m.FindByAPIKey(u.APIKey); !ok {
		t.Error("落盘失败的用户应已在内存中生效")
	}
}

// TestFirstUserMustBeAdmin 首个用户必须是管理员 —— 防自锁规则。
//
// 若允许首个用户是普通用户：一旦创建，鉴权就禁止匿名，而全局 api_key 可能为空，
// 结果是谁都进不了管理面（无法再建用户、无法重置 Key），只能手工改 users.json。
func TestFirstUserMustBeAdmin(t *testing.T) {
	m := newMgr(t)
	if _, err := m.Create("alice", RoleUser, ""); err == nil {
		t.Fatal("首个用户是普通用户时应被拒绝")
	}
	if m.Count() != 0 {
		t.Errorf("被拒绝的创建不应留下记录，Count = %d", m.Count())
	}
	if _, err := m.Create("root", RoleAdmin, ""); err != nil {
		t.Fatalf("首个管理员应创建成功: %v", err)
	}
	// 有管理员之后，普通用户可以正常创建
	if _, err := m.Create("alice", RoleUser, ""); err != nil {
		t.Fatalf("有管理员后应可创建普通用户: %v", err)
	}
}

// TestSetRole 改角色。
func TestSetRole(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m)
	u, err := m.Create("alice", RoleUser, "")
	if err != nil {
		t.Fatal(err)
	}
	// user → admin
	if err := m.SetRole(u.ID, RoleAdmin); err != nil {
		t.Fatalf("提升为管理员失败: %v", err)
	}
	got, _ := m.ByID(u.ID)
	if got.Role != RoleAdmin {
		t.Errorf("角色 = %q want admin", got.Role)
	}
	// 幂等：设成相同角色不该报错
	if err := m.SetRole(u.ID, RoleAdmin); err != nil {
		t.Errorf("设为相同角色应幂等: %v", err)
	}
	// admin → user（此时有两个管理员，允许）
	if err := m.SetRole(u.ID, RoleUser); err != nil {
		t.Errorf("降级（还有另一个管理员）应允许: %v", err)
	}
}

// TestSetRoleCannotDemoteLastAdmin 不能把**最后一个管理员**降级。
//
// 与 Delete/SetDisabled 是同一条安全规则：否则管理面会永久锁死
// （没人能再建用户、改配置、重置 Key）。这是"首个用户必须是管理员"那条规则
// 防的同一件事，只是换了个入口。
func TestSetRoleCannotDemoteLastAdmin(t *testing.T) {
	m := newMgr(t)
	root := seedAdmin(t, m) // 唯一的管理员

	if err := m.SetRole(root.ID, RoleUser); err == nil {
		t.Fatal("降级最后一个管理员应被拒绝")
	}
	if got, _ := m.ByID(root.ID); got.Role != RoleAdmin {
		t.Errorf("被拒绝的降级不该改动角色，得到 %q", got.Role)
	}

	// 再加一个管理员后就可以降级了
	if _, err := m.Create("root2", RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRole(root.ID, RoleUser); err != nil {
		t.Errorf("有第二个管理员后应允许降级: %v", err)
	}
}

// TestSetRoleValidation 非法角色与不存在的用户。
func TestSetRoleValidation(t *testing.T) {
	m := newMgr(t)
	seedAdmin(t, m)
	if err := m.SetRole("nope", RoleAdmin); err == nil {
		t.Error("不存在的用户应报错")
	}
	root := m.List()[0]
	if err := m.SetRole(root.ID, Role("superuser")); err == nil {
		t.Error("非法角色应报错")
	}
}
