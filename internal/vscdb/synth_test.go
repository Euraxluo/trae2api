package vscdb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// 本文件用标准库手写最小合法的 SQLite 文件字节流，覆盖不依赖真实 Trae 客户端的解析路径：
// 文件头、sqlite_master、表叶子页、表内部页、record、overflow 链、UTF-16 文本编码。
//
// 构造器只服务于测试，故意写得直白（页内自页尾向下摆放 cell），不追求通用。

const itemTableSQL = "CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)"

// ---------- 页/record 构造 ----------

type synthDB struct {
	pageSize int
	enc      int
	pages    [][]byte
}

func newSynth(pageSize, enc int) *synthDB {
	return &synthDB{pageSize: pageSize, enc: enc}
}

// usable 与实现一致：本构造器不设 reserved 区，故可用空间就是页大小。
func (s *synthDB) usable() int { return s.pageSize }

// newPage 追加一页并返回 (页号, 页缓冲)。页号从 1 开始。
func (s *synthDB) newPage() (uint32, []byte) {
	p := make([]byte, s.pageSize)
	s.pages = append(s.pages, p)
	return uint32(len(s.pages)), p
}

// bytes 拼出完整文件。注意 SQLite 的第 1 页**包含** 100 字节文件头：
// 文件头就是第 1 页的前 100 字节，所以这里不额外前置文件头。
func (s *synthDB) bytes() []byte {
	out := make([]byte, 0, s.pageSize*len(s.pages))
	for _, p := range s.pages {
		out = append(out, p...)
	}
	return out
}

func (s *synthDB) header() []byte {
	h := make([]byte, headerSize)
	copy(h, magic)
	if s.pageSize == 65536 {
		putU16(h, 16, 1) // 规范：写 1 表示 65536
	} else {
		putU16(h, 16, s.pageSize)
	}
	h[18], h[19], h[20] = 1, 1, 0
	h[21], h[22], h[23] = 64, 32, 32
	putU32(h, 24, 1)                    // file change counter
	putU32(h, 28, uint32(len(s.pages))) // 数据库页数
	putU32(h, 40, 1)                    // schema cookie
	putU32(h, 44, 4)                    // schema format number
	putU32(h, 56, uint32(s.enc))        // text encoding
	putU32(h, 92, 1)                    // version-valid-for
	putU32(h, 96, 3045000)              // sqlite version number
	return h
}

// leafCell 生成一个表叶子 cell：varint(payload 长度) + varint(rowid) + 局部 payload
// + 可选 4 字节溢出页号。局部大小按 SQLite 规则算，余下部分按需分配溢出页。
func (s *synthDB) leafCell(rowid int64, payload []byte) []byte {
	U := s.usable()
	P := len(payload)
	local := synthLocalSize(U, P)
	cell := appendVarint(nil, uint64(P))
	cell = appendVarint(cell, uint64(rowid))
	cell = append(cell, payload[:local]...)
	if local >= P {
		return cell
	}
	rest := payload[local:]
	var (
		first    uint32
		prevPage []byte
	)
	for len(rest) > 0 {
		n := U - 4
		if n > len(rest) {
			n = len(rest)
		}
		pgno, pg := s.newPage()
		copy(pg[4:4+n], rest[:n])
		if prevPage == nil {
			first = pgno
		} else {
			putU32(prevPage, 0, pgno) // 上一页的下一页指针
		}
		prevPage = pg
		rest = rest[n:]
	}
	return appendU32(cell, first)
}

// synthLocalSize 是 SQLite 局部 payload 规则的独立复写（测试里重新推一遍，
// 避免直接调用实现内部的算式而失去对照意义）。
func synthLocalSize(U, P int) int {
	X := U - 35
	if P <= X {
		return P
	}
	m := (U-12)*32/255 - 23
	k := m + (P-m)%(U-4)
	if k <= X {
		return k
	}
	return m
}

// leafTablePage 组装表叶子页（页类型 0x0d）。base 是该页 b-tree 页头在页内的偏移：
// 第 1 页被 100 字节文件头占住前 100 字节，所以 base=100；其余页 base=0。
// cell 内容偏移是页内绝对偏移，故 cell 一律自页尾向下摆放。
func leafTablePage(t *testing.T, pageSize, base int, cells [][]byte) []byte {
	t.Helper()
	p := make([]byte, pageSize)
	cur := pageSize
	offs := make([]int, len(cells))
	for i, c := range cells {
		cur -= len(c)
		copy(p[cur:], c)
		offs[i] = cur
	}
	if len(cells) > 0 && cur < base+8+2*len(cells) {
		t.Fatalf("测试构造失败：%d 个 cell 共 %d 字节，放不进 %d 字节的页",
			len(cells), pageSize-cur, pageSize)
	}
	p[base] = pageTypeLeafTable
	putU16(p, base+3, len(cells))
	putU16(p, base+5, cur) // cell content start
	for i, o := range offs {
		putU16(p, base+8+2*i, o)
	}
	return p
}

// interiorTablePage 组装表内部页（页类型 0x05）：cells 为左子页号，keys 为分隔键
// （比 children 少一个），最后一个 child 作为 rightmost pointer。
func interiorTablePage(t *testing.T, pageSize, base int, children []uint32, keys []int64) []byte {
	t.Helper()
	if len(children) != len(keys)+1 {
		t.Fatalf("测试构造失败：children=%d 与 keys=%d 不匹配", len(children), len(keys))
	}
	p := make([]byte, pageSize)
	cells := make([][]byte, len(keys))
	for i := range keys {
		c := appendU32(nil, children[i])
		c = appendVarint(c, uint64(keys[i]))
		cells[i] = c
	}
	cur := pageSize
	offs := make([]int, len(cells))
	for i, c := range cells {
		cur -= len(c)
		copy(p[cur:], c)
		offs[i] = cur
	}
	if cur < base+12+2*len(cells) {
		t.Fatalf("测试构造失败：内部页 cell 放不进 %d 字节的页", pageSize)
	}
	p[base] = pageTypeInteriorTable
	putU16(p, base+3, len(cells))
	putU16(p, base+5, cur)
	putU32(p, base+8, children[len(children)-1])
	for i, o := range offs {
		putU16(p, base+12+2*i, o)
	}
	return p
}

// ---------- record 构造 ----------

type synthCol struct {
	serial uint64
	body   []byte
}

func textCol(v string) synthCol {
	b := []byte(v)
	return synthCol{serial: 13 + 2*uint64(len(b)), body: b}
}

func textColRaw(b []byte) synthCol {
	return synthCol{serial: 13 + 2*uint64(len(b)), body: b}
}

func blobCol(b []byte) synthCol {
	return synthCol{serial: 12 + 2*uint64(len(b)), body: b}
}

func intCol(v int64) synthCol {
	switch {
	case v == 0:
		return synthCol{serial: 8}
	case v == 1:
		return synthCol{serial: 9}
	case v >= -128 && v <= 127:
		return synthCol{serial: 1, body: []byte{byte(v)}}
	case v >= -32768 && v <= 32767:
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(int16(v)))
		return synthCol{serial: 2, body: b}
	default:
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(int32(v)))
		return synthCol{serial: 4, body: b}
	}
}

func synthRecord(cols ...synthCol) []byte {
	var types []byte
	for _, c := range cols {
		types = appendVarint(types, c.serial)
	}
	hdrLen := len(types) + 1
	for {
		n := len(appendVarint(nil, uint64(hdrLen)))
		if len(types)+n == hdrLen {
			break
		}
		hdrLen = len(types) + n
	}
	out := appendVarint(nil, uint64(hdrLen))
	out = append(out, types...)
	for _, c := range cols {
		out = append(out, c.body...)
	}
	return out
}

// ---------- 组装完整文件 ----------

type synthRow struct {
	key      string
	value    []byte
	isBlob   bool
	keyUTF16 bool // key/value 按 s.enc 编码（构造 UTF-16 库时用）
}

// buildItemDB 组装一个最小合法 SQLite 文件：
//
//	page 1  sqlite_master（一行：ItemTable, rootpage=2）
//	page 2  ItemTable 表叶子页，rows 按顺序放入
//	page 3+ rows 里超长 value 的 overflow 页
//
// 表结构用不带 UNIQUE 的简化版（无自动索引页）；本读取器不碰索引，文件仍是合法 SQLite。
func buildItemDB(t *testing.T, pageSize, enc int, rows ...synthRow) []byte {
	t.Helper()
	s := newSynth(pageSize, enc)
	_, p1 := s.newPage() // page 1: sqlite_master
	_, p2 := s.newPage() // page 2: ItemTable root

	keyBody := func(r synthRow) []byte {
		if r.keyUTF16 {
			return encodeUTF16(r.key, enc)
		}
		return []byte(r.key)
	}
	valBody := func(r synthRow) []byte {
		if r.keyUTF16 {
			return encodeUTF16(string(r.value), enc)
		}
		return r.value
	}

	cells := make([][]byte, 0, len(rows))
	for i, r := range rows {
		kc := textColRaw(keyBody(r))
		var vc synthCol
		if r.isBlob {
			vc = blobCol(r.value)
		} else {
			vc = textColRaw(valBody(r))
		}
		cells = append(cells, s.leafCell(int64(i+1), synthRecord(kc, vc)))
	}
	copy(p2, leafTablePage(t, pageSize, 0, cells))

	// sqlite_master 的 record：type, name, tbl_name, rootpage, sql。
	// schema 文本同样按库的文本编码存放（SQLite 就是这么写的）。
	encText := func(v string) synthCol {
		if enc == encUTF8 {
			return textCol(v)
		}
		return textColRaw(encodeUTF16(v, enc))
	}
	master := synthRecord(
		encText("table"), encText("ItemTable"), encText("ItemTable"),
		intCol(2), encText(itemTableSQL),
	)
	masterCell := s.leafCell(1, master)
	// 第 1 页的 b-tree 页头在第 100 字节之后；文件头就是第 1 页的前 100 字节。
	copy(p1, leafTablePage(t, pageSize, headerSize, [][]byte{masterCell}))
	copy(p1[:headerSize], s.header())
	return s.bytes()
}

// ---------- 小工具 ----------

func putU16(b []byte, off int, v int) {
	b[off] = byte(v >> 8)
	b[off+1] = byte(v)
}

func putU32(b []byte, off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// appendVarint 按 SQLite varint 规则编码（1-9 字节，大端 7 位一组，第 9 字节用满 8 位）。
func appendVarint(dst []byte, v uint64) []byte {
	if v <= 0x7f {
		return append(dst, byte(v))
	}
	if v > 0x00ffffffffffffff {
		// 9 字节形式：低 8 位单独占最后一字节
		var out [9]byte
		out[8] = byte(v)
		v >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return append(dst, out[:]...)
	}
	var tmp [10]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte(v & 0x7f)
		v >>= 7
	}
	for j := i; j < len(tmp)-1; j++ {
		tmp[j] |= 0x80
	}
	return append(dst, tmp[i:]...)
}

// encodeUTF16 把 s 编成文件头声明的 UTF-16 编码（含代理对）。
func encodeUTF16(s string, enc int) []byte {
	var out []byte
	put := func(u uint16) {
		if enc == encUTF16BE {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	for _, r := range s {
		if r > 0xffff {
			r -= 0x10000
			put(uint16(0xd800 + (r >> 10)))
			put(uint16(0xdc00 + (r & 0x3ff)))
			continue
		}
		put(uint16(r))
	}
	return out
}

// pattern 生成位置相关的字节序列，便于发现 overflow 链乱序/丢页。
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("写临时文件 %s: %v", p, err)
	}
	return p
}

func openSynth(t *testing.T, data []byte) *DB {
	t.Helper()
	db, err := Open(writeTemp(t, "state.vscdb", data))
	if err != nil {
		t.Fatalf("Open 手写 SQLite 文件失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustGet(t *testing.T, db *DB, key string) []byte {
	t.Helper()
	v, err := db.GetItem(key)
	if err != nil {
		t.Fatalf("GetItem(%q) 出错: %v", key, err)
	}
	return v
}

// ---------- 测试 ----------

// TestSyntheticMinimal 用最小合法文件（512 字节页，两条记录）验证基本读取。
func TestSyntheticMinimal(t *testing.T) {
	data := buildItemDB(t, 512, encUTF8,
		synthRow{key: "alpha", value: []byte("hello")},
		synthRow{key: "beta", value: []byte("world")},
	)
	if got := len(data); got != 2*512 {
		t.Fatalf("构造文件大小 %d，期望 %d", got, 2*512)
	}
	db := openSynth(t, data)

	if got := string(mustGet(t, db, "alpha")); got != "hello" {
		t.Fatalf("alpha = %q，期望 hello", got)
	}
	if got := string(mustGet(t, db, "beta")); got != "world" {
		t.Fatalf("beta = %q，期望 world", got)
	}
	if _, err := db.GetItem("gamma"); err != ErrNotFound {
		t.Fatalf("不存在的 key 应返回 ErrNotFound，实际 %v", err)
	}
	keys, err := db.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "alpha" || keys[1] != "beta" {
		t.Fatalf("Keys = %v，期望 [alpha beta]", keys)
	}
}

// TestSyntheticOverflowBoundaries 覆盖 overflow 链的各个分支：
// 不溢出、K 分支（局部 = K）、M 分支（局部 = M）、以及多页链。
func TestSyntheticOverflowBoundaries(t *testing.T) {
	const pageSize = 4096
	U := pageSize
	X := U - 35
	m := (U-12)*32/255 - 23

	cases := []struct {
		size  int
		note  string
		local int
	}{
		{0, "空 value（零长度 TEXT）", 0},
		{1, "1 字节", 1},
		{100, "远小于阈值", 100},
		{X, "恰好等于阈值 X，无溢出", X},
		{X + 1, "刚过阈值 -> M 分支", synthLocalSize(U, X+1)},
		{X + (U - 4), "K 分支：局部 = X，恰好一页溢出", synthLocalSize(U, X+(U-4))},
		{X + (U - 4) + 1, "刚过一页 -> M 分支，两页溢出", synthLocalSize(U, X+(U-4)+1)},
		{20000, "多页链", synthLocalSize(U, 20000)},
		{140220, "与真实 state.vscdb 同量级（135KB+）", synthLocalSize(U, 140220)},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			want := pattern(tc.size)
			data := buildItemDB(t, pageSize, encUTF8,
				synthRow{key: "before", value: []byte("x")},
				synthRow{key: "big", value: want},
				synthRow{key: "after", value: []byte("y")},
			)
			if got := synthLocalSize(U, tc.size); got != tc.local {
				t.Fatalf("构造器与实现的局部大小算法不一致：%d vs %d", got, tc.local)
			}
			t.Logf("payload=%d 局部=%d 溢出页=%d", tc.size, tc.local,
				overflowPages(U, tc.size, tc.local))

			db := openSynth(t, data)
			got := mustGet(t, db, "big")
			if len(got) != tc.size {
				t.Fatalf("长度 %d，期望 %d", len(got), tc.size)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("第 %d 字节 = %d，期望 %d", i, got[i], want[i])
				}
			}
			// 邻居记录必须仍然正确：证明 cell 指针数组没被 overflow 页打乱
			if v := string(mustGet(t, db, "before")); v != "x" {
				t.Fatalf("before = %q", v)
			}
			if v := string(mustGet(t, db, "after")); v != "y" {
				t.Fatalf("after = %q", v)
			}
		})
	}
	if synthLocalSize(U, X+1) == X+1 || synthLocalSize(U, X+(U-4)) != X {
		t.Fatalf("边界样例没有覆盖到 K/M 两个分支：X=%d m=%d", X, m)
	}
}

func overflowPages(U, payload, local int) int {
	rest := payload - local
	if rest <= 0 {
		return 0
	}
	return (rest + U - 5) / (U - 4)
}

// TestSyntheticInteriorPage 覆盖表内部页（0x05）遍历：root 是内部页，
// 两个子叶子页 + rightmost 指针都要走到。
func TestSyntheticInteriorPage(t *testing.T) {
	const pageSize = 512
	s := newSynth(pageSize, encUTF8)
	_, p1 := s.newPage() // sqlite_master
	_, p2 := s.newPage() // ItemTable root = 内部页
	_, p3 := s.newPage() // 左叶子
	_, p4 := s.newPage() // 右叶子（rightmost）

	leaf := func(rows [][2]string, firstRowid int64) []byte {
		cells := make([][]byte, 0, len(rows))
		for i, kv := range rows {
			cells = append(cells, s.leafCell(firstRowid+int64(i),
				synthRecord(textCol(kv[0]), textCol(kv[1]))))
		}
		return leafTablePage(t, pageSize, 0, cells)
	}
	copy(p3, leaf([][2]string{{"a1", "1"}, {"a2", "2"}}, 1))
	copy(p4, leaf([][2]string{{"b1", "3"}, {"b2", "4"}}, 10))
	copy(p2, interiorTablePage(t, pageSize, 0, []uint32{3, 4}, []int64{5}))

	master := synthRecord(textCol("table"), textCol("ItemTable"), textCol("ItemTable"),
		intCol(2), textCol(itemTableSQL))
	copy(p1, leafTablePage(t, pageSize, headerSize, [][]byte{s.leafCell(1, master)}))
	copy(p1[:headerSize], s.header())

	db := openSynth(t, s.bytes())
	for k, want := range map[string]string{"a1": "1", "a2": "2", "b1": "3", "b2": "4"} {
		if got := string(mustGet(t, db, k)); got != want {
			t.Fatalf("%s = %q，期望 %q", k, got, want)
		}
	}
	if _, err := db.GetItem("zzz"); err != ErrNotFound {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

// TestSyntheticPageSize65536 覆盖文件头 offset 16 写 1 表示 65536 的写法。
func TestSyntheticPageSize65536(t *testing.T) {
	const pageSize = 65536
	data := buildItemDB(t, pageSize, encUTF8, synthRow{key: "k", value: []byte("v")})
	if got := int(binary.BigEndian.Uint16(data[16:18])); got != 1 {
		t.Fatalf("offset 16 = %d，期望 1（表示 65536）", got)
	}
	db := openSynth(t, data)
	if db.pageSize != pageSize {
		t.Fatalf("解析出的页大小 %d，期望 %d", db.pageSize, pageSize)
	}
	if got := string(mustGet(t, db, "k")); got != "v" {
		t.Fatalf("k = %q", got)
	}
}

// TestSyntheticTextEncoding 覆盖 UTF-16LE / UTF-16BE 文本编码（含代理对）。
func TestSyntheticTextEncoding(t *testing.T) {
	const key = "键🔑key"
	const val = `{"名称":"模型","emoji":"🚀"}`
	for _, enc := range []int{encUTF8, encUTF16LE, encUTF16BE} {
		t.Run(map[int]string{encUTF8: "utf8", encUTF16LE: "utf16le", encUTF16BE: "utf16be"}[enc],
			func(t *testing.T) {
				data := buildItemDB(t, 1024, enc,
					synthRow{key: key, value: []byte(val), keyUTF16: enc != encUTF8},
				)
				db := openSynth(t, data)
				if got := string(mustGet(t, db, key)); got != val {
					t.Fatalf("value = %q，期望 %q", got, val)
				}
			})
	}
}

// TestSyntheticBlobValue 覆盖 value 列以 BLOB 存储的情形（VS Code 两种都写）。
func TestSyntheticBlobValue(t *testing.T) {
	raw := []byte{0x00, 0x01, 0xfe, 0xff, 'j', 's', 'o', 'n'}
	data := buildItemDB(t, 512, encUTF8,
		synthRow{key: "blob", value: raw, isBlob: true},
	)
	db := openSynth(t, data)
	got := mustGet(t, db, "blob")
	if len(got) != len(raw) {
		t.Fatalf("长度 %d，期望 %d", len(got), len(raw))
	}
	for i := range raw {
		if got[i] != raw[i] {
			t.Fatalf("第 %d 字节 = 0x%02x，期望 0x%02x", i, got[i], raw[i])
		}
	}
}

// TestSyntheticBrokenOverflowChain 溢出链被截断时必须报错而不是返回半截数据。
func TestSyntheticBrokenOverflowChain(t *testing.T) {
	data := buildItemDB(t, 4096, encUTF8,
		synthRow{key: "big", value: pattern(20000)},
	)
	// 页 3 是第一个溢出页（第 3 页的起点 = 2 个页大小），把它的「下一页」指针清零，
	// 让链在数据还没读完时就结束。
	off := 2 * 4096
	if data[off] == 0 && data[off+3] == 0 {
		t.Fatalf("预期页 3 的下一页指针非零，实际 % x", data[off:off+4])
	}
	putU32(data, off, 0)

	db := openSynth(t, data)
	_, err := db.GetItem("big")
	if err == nil {
		t.Fatal("截断的溢出链应报错")
	}
	if !isErr(err, ErrCorrupt) {
		t.Fatalf("错误应为 ErrCorrupt，实际 %v", err)
	}
}

// TestSyntheticNoItemTable 库里没有 ItemTable 时 Open 直接失败。
func TestSyntheticNoItemTable(t *testing.T) {
	const pageSize = 512
	s := newSynth(pageSize, encUTF8)
	_, p1 := s.newPage()
	master := synthRecord(textCol("table"), textCol("Other"), textCol("Other"),
		intCol(2), textCol("CREATE TABLE Other (a TEXT)"))
	copy(p1, leafTablePage(t, pageSize, headerSize, [][]byte{s.leafCell(1, master)}))
	copy(p1[:headerSize], s.header())

	if _, err := Open(writeTemp(t, "state.vscdb", s.bytes())); !isErr(err, ErrNoItemTable) {
		t.Fatalf("期望 ErrNoItemTable，实际 %v", err)
	}
}

// TestSyntheticNotSQLite 非 SQLite 文件与截断文件都返回 ErrNotSQLite。
func TestSyntheticNotSQLite(t *testing.T) {
	cases := map[string][]byte{
		"垃圾内容": make([]byte, 4096),
		"截断的头": []byte("SQLite format 3\x00abc"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(writeTemp(t, "state.vscdb", data)); !isErr(err, ErrNotSQLite) {
				t.Fatalf("期望 ErrNotSQLite，实际 %v", err)
			}
		})
	}
}

// TestSyntheticCorruptionNoPanic 抽样改坏一个合法文件（含 overflow 链）的每个字节，
// 保证解析器只会返回错误，绝不 panic——state.vscdb 随时可能被客户端写到一半。
func TestSyntheticCorruptionNoPanic(t *testing.T) {
	const pageSize = 512
	const stride = 3 // 逐字节太慢，抽样覆盖（含页头、cell、overflow 页与文件头）
	good := buildItemDB(t, pageSize, encUTF8,
		synthRow{key: "k1", value: pattern(2000)}, // 走 overflow 链
		synthRow{key: "k2", value: []byte("v2")},
	)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.vscdb")
	bad := make([]byte, len(good))
	opened, reads, mutated := 0, 0, 0
	for i := 0; i < len(good); i += stride {
		copy(bad, good)
		bad[i] ^= 0xff
		mutated++
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatalf("写文件: %v", err)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("第 %d 字节被改坏后 panic：%v", i, r)
				}
			}()
			db, err := Open(path)
			if err != nil {
				return
			}
			opened++
			defer db.Close()
			for _, k := range []string{"k1", "k2", "k3"} {
				_, _ = db.GetItem(k)
				reads++
			}
		}()
	}
	t.Logf("抽样破坏 %d 次（文件 %d 字节）：%d 次仍能打开，%d 次读取，全程无 panic",
		mutated, len(good), opened, reads)
}

// TestSyntheticNoSideEffects 读取过程绝不能改动文件、也不能生成 -wal/-shm/-journal。
func TestSyntheticNoSideEffects(t *testing.T) {
	data := buildItemDB(t, 4096, encUTF8,
		synthRow{key: "k", value: pattern(20000)},
	)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.vscdb")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.GetItem("k"); err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if _, err := db.Keys(); err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if len(before) != len(after) || string(before) != string(after) {
		t.Fatal("读取后文件内容发生了变化")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.vscdb" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("目录里多出了文件: %v", names)
	}
	// Close 之后继续用应返回 ErrClosed，而不是 panic。
	if _, err := db.GetItem("k"); !isErr(err, ErrClosed) {
		t.Fatalf("Close 之后期望 ErrClosed，实际 %v", err)
	}
}

// TestGetVarint / TestDecodeText 直接盯住两个最容易写错的底层函数。
func TestGetVarint(t *testing.T) {
	cases := []struct {
		in   []byte
		want uint64
		n    int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0x81, 0x00}, 128, 2},
		{[]byte{0x82, 0x24}, 292, 2},
		{[]byte{0xff, 0x7f}, 16383, 2},
		{[]byte{0x81, 0x80, 0x00}, 16384, 3},
		// 9 字节形式：前 8 字节各 7 位，第 9 字节 8 位
		{[]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, 1, 9},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 1<<64 - 1, 9},
	}
	for _, tc := range cases {
		got, n := getVarint(tc.in)
		if got != tc.want || n != tc.n {
			t.Fatalf("getVarint(% x) = (%d,%d)，期望 (%d,%d)", tc.in, got, n, tc.want, tc.n)
		}
	}
	if _, n := getVarint([]byte{0x80}); n != 0 {
		t.Fatal("续接位未闭合时应返回 n=0")
	}
	// 构造器与解析器必须互逆
	for _, v := range []uint64{0, 1, 127, 128, 292, 16383, 16384, 1 << 20, 1 << 40, 1<<64 - 1} {
		enc := appendVarint(nil, v)
		got, n := getVarint(enc)
		if got != v || n != len(enc) {
			t.Fatalf("varint 往返失败：%d -> % x -> (%d,%d)", v, enc, got, n)
		}
	}
}

func TestSerialLen(t *testing.T) {
	cases := map[uint64]int{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8, 7: 8, 8: 0, 9: 0, 10: 0, 11: 0, 12: 0, 13: 0, 14: 1, 15: 1, 16: 2}
	for st, want := range cases {
		if got := serialLen(st); got != want {
			t.Fatalf("serialLen(%d) = %d，期望 %d", st, got, want)
		}
	}
	if serialLen(1<<33) != -1 {
		t.Fatal("过大的 serial type 应返回 -1")
	}
}

func TestDecodeText(t *testing.T) {
	const s = "模型A🚀"
	le := encodeUTF16(s, encUTF16LE)
	be := encodeUTF16(s, encUTF16BE)
	if got := string(decodeText(le, encUTF16LE)); got != s {
		t.Fatalf("UTF-16LE 解码 = %q，期望 %q", got, s)
	}
	if got := string(decodeText(be, encUTF16BE)); got != s {
		t.Fatalf("UTF-16BE 解码 = %q，期望 %q", got, s)
	}
	raw := []byte(s)
	if got := string(decodeText(raw, encUTF8)); got != s {
		t.Fatalf("UTF-8 解码 = %q", got)
	}
}
