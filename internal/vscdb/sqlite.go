package vscdb

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// SQLite 常量。只覆盖读取表 b-tree 所需的部分。
const (
	magic      = "SQLite format 3\x00"
	headerSize = 100 // 数据库文件头长度

	pageTypeInteriorIndex = 0x02
	pageTypeInteriorTable = 0x05
	pageTypeLeafIndex     = 0x0a
	pageTypeLeafTable     = 0x0d

	// maxTreeDepth 限制 b-tree 递归深度。正常库深度为个位数，
	// 超过说明页指针成环（文件损坏），直接报错而不是栈溢出。
	maxTreeDepth = 64

	// maxPayload 是单个 record 的字节数上限，防止损坏的 varint 让 int 溢出。
	maxPayload = 1 << 30
)

// 文本编码，取值来自文件头 offset 56。
const (
	encUTF8    = 1
	encUTF16LE = 2
	encUTF16BE = 3
)

func be16(b []byte) int    { return int(b[0])<<8 | int(b[1]) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// page 返回第 pgno 页（1 起）的完整页内容，长度恒为 pageSize。
// 若该页在 WAL 中有已提交的新版本，返回 WAL 里的版本而不是主库文件里的旧版本。
// 每次调用都返回新分配的缓冲区，调用方可以安全持有其中任意切片。
func (db *DB) page(pgno uint32) ([]byte, error) {
	if db.f == nil {
		return nil, ErrClosed
	}
	if pgno == 0 || pgno > db.pageCount {
		return nil, fmt.Errorf("%w: 页号 %d 越界（可见页数 %d）", ErrCorrupt, pgno, db.pageCount)
	}
	if db.wal != nil {
		p, ok, err := db.wal.page(pgno)
		if err != nil {
			return nil, err
		}
		if ok {
			return p, nil
		}
	}
	buf := make([]byte, db.pageSize)
	if _, err := db.f.ReadAt(buf, int64(pgno-1)*int64(db.pageSize)); err != nil {
		return nil, fmt.Errorf("vscdb: 读页 %d: %w", pgno, err)
	}
	return buf, nil
}

// scanTable 深度优先遍历以 root 为根的整棵表 b-tree，对每个叶子 cell 的 record
// payload 调用 fn。fn 返回 errStop 即提前结束（其余错误原样上抛）。
func (db *DB) scanTable(root uint32, fn func(payload []byte) error) error {
	return db.scanPage(root, 0, fn)
}

func (db *DB) scanPage(pgno uint32, depth int, fn func([]byte) error) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%w: b-tree 深度超过 %d（页指针可能成环）", ErrCorrupt, maxTreeDepth)
	}
	p, err := db.page(pgno)
	if err != nil {
		return err
	}
	// 第 1 页的 b-tree 页头位于 100 字节文件头之后，其余页从 0 开始。
	base := 0
	if pgno == 1 {
		base = headerSize
	}
	if base+12 > len(p) {
		return fmt.Errorf("%w: 页 %d 过短", ErrCorrupt, pgno)
	}
	ncells := be16(p[base+3 : base+5])
	switch p[base] {
	case pageTypeInteriorTable:
		// 页头 12 字节，cell 指针数组紧随其后；每个 cell = 4 字节左子页号 + varint key。
		if err := checkPtrArray(p, base+12, ncells, db.usable); err != nil {
			return err
		}
		for i := 0; i < ncells; i++ {
			off := be16(p[base+12+2*i : base+14+2*i])
			if off+4 > db.usable {
				return fmt.Errorf("%w: 页 %d cell %d 偏移 %d 越界", ErrCorrupt, pgno, i, off)
			}
			if err := db.scanPage(be32(p[off:off+4]), depth+1, fn); err != nil {
				return err
			}
		}
		right := be32(p[base+8 : base+12])
		if right != 0 {
			if err := db.scanPage(right, depth+1, fn); err != nil {
				return err
			}
		}
		return nil
	case pageTypeLeafTable:
		// 页头 8 字节，cell 指针数组紧随其后。
		if err := checkPtrArray(p, base+8, ncells, db.usable); err != nil {
			return err
		}
		for i := 0; i < ncells; i++ {
			off := be16(p[base+8+2*i : base+10+2*i])
			payload, err := db.cellPayload(p, off, pgno, i)
			if err != nil {
				return err
			}
			if err := fn(payload); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: 页 %d 类型 0x%02x 不是表 b-tree 页（索引页 0x%02x/0x%02x 不在读取范围内）",
			ErrCorrupt, pgno, p[base], pageTypeInteriorIndex, pageTypeLeafIndex)
	}
}

// checkPtrArray 校验 cell 指针数组本身落在可用页空间内。
func checkPtrArray(p []byte, start, ncells, usable int) error {
	if ncells < 0 || start+2*ncells > usable || start+2*ncells > len(p) {
		return fmt.Errorf("%w: cell 指针数组越界（ncells=%d）", ErrCorrupt, ncells)
	}
	return nil
}

// cellPayload 解析一个表叶子 cell，返回该行的完整 record payload。
//
// cell 布局：varint(payload 总长 P) + varint(rowid) + 局部 payload + 可选 4 字节溢出页号。
// 局部大小按 SQLite 的规则计算（U = 可用页空间）：
//
//	X = U-35
//	M = (U-12)*32/255 - 23
//	K = M + (P-M) % (U-4)
//	P <= X        -> 全部局部，无溢出
//	K <= X        -> 局部 K 字节，其余在溢出链
//	否则          -> 局部 M 字节，其余在溢出链
//
// 溢出链：每页前 4 字节是下一页页号（0 表示链尾），其后 U-4 字节是数据。
// 135KB+ 的 value 必然走这条链，是读取模型目录的关键路径。
func (db *DB) cellPayload(p []byte, off int, pgno uint32, idx int) ([]byte, error) {
	bad := func() error {
		return fmt.Errorf("%w: 页 %d 第 %d 个 cell 偏移 %d 非法", ErrCorrupt, pgno, idx, off)
	}
	if off < 0 || off >= db.usable {
		return nil, bad()
	}
	payloadLen, n := getVarint(p[off:])
	if n == 0 {
		return nil, bad()
	}
	off += n
	if off >= db.usable { // 防损坏文件把偏移推到页外（下一步要切片，先挡住）
		return nil, bad()
	}
	if _, n = getVarint(p[off:]); n == 0 { // rowid，本包用不到
		return nil, bad()
	}
	off += n
	if off > db.usable {
		return nil, bad()
	}
	if payloadLen > maxPayload {
		return nil, fmt.Errorf("%w: 页 %d 第 %d 个 cell 的 payload 长度 %d 过大", ErrCorrupt, pgno, idx, payloadLen)
	}

	local := int(payloadLen)
	U := db.usable
	if payloadLen > uint64(U-35) {
		m := uint64((U-12)*32/255 - 23)
		k := m + (payloadLen-m)%uint64(U-4)
		if k <= uint64(U-35) {
			local = int(k)
		} else {
			local = int(m)
		}
	}
	if off+local > db.usable {
		return nil, bad()
	}

	// 无论是否溢出都拷贝一份：调用方拿到的 payload 与页缓冲解耦，
	// GetItem 返回的 value 切片才能安全地长期持有。
	buf := make([]byte, 0, local)
	buf = append(buf, p[off:off+local]...)
	if local < int(payloadLen) {
		if off+local+4 > db.usable {
			return nil, bad()
		}
		next := be32(p[off+local : off+local+4])
		if err := db.readOverflow(&buf, next, int(payloadLen)-local); err != nil {
			return nil, fmt.Errorf("页 %d 第 %d 个 cell: %w", pgno, idx, err)
		}
	}
	return buf, nil
}

// readOverflow 顺着溢出页链把剩余的 remaining 字节追加到 buf。
// 每轮至少消耗 1 字节，因此即使链成环也必然终止。
func (db *DB) readOverflow(buf *[]byte, pgno uint32, remaining int) error {
	for remaining > 0 {
		if pgno == 0 {
			return fmt.Errorf("%w: 溢出页链提前结束，还缺 %d 字节", ErrCorrupt, remaining)
		}
		p, err := db.page(pgno)
		if err != nil {
			return err
		}
		n := db.usable - 4
		if n > remaining {
			n = remaining
		}
		*buf = append(*buf, p[4:4+n]...)
		remaining -= n
		pgno = be32(p[0:4])
	}
	return nil
}

// getVarint 解析 SQLite varint：1-9 字节，大端，每字节 7 位，最高位为续接标志；
// 第 9 字节用满 8 位。返回 (值, 消耗字节数)，字节不足时返回 (0, 0)。
func getVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		c := b[i]
		v = v<<7 | uint64(c&0x7f)
		if c < 0x80 {
			return v, i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return v<<8 | uint64(b[8]), 9
}

// serialLen 返回某个 serial type 在 record body 中占用的字节数。
// 0/8/9/10/11 是零字节类型，返回 0；12 起偶数为 BLOB、奇数为 TEXT，长度 = (st-12)/2。
// 非法（过大）的 serial type 返回 -1。
func serialLen(st uint64) int {
	switch st {
	case 0, 8, 9, 10, 11:
		return 0
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return 3
	case 4:
		return 4
	case 5:
		return 6
	case 6, 7:
		return 8
	}
	if st < 12 || st > 1<<32 {
		return -1
	}
	return int(st-12) / 2
}

// parseRecord 解析一个 record，返回各列的 serial type 与原始字节（列序一致）。
// NULL 与整数 0/1 这类零字节列返回 nil 切片。
func parseRecord(payload []byte) ([]uint64, [][]byte, error) {
	hdrLen, n := getVarint(payload)
	if n == 0 || hdrLen < uint64(n) || hdrLen > uint64(len(payload)) {
		return nil, nil, fmt.Errorf("%w: record 头长度 %d 非法", ErrCorrupt, hdrLen)
	}
	var serials []uint64
	for off := n; off < int(hdrLen); {
		st, m := getVarint(payload[off:])
		if m == 0 {
			return nil, nil, fmt.Errorf("%w: record 头里的 serial type 截断", ErrCorrupt)
		}
		off += m
		serials = append(serials, st)
	}
	body := payload[hdrLen:]
	cols := make([][]byte, len(serials))
	pos := 0
	for i, st := range serials {
		l := serialLen(st)
		if l < 0 {
			return nil, nil, fmt.Errorf("%w: 非法 serial type %d", ErrCorrupt, st)
		}
		if pos+l > len(body) {
			return nil, nil, fmt.Errorf("%w: record body 截断（列 %d 需要 %d 字节）", ErrCorrupt, i, l)
		}
		if l > 0 {
			cols[i] = body[pos : pos+l]
		}
		pos += l
	}
	return serials, cols, nil
}

// isText 判断 serial type 是否 TEXT（12 起奇数）。
func isText(st uint64) bool { return st >= 13 && st%2 == 1 }

// textOf 把一列按 TEXT 解释成 Go 字符串（非 UTF-8 编码会转成 UTF-8）。
// 列不是 TEXT（NULL/INT/REAL/BLOB）时按原始字节比较。
func textOf(st uint64, b []byte, enc int) string {
	if b == nil {
		return ""
	}
	if isText(st) {
		return string(decodeText(b, enc))
	}
	return string(b)
}

// valueOf 把 value 列转成调用方要的字节：TEXT 按文件编码转 UTF-8，BLOB 原样返回。
func valueOf(st uint64, b []byte, enc int) []byte {
	if b == nil {
		return nil
	}
	if isText(st) {
		return decodeText(b, enc)
	}
	return b
}

// intOf 按 serial type 把整数列解析成 int64（BLOB/TEXT 列返回 0）。
func intOf(st uint64, b []byte) int64 {
	if b == nil {
		switch st {
		case 8:
			return 0
		case 9:
			return 1
		}
		return 0
	}
	switch st {
	case 1:
		return int64(int8(b[0]))
	case 2:
		return int64(int16(binary.BigEndian.Uint16(b)))
	case 3:
		v := int64(b[0])<<16 | int64(b[1])<<8 | int64(b[2])
		if v&0x800000 != 0 {
			v -= 1 << 24
		}
		return v
	case 4:
		return int64(int32(binary.BigEndian.Uint32(b)))
	case 5: // 6 字节
		v := int64(b[0])<<40 | int64(b[1])<<32 | int64(b[2])<<24 |
			int64(b[3])<<16 | int64(b[4])<<8 | int64(b[5])
		if v&(1<<47) != 0 {
			v -= 1 << 48
		}
		return v
	case 6:
		return int64(binary.BigEndian.Uint64(b))
	}
	return 0
}

// decodeText 按文件头声明的文本编码把 TEXT 列的原始字节转成 UTF-8。
// enc 为 encUTF8 时原样返回（不拷贝）；UTF-16 时按小/大端组装并解码代理对。
func decodeText(b []byte, enc int) []byte {
	if enc == encUTF8 || len(b) == 0 {
		return b
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		if enc == encUTF16BE {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return []byte(string(utf16.Decode(u)))
}
