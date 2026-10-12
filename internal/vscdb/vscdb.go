// Package vscdb 是零依赖的最小只读 SQLite 读取器，专门用于从 Trae 桌面客户端的
// state.vscdb 中读取模型目录（表 ItemTable，key 形如
// "916348159159136_AI.agent.model.model_list_map"，value 可达 135KB+）。
//
// 设计约束（改动时务必保持）：
//   - 只用 Go 标准库。禁止引入 modernc.org/sqlite / mattn/go-sqlite3 / cgo。
//   - 只读：以 O_RDONLY 打开，不写、不加锁、不创建 -wal/-shm/-journal。
//     Trae 客户端可能正在运行并持有该文件，本包的任何操作都不得改变磁盘状态。
//   - leaf package：不 import 仓库内其他包。
//
// 支持范围：
//   - SQLite 文件头解析（page size，含 header offset 16 值为 1 表示 65536 的写法）；
//   - 表 b-tree 遍历（interior 0x05 / leaf 0x0d），经 sqlite_master（root page 1）
//     定位 ItemTable 的 rootpage；
//   - record 格式解析（varint、serial type，NULL/INT/REAL/TEXT/BLOB）；
//   - overflow page 链：大 value 必然走这条链，见 sqlite.go 的 cellPayload；
//   - WAL：存在 <path>-wal 时解析其已提交帧并优先采用，避免读到陈旧数据（wal.go）；
//   - 文本编码 UTF-8 / UTF-16LE / UTF-16BE（按文件头 offset 56 判定）。
//
// 已知限制：
//   - 不解析 rollback journal（-journal）。若数据库处于热日志状态（客户端上次崩溃、
//     日志尚未回滚），读到的是未回滚的中间态；需要强一致时请先退出客户端。
//   - 不校验 schema，只按 sqlite_master 里记录的 rootpage 遍历；不使用 key 上的自动
//     索引（GetItem 走全表扫描，state.vscdb 只有几百行，开销可忽略）。
//   - 不加锁，也不做多页快照：客户端正在写入时，读到的是某一时刻的近似一致视图。
//   - Close 不可与其它方法并发调用；除此之外 DB 可并发使用。
package vscdb

import (
	"errors"
	"fmt"
	"os"
)

// 本包对外暴露的错误。调用方用 errors.Is 判定。
var (
	// ErrNotSQLite 目标文件不是 SQLite 3 数据库（magic 不匹配或文件头被截断）。
	ErrNotSQLite = errors.New("vscdb: 不是 SQLite 3 数据库文件")
	// ErrNotFound 由 GetItem 在 key 不存在时返回。
	ErrNotFound = errors.New("vscdb: key 不存在")
	// ErrNoItemTable 库里没有名为 ItemTable 的表（不是 VS Code 状态库）。
	ErrNoItemTable = errors.New("vscdb: 未找到 ItemTable")
	// ErrCorrupt 文件头合法但页/record 结构自相矛盾。
	ErrCorrupt = errors.New("vscdb: 数据库文件损坏")
	// ErrClosed 在 Close 之后继续调用方法。
	ErrClosed = errors.New("vscdb: DB 已关闭")
)

// errStop 是内部信号，用于让 scanTable 提前结束遍历，不对外暴露。
var errStop = errors.New("vscdb: stop scan")

// DB 是一个只读的 SQLite 数据库句柄，专为 state.vscdb 的表扫描而实现。
type DB struct {
	path string
	f    *os.File

	pageSize int // 页大小；文件头写 1 时归一化为 65536
	usable   int // pageSize - reserved（reserved 见文件头 offset 20）
	enc      int // 文本编码：1=UTF-8，2=UTF-16LE，3=UTF-16BE

	pageCount uint32 // 可见页数；存在有效 WAL 时取最后一个提交帧记录的页数
	root      uint32 // ItemTable 的 rootpage（来自 sqlite_master）

	wal *walIndex // 可能为 nil
}

// Open 以只读方式打开 state.vscdb。调用方负责 Close。
//
// 打开过程不做任何写操作、不加文件锁、不创建 -wal/-shm：客户端正在运行时也可以安全调用。
// 若同目录存在 <path>-wal 且文件头版本号表明数据库处于 WAL 模式，会解析 WAL 中已提交的
// 帧并优先采用（详见 wal.go 的限制说明）。
//
// Open 会立即读取文件头、解析 sqlite_master 并定位 ItemTable 的 rootpage，
// 因此下面这些错误在 Open 阶段就会返回：ErrNotSQLite、ErrCorrupt、ErrNoItemTable。
func Open(path string) (*DB, error) {
	// os.Open 即 O_RDONLY：Windows 上 Go 以 FILE_SHARE_READ|WRITE|DELETE 打开，
	// 不会独占也不会给客户端加锁。
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vscdb: 打开 %s: %w", path, err)
	}
	db := &DB{path: path, f: f}
	if err := db.init(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return db, nil
}

// Path 返回打开时使用的路径（诊断用）。
func (db *DB) Path() string { return db.path }

// Close 关闭底层文件句柄（含 WAL 句柄）。可重复调用。
func (db *DB) Close() error {
	var err error
	if db.wal != nil {
		err = db.wal.Close()
		db.wal = nil
	}
	if db.f != nil {
		if e := db.f.Close(); err == nil {
			err = e
		}
		db.f = nil
	}
	return err
}

// GetItem 等价于 SELECT value FROM ItemTable WHERE key = ?。
//
// 返回语义（重要）：
//   - 命中：返回该行 value 的字节，err == nil。TEXT 列会按文件头声明的编码转成 UTF-8，
//     BLOB 列原样返回。返回的切片归调用方所有，可自由持有与修改。
//   - 未命中：返回 (nil, ErrNotFound)，用 errors.Is(err, ErrNotFound) 判定。
//   - 命中但 value 为 NULL 或零长度：返回 (nil, nil)。
//
// 实现为 ItemTable 的全表扫描（含 overflow page 链），不使用 key 上的自动索引。
func (db *DB) GetItem(key string) ([]byte, error) {
	if db.f == nil {
		return nil, ErrClosed
	}
	var (
		val   []byte
		found bool
	)
	err := db.scanTable(db.root, func(payload []byte) error {
		serials, cols, err := parseRecord(payload)
		if err != nil {
			return err
		}
		if len(cols) < 2 {
			return nil // ItemTable 固定两列；不是就不可能是目标行
		}
		if textOf(serials[0], cols[0], db.enc) != key {
			return nil
		}
		val = valueOf(serials[1], cols[1], db.enc)
		found = true
		return errStop
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return val, nil
}

// Keys 返回 ItemTable 里全部 key（诊断/调试用，同样走全表扫描）。
// 若只想确认库能不能读通，用它比逐个 GetItem 更省事。
func (db *DB) Keys() ([]string, error) {
	if db.f == nil {
		return nil, ErrClosed
	}
	var keys []string
	err := db.scanTable(db.root, func(payload []byte) error {
		serials, cols, err := parseRecord(payload)
		if err != nil {
			return err
		}
		if len(cols) < 1 {
			return nil
		}
		keys = append(keys, textOf(serials[0], cols[0], db.enc))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// init 解析文件头、挂上 WAL、并定位 ItemTable 的 rootpage。
func (db *DB) init() error {
	hdr := make([]byte, headerSize)
	if _, err := db.f.ReadAt(hdr, 0); err != nil {
		return fmt.Errorf("%w: 读取 100 字节文件头失败: %v", ErrNotSQLite, err)
	}
	if string(hdr[0:16]) != magic {
		return ErrNotSQLite
	}

	// offset 16：页大小（大端 uint16）。写 1 表示 65536（该写法只可能出现在
	// 页大小确实是 65536 的文件里），其余必须是 512 的 2 的幂。
	ps := int(hdr[16])<<8 | int(hdr[17])
	switch {
	case ps == 1:
		ps = 65536
	case ps == 0:
		return fmt.Errorf("%w: 文件头页大小为 0", ErrCorrupt)
	case ps < 512 || ps&(ps-1) != 0:
		return fmt.Errorf("%w: 非法页大小 %d", ErrCorrupt, ps)
	}
	db.pageSize = ps

	// offset 20：每页末尾保留字节数（加密/校验用），可用空间 = 页大小 - reserved。
	db.usable = ps - int(hdr[20])
	if db.usable < 480 {
		return fmt.Errorf("%w: 可用页空间 %d 过小", ErrCorrupt, db.usable)
	}

	// offset 56：文本编码。0 视为 UTF-8（空库的默认值）。
	db.enc = int(hdr[56])<<24 | int(hdr[57])<<16 | int(hdr[58])<<8 | int(hdr[59])
	if db.enc == 0 {
		db.enc = encUTF8
	}
	if db.enc != encUTF8 && db.enc != encUTF16LE && db.enc != encUTF16BE {
		return fmt.Errorf("%w: 未知文本编码 %d", ErrCorrupt, db.enc)
	}

	// offset 28：数据库页数。WAL 模式下它可能陈旧，故随后用 WAL 的提交页数覆盖。
	hdrPages := be32(hdr[28:32])
	sizePages := uint32(0)
	if fi, err := db.f.Stat(); err == nil && fi.Size() > 0 {
		sizePages = uint32(fi.Size() / int64(ps))
	}
	db.pageCount = hdrPages
	if db.pageCount == 0 || db.pageCount > sizePages {
		db.pageCount = sizePages
	}

	// offset 18/19：文件格式读写版本，2 表示 WAL 模式。只有处于 WAL 模式时才认 -wal，
	// 否则残留的 WAL 文件会被误当成有效数据源（SQLite 自身也是这么判定的）。
	if hdr[18] == 2 || hdr[19] == 2 {
		if w := openWAL(db.path+"-wal", ps); w != nil {
			db.wal = w
			if w.commit > 0 {
				db.pageCount = w.commit
			}
		}
	}

	root, err := db.findItemTable()
	if err != nil {
		return err
	}
	db.root = root
	return nil
}

// findItemTable 扫描 sqlite_master（root page 1），返回 ItemTable 的 rootpage。
// sqlite_master 的列序为 type, name, tbl_name, rootpage, sql。
func (db *DB) findItemTable() (uint32, error) {
	var (
		root  uint32
		found bool
	)
	err := db.scanTable(1, func(payload []byte) error {
		serials, cols, err := parseRecord(payload)
		if err != nil {
			return err
		}
		if len(cols) < 4 {
			return nil
		}
		if textOf(serials[0], cols[0], db.enc) != "table" {
			return nil
		}
		if textOf(serials[1], cols[1], db.enc) != "ItemTable" {
			return nil
		}
		root = uint32(intOf(serials[3], cols[3]))
		found = true
		return errStop
	})
	if err != nil && !errors.Is(err, errStop) {
		return 0, err
	}
	if !found || root == 0 {
		return 0, ErrNoItemTable
	}
	return root, nil
}
