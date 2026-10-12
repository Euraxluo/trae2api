package accountstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"trae2api/internal/traectl"
)

// testKeyPair 生成一份真实的 EC P-256 密钥对（PEM）。
//
// 必须用真密钥：accountstore 的 Add 会走 traectl 的 Validate，
// 而 Validate 会真的解析私钥 —— 假 PEM 会被拒。
func testKeyPair(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

// validSnap 生成一份字段完整、私钥真实的快照。
func validSnap(t *testing.T, product, userID string) *traectl.CredentialSnapshot {
	t.Helper()
	priv, pub := testKeyPair(t)
	return &traectl.CredentialSnapshot{
		Product: product, UserID: userID, Label: "测试 " + userID,
		Token: "tok-" + userID, RefreshToken: "ref-" + userID,
		DeviceID: "1234567890", MachineID: "abcdef",
		PrivateKeyPEM: priv, PublicKeyPEM: pub,
	}
}

func TestAddAndList(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	name, err := s.Add(validSnap(t, "trae-cn", "916348159159136"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if name != "trae-cn-916348159159136.json" {
		t.Errorf("文件名 = %q（应含产品与 userId）", name)
	}

	entries, skipped := s.List()
	if len(skipped) != 0 {
		t.Errorf("不应有跳过项: %v", skipped)
	}
	if len(entries) != 1 {
		t.Fatalf("List 返回 %d 条 want 1", len(entries))
	}
	if entries[0].Snap.UserID != "916348159159136" {
		t.Errorf("userId 不符: %q", entries[0].Snap.UserID)
	}
	// 落盘文件必须能被独立解析（可移植性）
	raw, err := os.ReadFile(entries[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	var back traectl.CredentialSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("落盘文件不是合法 JSON: %v", err)
	}
	if back.Token != entries[0].Snap.Token {
		t.Error("落盘内容与内存不一致")
	}
}

// TestAddOverwritesSameAccount 同一账号重复导入必须**覆盖**而不是堆积副本。
//
// refreshToken 是轮换语义：留两份必然有一份失效，覆盖才是对的。
func TestAddOverwritesSameAccount(t *testing.T) {
	s := New(t.TempDir())
	s1 := validSnap(t, "trae-cn", "u1")
	s1.RefreshToken = "old"
	if _, err := s.Add(s1); err != nil {
		t.Fatal(err)
	}
	s2 := validSnap(t, "trae-cn", "u1")
	s2.RefreshToken = "new"
	if _, err := s.Add(s2); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	if len(entries) != 1 {
		t.Fatalf("重复导入应覆盖，实际 %d 份", len(entries))
	}
	if entries[0].Snap.RefreshToken != "new" {
		t.Errorf("未覆盖为最新 refreshToken: %q", entries[0].Snap.RefreshToken)
	}
}

// TestDifferentProductsSameUserID 同一 userId 在两个产品下是两份独立快照。
func TestDifferentProductsSameUserID(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Add(validSnap(t, "trae-cn", "u1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(validSnap(t, "solo-cn", "u1")); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	if len(entries) != 2 {
		t.Fatalf("两个产品应各存一份，实际 %d", len(entries))
	}
	names := []string{entries[0].Name, entries[1].Name}
	if names[0] == names[1] {
		t.Error("文件名撞了")
	}
}

// TestAddRejectsInvalid 无效快照必须被拒绝且不落盘。
func TestAddRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	bad := validSnap(t, "trae-cn", "u1")
	bad.PrivateKeyPEM = "" // 没有私钥 → 无法续期 → 必须拒绝
	if _, err := s.Add(bad); err == nil {
		t.Fatal("缺私钥的快照应被拒绝")
	}
	if entries, _ := s.List(); len(entries) != 0 {
		t.Errorf("被拒绝的快照不应落盘，实际 %d 份", len(entries))
	}
}

// TestListSkipsCorrupt 坏文件只跳过，不影响其余（导入目录混进坏文件很常见）。
func TestListSkipsCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if _, err := s.Add(validSnap(t, "trae-cn", "good")); err != nil {
		t.Fatal(err)
	}
	// 混入三种坏文件
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "empty.json"), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ignored"), 0o600)

	entries, skipped := s.List()
	if len(entries) != 1 {
		t.Fatalf("好账号应仍被读出，实际 %d", len(entries))
	}
	if len(skipped) != 2 {
		t.Errorf("应有 2 条跳过说明（broken + empty），实际 %v", skipped)
	}
	for _, sk := range skipped {
		if !strings.Contains(sk, "broken") && !strings.Contains(sk, "empty") {
			t.Errorf("跳过说明未指明文件: %q", sk)
		}
	}
}

// TestDelete 删除与路径穿越防护。
func TestDelete(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	name, err := s.Add(validSnap(t, "trae-cn", "u1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if entries, _ := s.List(); len(entries) != 0 {
		t.Errorf("删除后应为空，实际 %d", len(entries))
	}
	if err := s.Delete(name); err == nil {
		t.Error("删除不存在的快照应报错")
	}
	// 路径穿越必须被拒
	for _, evil := range []string{"../evil.json", "..", "a/b.json", `..\evil.json`} {
		if err := s.Delete(evil); err == nil {
			t.Errorf("路径穿越 %q 应被拒绝", evil)
		}
	}
}

// TestEmptyDirDisabled 未配置目录时操作应安全失败（不 panic）。
func TestEmptyDirDisabled(t *testing.T) {
	s := New("")
	if entries, skipped := s.List(); len(entries) != 0 || len(skipped) != 0 {
		t.Error("空目录应返回空结果")
	}
	if _, err := s.Add(validSnap(t, "trae-cn", "u1")); err == nil {
		t.Error("未配置目录时 Add 应报错")
	}
	if err := s.Delete("x.json"); err == nil {
		t.Error("未配置目录时 Delete 应报错")
	}
	if s.Count() != 0 {
		t.Error("Count 应为 0")
	}
}

// TestDirPermissions 快照目录含长期凭证，POSIX 下权限必须是 0700。
//
// Windows 跳过：Go 在 Windows 不映射 POSIX 权限位（MkdirAll 的 perm 被忽略，
// Stat 恒报 0777），保护依赖用户目录的 ACL 继承。这不是实现问题，见 accountstore.go 注释。
func TestDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不映射 POSIX 权限位（依赖 ACL 继承），此断言无意义")
	}
	dir := filepath.Join(t.TempDir(), "accounts")
	s := New(dir)
	if _, err := s.Add(validSnap(t, "trae-cn", "u1")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("目录权限 = %o want 700（含 refreshToken 与设备私钥）", perm)
	}
}

// TestFileNameSanitized userId 里的非法字符必须被替换（防路径穿越）。
func TestFileNameSanitized(t *testing.T) {
	s := New(t.TempDir())
	for _, evil := range []string{"../../evil", `..\..\evil`, "a/b", "x:y", ".."} {
		snap := validSnap(t, "trae-cn", evil)
		name, err := s.Add(snap)
		if err != nil {
			t.Fatalf("Add(%q): %v", evil, err)
		}
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("输入 %q 生成的文件名含路径分隔: %q", evil, name)
		}
		if strings.Contains(name, "..") {
			t.Errorf("输入 %q 生成的文件名含 ..: %q", evil, name)
		}
		if filepath.Base(name) != name {
			t.Errorf("输入 %q 生成的文件名不是纯文件名: %q", evil, name)
		}
	}
}
