package vscdb

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WAL 读取的测试数据是**真 SQLite 写出来的**，不是本包自己造的：
//
//	testdata/wal/state.vscdb      主库（处于 WAL 模式，文件头版本号 2/2，5 页）
//	testdata/wal/state.vscdb-wal  未 checkpoint 的 WAL（4 个已提交帧）
//
// 生成方式（Node 内置的 node:sqlite，即官方 SQLite）：
//
//	page_size=4096；建 ItemTable(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)；
//	先写 pre_big(9000 字符)、small("unchanged")、big("stale-v1")  -> 只落在主库文件里
//	再 PRAGMA journal_mode=WAL + wal_autocheckpoint=0
//	最后 UPDATE big 为一个 4800 字符的长值                        -> 只落在 -wal 里
//	（在连接仍打开时把 state.vscdb 与 state.vscdb-wal 拷出来，否则 close 会 checkpoint 并删掉 WAL）
//
// 于是：主库文件里的 big 是旧值 "stale-v1"，真值在 WAL 里。谁不读 WAL，谁就拿到陈旧数据。
// 用真 SQLite 产出的 WAL 做正例，才能证明本包的 WAL 头/帧校验和实现是对的
// （校验和算法写错的话，帧会被判为无效，测试立刻失败）。

const (
	walTestPreBigBytes = 9000
	walTestPreBigSHA   = "055790505de26c45ceea9555600002da665b6e3ae758df48bb362119f5ffbae2"
	walTestNewBigBytes = 4800
	walTestNewBigSHA   = "301b00541f880cddae842795fd3632be036cb807b3338ac00616d10c902653a0"
)

// seq 复刻生成脚本里的 Array.from(...).map(i => String(f(i)).padStart(4,'0')).join(”)
func seq(n int, f func(i int) int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "%04d", f(i))
	}
	return sb.String()
}

func walPreBig() string { return seq(2250, func(i int) int { return i % 10000 }) }
func walNewBig() string { return seq(1200, func(i int) int { return (i * 7) % 10000 }) }

// copyTestdata 把 testdata/wal 下的文件拷到临时目录（-wal 必须与主库同目录同前缀）。
func copyTestdata(t *testing.T, names ...string) string {
	t.Helper()
	dst := t.TempDir()
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join("testdata", "wal", n))
		if err != nil {
			t.Skipf("测试数据缺失（%s），跳过：%v", n, err)
		}
		if err := os.WriteFile(filepath.Join(dst, n), raw, 0o600); err != nil {
			t.Fatalf("写临时文件: %v", err)
		}
	}
	return filepath.Join(dst, "state.vscdb")
}

// TestWALFixtureSanity 先自证「Go 里重建的期望值」与生成脚本写进库里的值一致，
// 避免期望值本身写错导致下面的断言变成空转。
func TestWALFixtureSanity(t *testing.T) {
	pre, new_ := walPreBig(), walNewBig()
	if len(pre) != walTestPreBigBytes || shaHex(pre) != walTestPreBigSHA {
		t.Fatalf("pre_big 期望值重建错误：%d 字节 %s", len(pre), shaHex(pre))
	}
	if len(new_) != walTestNewBigBytes || shaHex(new_) != walTestNewBigSHA {
		t.Fatalf("big(新) 期望值重建错误：%d 字节 %s", len(new_), shaHex(new_))
	}
	// 真值也应与 node:sqlite 一致（生成脚本打印过同样的 SHA-256）
	if _, err := os.Stat(filepath.Join("testdata", "wal", "state.vscdb-wal")); err != nil {
		t.Skip("没有 WAL 测试数据，跳过")
	}
}

// TestWALReadsCommittedFrames 有 -wal 时必须读到 WAL 里的新数据，
// 并且主库文件里的其它页（pre_big 的 overflow 链）仍要能正确读出。
func TestWALReadsCommittedFrames(t *testing.T) {
	path := copyTestdata(t, "state.vscdb", "state.vscdb-wal")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if db.wal == nil {
		t.Fatal("未识别出 -wal（真 SQLite 产出的 WAL 应当被接受）")
	}
	// 提交帧里的 nTruncate：更新 big 后 SQLite 给新值又分配了一个溢出页，
	// 数据库从 5 页长到 6 页，所以 WAL 声明的可见页数是 6（比主库文件多一页）。
	if db.wal.commit != 6 {
		t.Fatalf("WAL 提交页数 = %d，期望 6", db.wal.commit)
	}
	if db.pageCount != 6 {
		t.Fatalf("可见页数 = %d，期望 6（必须采用 WAL 声明的页数）", db.pageCount)
	}

	// 1) big 的新值只存在于 WAL：叶子页与它的 overflow 页都必须从 WAL 取。
	if got := string(mustGet(t, db, "big")); got != walNewBig() {
		t.Fatalf("big = %d 字节 %s，期望 %d 字节 %s",
			len(got), shaHex(got), walTestNewBigBytes, walTestNewBigSHA)
	}
	// 2) pre_big 的 overflow 链只在主库文件里（WAL 没覆盖那几页）：混合读取必须正确。
	if got := string(mustGet(t, db, "pre_big")); got != walPreBig() {
		t.Fatalf("pre_big = %d 字节 %s，期望 %d 字节 %s",
			len(got), shaHex(got), walTestPreBigBytes, walTestPreBigSHA)
	}
	// 3) 短值仍然正常
	if got := string(mustGet(t, db, "small")); got != "unchanged" {
		t.Fatalf("small = %q", got)
	}
	if _, err := db.GetItem("nope"); !isErr(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}

	// 4) 全程不得生成 -shm（本包自行解析 WAL，不需要共享内存索引）
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.vscdb" && e.Name() != "state.vscdb-wal" {
			t.Fatalf("读取过程中多出了文件 %q", e.Name())
		}
	}
}

// TestWALAbsentFallsBackToMainFile 反证 WAL 是「承重」的：
// 只留主库文件时读到的是 checkpoint 之前的旧值，正好说明为什么必须读 WAL。
func TestWALAbsentFallsBackToMainFile(t *testing.T) {
	path := copyTestdata(t, "state.vscdb") // 故意不拷 -wal

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.wal != nil {
		t.Fatal("没有 -wal 文件时不应有 WAL 索引")
	}
	if got := string(mustGet(t, db, "big")); got != "stale-v1" {
		t.Fatalf("big = %q，期望主库里的旧值 stale-v1", got)
	}
	if got := string(mustGet(t, db, "small")); got != "unchanged" {
		t.Fatalf("small = %q", got)
	}
}

// TestWALChecksumRejectsCorruption WAL 数据被改坏时必须整体忽略并回退主库，
// 而不是把损坏的页当成数据返回。
func TestWALChecksumRejectsCorruption(t *testing.T) {
	path := copyTestdata(t, "state.vscdb", "state.vscdb-wal")
	walPath := path + "-wal"

	raw, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("读 WAL: %v", err)
	}
	// 改掉第一帧页数据里的一个字节：帧校验和应当失败，整个 WAL 失效。
	raw[walHeaderSize+walFrameHdrSize+10] ^= 0xff
	if err := os.WriteFile(walPath, raw, 0o600); err != nil {
		t.Fatalf("写 WAL: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.wal != nil {
		t.Fatal("校验和不匹配的 WAL 不应被接受")
	}
	if got := string(mustGet(t, db, "big")); got != "stale-v1" {
		t.Fatalf("big = %q，期望回退到主库的 stale-v1", got)
	}
}

// TestWALHeaderCorruption 同理：WAL 头坏掉时整个 WAL 被忽略。
func TestWALHeaderCorruption(t *testing.T) {
	path := copyTestdata(t, "state.vscdb", "state.vscdb-wal")
	walPath := path + "-wal"

	raw, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("读 WAL: %v", err)
	}
	raw[0] ^= 0xff // 破坏 magic
	if err := os.WriteFile(walPath, raw, 0o600); err != nil {
		t.Fatalf("写 WAL: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.wal != nil {
		t.Fatal("magic 损坏的 WAL 不应被接受")
	}
	if got := string(mustGet(t, db, "big")); got != "stale-v1" {
		t.Fatalf("big = %q，期望回退到主库的 stale-v1", got)
	}
}

// TestWALIgnoredInRollbackMode 主库文件头不是 WAL 模式（版本 1/1）时，
// 即使旁边躺着一个 -wal 也不能用它——否则会读到早已废弃的数据。
func TestWALIgnoredInRollbackMode(t *testing.T) {
	path := copyTestdata(t, "state.vscdb", "state.vscdb-wal")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读主库: %v", err)
	}
	if raw[18] != 2 || raw[19] != 2 {
		t.Fatalf("测试数据的主库文件头版本应为 2/2，实际 %d/%d", raw[18], raw[19])
	}
	raw[18], raw[19] = 1, 1 // 假装这是 journal_mode=delete 的库
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写主库: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.wal != nil {
		t.Fatal("非 WAL 模式下不应解析 -wal")
	}
	if got := string(mustGet(t, db, "big")); got != "stale-v1" {
		t.Fatalf("big = %q，期望主库里的 stale-v1", got)
	}
}

// TestWALChecksumHelper 用官方 SQLite 的 WAL 头做校验和自证：
// 头校验和覆盖前 24 字节，结果必须等于头里记录的 24..31 字节。
func TestWALChecksumHelper(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "wal", "state.vscdb-wal"))
	if err != nil {
		t.Skipf("没有 WAL 测试数据，跳过: %v", err)
	}
	magic := uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
	bigEnd := magic&1 == 1
	s0, s1 := walChecksum(bigEnd, raw[0:24], 0, 0)
	want0 := uint32(raw[24])<<24 | uint32(raw[25])<<16 | uint32(raw[26])<<8 | uint32(raw[27])
	want1 := uint32(raw[28])<<24 | uint32(raw[29])<<16 | uint32(raw[30])<<8 | uint32(raw[31])
	if s0 != want0 || s1 != want1 {
		t.Fatalf("WAL 头校验和 (%08x,%08x)，头里记录的是 (%08x,%08x)", s0, s1, want0, want1)
	}
	// 逐帧累加校验：4 帧全部通过才说明实现与 SQLite 一致。
	ck0, ck1 := s0, s1
	frames := (len(raw) - walHeaderSize) / (walFrameHdrSize + 4096)
	if frames != 4 {
		t.Fatalf("WAL 帧数 = %d，期望 4", frames)
	}
	for i := 0; i < frames; i++ {
		f := raw[walHeaderSize+i*(walFrameHdrSize+4096):]
		c0, c1 := walChecksum(bigEnd, f[0:8], ck0, ck1)
		c0, c1 = walChecksum(bigEnd, f[walFrameHdrSize:walFrameHdrSize+4096], c0, c1)
		g0 := uint32(f[16])<<24 | uint32(f[17])<<16 | uint32(f[18])<<8 | uint32(f[19])
		g1 := uint32(f[20])<<24 | uint32(f[21])<<16 | uint32(f[22])<<8 | uint32(f[23])
		if c0 != g0 || c1 != g1 {
			t.Fatalf("第 %d 帧校验和 (%08x,%08x)，帧里记录的是 (%08x,%08x)", i, c0, c1, g0, g1)
		}
		ck0, ck1 = c0, c1
	}
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
