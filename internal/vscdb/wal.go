package vscdb

import (
	"encoding/binary"
	"fmt"
	"os"
)

// WAL（Write-Ahead Log）只读支持。
//
// 背景：Trae 客户端把 state.vscdb 打开在 WAL 模式下时，最新数据可能只存在于
// state.vscdb-wal 里，主库文件还停留在上一次 checkpoint 的状态。此时若只看主库，
// 就会读到陈旧数据（例如模型目录还是旧版本）。本文件按 SQLite 的 WAL 格式解析出
// 每个页号最新**已提交**的版本，让 DB.page 优先采用它。
//
// 严格只读：只以 os.Open（O_RDONLY）打开 -wal，绝不创建、截断、checkpoint 或删除它，
// 也绝不创建 -shm（-shm 只是多进程共享的索引，本包自行解析 WAL，不需要它）。
//
// 已知限制：
//   - WAL 头校验失败、盐值不匹配、帧校验失败时，整个 WAL 被忽略并回退到主库文件
//     （与 SQLite 的行为一致），此时若主库确实落后就会读到陈旧数据。绝不因此报错，
//     更不会去修复/删除 WAL 文件。
//   - 不处理 -journal（回滚日志）与 checkpoint 竞争；不保证跨页的强一致快照。
//   - 只在文件头读写版本为 2（WAL 模式）时才认 -wal，避免误用残留文件。

const (
	walMagic1       = 0x377f0682
	walMagic2       = 0x377f0683
	walFormat       = 3007000
	walHeaderSize   = 32
	walFrameHdrSize = 24
)

// walIndex 是 -wal 文件的只读索引：页号 -> 该页最新已提交帧的数据偏移。
type walIndex struct {
	f        *os.File
	pageSize int
	frames   map[uint32]int64
	commit   uint32 // 最后一个提交帧记录的数据库页数；0 表示 WAL 无有效提交
}

// page 返回 pgno 在 WAL 中的已提交版本。ok=false 表示 WAL 里没有这一页。
func (w *walIndex) page(pgno uint32) ([]byte, bool, error) {
	off, ok := w.frames[pgno]
	if !ok {
		return nil, false, nil
	}
	buf := make([]byte, w.pageSize)
	if _, err := w.f.ReadAt(buf, off); err != nil {
		return nil, false, fmt.Errorf("vscdb: 读 WAL 页 %d: %w", pgno, err)
	}
	return buf, true, nil
}

// Close 关闭 WAL 文件句柄。
func (w *walIndex) Close() error { return w.f.Close() }

// openWAL 尝试把 path 解析成有效的 WAL 索引。任何异常（不存在、过短、magic/校验
// 不匹配、页大小不符）都返回 nil，让调用方安静地回退到主库文件。
func openWAL(path string, pageSize int) *walIndex {
	f, err := os.Open(path) // 只读；不创建、不加锁
	if err != nil {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() < int64(walHeaderSize+walFrameHdrSize+pageSize) {
		_ = f.Close()
		return nil
	}
	hdr := make([]byte, walHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		_ = f.Close()
		return nil
	}
	magic := binary.BigEndian.Uint32(hdr[0:4])
	if magic != walMagic1 && magic != walMagic2 {
		_ = f.Close()
		return nil
	}
	if binary.BigEndian.Uint32(hdr[4:8]) != walFormat {
		_ = f.Close()
		return nil
	}
	if int(binary.BigEndian.Uint32(hdr[8:12])) != pageSize {
		_ = f.Close()
		return nil
	}
	// magic 最低位表示校验和按大端还是本机字节序计算：SQLite 用
	// (magic&1)==SQLITE_BIGENDIAN 判定，x86 上 SQLITE_BIGENDIAN==0，
	// 故 magic 0x...83 表示按大端字读取。
	bigEnd := magic&1 == 1
	ck0, ck1 := walChecksum(bigEnd, hdr[0:24], 0, 0)
	if ck0 != binary.BigEndian.Uint32(hdr[24:28]) || ck1 != binary.BigEndian.Uint32(hdr[28:32]) {
		// WAL 头不可信：客户端可能刚创建/正在写。忽略之，绝不改动文件。
		_ = f.Close()
		return nil
	}
	salt1 := binary.BigEndian.Uint32(hdr[16:20])
	salt2 := binary.BigEndian.Uint32(hdr[20:24])

	w := &walIndex{f: f, pageSize: pageSize, frames: make(map[uint32]int64)}
	buf := make([]byte, walFrameHdrSize+pageSize)
	// pending 收集「尚未提交」的帧；只有遇到 nTruncate != 0 的提交帧才并入 frames，
	// 保证读到的是最后一个完整事务，而不是写到一半的内容。
	var pending []frameRef
	for off := int64(walHeaderSize); off+int64(len(buf)) <= fi.Size(); off += int64(len(buf)) {
		if _, err := f.ReadAt(buf, off); err != nil {
			break
		}
		// 帧头的盐值必须与 WAL 头一致，否则说明 WAL 已被重启/截断，后面的都不算数。
		if binary.BigEndian.Uint32(buf[8:12]) != salt1 || binary.BigEndian.Uint32(buf[12:16]) != salt2 {
			break
		}
		// 校验和是跨帧累加的：先算帧头前 8 字节（页号 + 提交页数），再算整页数据。
		c0, c1 := walChecksum(bigEnd, buf[0:8], ck0, ck1)
		c0, c1 = walChecksum(bigEnd, buf[walFrameHdrSize:], c0, c1)
		if c0 != binary.BigEndian.Uint32(buf[16:20]) || c1 != binary.BigEndian.Uint32(buf[20:24]) {
			break
		}
		ck0, ck1 = c0, c1
		pending = append(pending, frameRef{
			pgno: binary.BigEndian.Uint32(buf[0:4]),
			off:  off + walFrameHdrSize,
		})
		if n := binary.BigEndian.Uint32(buf[4:8]); n != 0 {
			for _, fr := range pending {
				w.frames[fr.pgno] = fr.off
			}
			pending = pending[:0]
			w.commit = n
		}
	}
	if w.commit == 0 {
		// 没有任何完整提交：WAL 不提供有效数据，回退主库。
		_ = f.Close()
		return nil
	}
	return w
}

type frameRef struct {
	pgno uint32
	off  int64
}

// walChecksum 是 SQLite walChecksumBytes 的等价实现：按 8 字节一组迭代，
//
//	s0 += x + s1;  s1 += y + s0
//
// bigEnd 为真时按大端读取 32 位字，否则按小端。s0/s1 为初始值（跨帧累加）。
func walChecksum(bigEnd bool, data []byte, s0, s1 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(data); i += 8 {
		var x, y uint32
		if bigEnd {
			x = binary.BigEndian.Uint32(data[i : i+4])
			y = binary.BigEndian.Uint32(data[i+4 : i+8])
		} else {
			x = binary.LittleEndian.Uint32(data[i : i+4])
			y = binary.LittleEndian.Uint32(data[i+4 : i+8])
		}
		s0 += x + s1
		s1 += y + s0
	}
	return s0, s1
}
