// Package accountstore 管理便携凭据快照的落盘存储（data/accounts/*.json）。
//
// 定位：这是「网关机器上不装 Trae 客户端」的部署方式的基础 ——
// 把别的机器上导出的快照拷进来，网关就能直接使用与续期。
//
// 与 config.trae.accounts 的关系：
//   - config 里配 storage_path 的，指向**客户端 storage.json**（活的，会写回）
//   - 本存储里的，是**网关自己拥有的快照**（导出/导入的产物，也写回自己）
//
// 两者最终都变成一个 traectl.Store，对上层无差别。
package accountstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"trae2api/internal/traectl"
)

// Store 是快照目录的句柄。并发安全。
type Store struct {
	dir string

	mu sync.RWMutex
}

// Entry 是目录里的一份快照。
type Entry struct {
	// Name 文件名（不含目录），也是删除时用的标识。
	Name string
	// Path 完整路径。
	Path string
	// Snap 解析后的快照。
	Snap *traectl.CredentialSnapshot
}

// New 构造（dir 为空表示禁用持久化，所有操作在内存语义下无操作）。
func New(dir string) *Store { return &Store{dir: dir} }

// Dir 返回存储目录。
func (s *Store) Dir() string { return s.dir }

// safeNameRe 是文件名的白名单过滤：只保留字母、数字、下划线、连字符。
//
// 刻意**不**保留点号：否则 userId 里的 `../` 会被净化成 `.._.._`（虽然已无穿越风险，
// 但会留下含 `..` 的怪名）。去掉点号后 `../../evil` → `______evil`，干净且无歧义。
var safeNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// snapshotFileName 由产品与 userId 派生稳定文件名。
//
// 用 userId 而不是随机名：同一账号重复导入会**覆盖**而不是堆积副本 ——
// refreshToken 是轮换语义，留两份必然有一份失效，覆盖才是对的。
func snapshotFileName(snap *traectl.CredentialSnapshot) string {
	uid := safeNameRe.ReplaceAllString(snap.UserID, "_")
	if uid == "" {
		uid = "unknown"
	}
	prod := safeNameRe.ReplaceAllString(snap.Product, "_")
	return fmt.Sprintf("%s-%s.json", prod, uid)
}

// List 返回目录里的全部快照（按文件名排序，稳定顺序）。
//
// 单个文件损坏只跳过并记在 skipped 里，不影响其余 —— 导入目录里混进坏文件
// 是很常见的，不该让整个账号池起不来。
func (s *Store) List() (entries []Entry, skipped []string) {
	if s.dir == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	items, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("读取目录失败: %v", err)}
	}
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(strings.ToLower(it.Name()), ".json") {
			continue
		}
		path := filepath.Join(s.dir, it.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", it.Name(), err))
			continue
		}
		var snap traectl.CredentialSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: 不是合法 JSON", it.Name()))
			continue
		}
		snap.Normalize()
		if err := snap.Validate(); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", it.Name(), err))
			continue
		}
		entries = append(entries, Entry{Name: it.Name(), Path: path, Snap: &snap})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, skipped
}

// Add 写入一份快照（同名覆盖），返回文件名。
func (s *Store) Add(snap *traectl.CredentialSnapshot) (string, error) {
	if s.dir == "" {
		return "", fmt.Errorf("accountstore: 未配置存储目录")
	}
	snap.Normalize()
	if err := snap.Validate(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 0o700 是 POSIX 意图（目录内含 refreshToken 与设备私钥）。
	// ⚠️ Windows 上 Go 不映射 POSIX 权限位（mkdir 的 perm 参数被忽略），
	// 实际保护依赖用户目录的 ACL 继承 —— 也就是说 Windows 下这里**没有**
	// 额外的权限收紧。这是平台限制，不是实现疏漏。
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", fmt.Errorf("accountstore: 创建目录: %w", err)
	}
	name := snapshotFileName(snap)
	if err := traectl.WriteSnapshot(filepath.Join(s.dir, name), snap); err != nil {
		return "", err
	}
	return name, nil
}

// Delete 删除一份快照。
func (s *Store) Delete(name string) error {
	if s.dir == "" {
		return fmt.Errorf("accountstore: 未配置存储目录")
	}
	// 防路径穿越：只允许纯文件名
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("accountstore: 非法文件名 %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("accountstore: 快照不存在: %s", name)
	}
	return os.Remove(path)
}

// Count 返回有效快照数量。
func (s *Store) Count() int {
	entries, _ := s.List()
	return len(entries)
}
