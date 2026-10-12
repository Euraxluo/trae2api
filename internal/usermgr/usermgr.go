// Package usermgr 实现多用户与角色权限管理（RBAC）。
//
// 设计取舍（借鉴 workbuddy2api 的多租户思路，独立实现）：
//
//   - **API Key 即身份**：每个用户一个专属 Key，请求带上它即可确定"我是谁"。
//     不引入会话/Cookie —— 本地工具不需要，且能避免 CSRF 一类问题。
//   - **owner 隔离**：普通用户只能调度 `owner` 为空（public）或等于自己 ID/用户名的
//     账号；管理员不受限。这与 pool 的 owner 过滤是同一套语义，两处必须一致。
//   - **内存优先**：落盘失败（只读目录等）不影响已创建用户在内存中生效 ——
//     与 workbuddy2api 的「内存优先与磁盘容错自愈」同一考虑：宁可丢持久化也不能
//     让用户创建后无法立即使用。
package usermgr

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Role 是用户角色。
type Role string

const (
	// RoleAdmin 管理员：可调度全部账号，可访问管理端点。
	RoleAdmin Role = "admin"
	// RoleUser 普通用户：只能调度 public 账号与自己 owner 的账号。
	RoleUser Role = "user"
)

// Valid 报告角色是否合法。
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleUser }

// ErrUserExists 表示用户名已存在。
//
// 为什么要有这个哨兵错误：调用方需要把"重名"映射成 **409 Conflict**
// 而不是 400 —— 靠匹配错误字符串做这件事既脆又容易漏。用 errors.Is 判定。
var ErrUserExists = errors.New("用户名已存在")

// User 是一个用户。
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	APIKey    string `json:"api_key"`
	Role      Role   `json:"role"`
	CreatedAt string `json:"created_at"`
	Disabled  bool   `json:"disabled"`
	// Note 备注（便于识别用途，如"给 Cline 用"）。
	Note string `json:"note,omitempty"`

	// Quota 总额度（Token 计数）。-1 或 0 表示无限制。
	Quota int64 `json:"quota,omitempty"`
	// UsedQuota 已消耗额度（Token 计数）。
	UsedQuota int64 `json:"used_quota,omitempty"`
	// ModelWhitelist 允许调用的模型列表。空数组或 nil 表示允许调用全部模型。
	ModelWhitelist []string `json:"model_whitelist,omitempty"`

	// ---- 密码（PBKDF2-HMAC-SHA256）----
	//
	// PasswordHash 是派生密钥的 hex；**空字符串表示该用户没有密码**。
	//
	// 老用户（本次改动之前创建的、只有 API Key 的）就是空 —— 他们**不能**用
	// 密码登录，只能用 API Key，直到管理员用 /admin/users/{id}/reset-password
	// 给他们设一个。刻意不做"默认密码"（那是纯粹的安全漏洞，等于给所有存量
	// 账号配一个公开口令），也不做"强制迁移"（会让存量客户端在升级瞬间全部失效）。
	PasswordHash string `json:"password_hash,omitempty"`
	// Salt 是**每用户独立**的随机盐（hex，16 字节）。
	//
	// 为什么必须每用户一个而不是全局一个：全局盐下，两个用同一密码的用户会得到
	// 完全相同的哈希，攻击者拿到 users.json 后可以一眼看出"这些人密码一样"，
	// 而且一份彩虹表就能同时打穿所有人。
	Salt string `json:"salt,omitempty"`
	// PasswordIter 是派生时实际用的迭代次数。
	//
	// 为什么要落盘而不是写死读常量：将来调高迭代次数时，**老哈希仍然能校验**
	// （用记录里的旧次数），不需要让所有人重设密码。0 表示记录里没有这个字段
	// （早期数据），按当前常量处理。
	PasswordIter int `json:"password_iter,omitempty"`
}

// Manager 是用户管理器。
type Manager struct {
	mu   sync.RWMutex
	path string
	// settingsPath 是系统级设置的落盘路径（见 AllowRegister）。
	//
	// 为什么和 users.json 分开：users.json 的格式是**裸数组**（[User,...]），
	// 要往里面塞一个 {"allow_register":...,"users":[...]} 就得改格式 ——
	// 那会让升级前的文件读不出来（等于升级即丢全部用户与 API Key）。
	// 分一个文件存设置，用户表格式一个字节都不用动。
	settingsPath string
	// allowRegister 是否开放自助注册。
	//
	// 默认 **false（关闭）**：注册是"任何人都能给自己开一个账号"的入口，
	// 默认必须关。只有管理员显式打开才生效。
	allowRegister bool
	// byID / byKey / byName 三个索引指向同一批 *User，修改时必须同时维护。
	byID   map[string]*User
	byKey  map[string]*User
	byName map[string]*User
}

// New 构造空管理器（path 为空则纯内存，不落盘）。
func New(path string) *Manager {
	m := &Manager{
		path:   path,
		byID:   map[string]*User{},
		byKey:  map[string]*User{},
		byName: map[string]*User{},
	}
	if path != "" {
		// 系统设置放在用户表**旁边**（同一数据目录），便于一起备份/迁移。
		m.settingsPath = filepath.Join(filepath.Dir(path), "system.json")
	}
	return m
}

// Load 从文件加载用户与系统设置；文件不存在视为空（首次运行）。
//
// ⚠️ 用户表与系统设置是**两个文件**，且用户表的读取失败必须照旧返回错误
// （调用方需要知道"用户没加载上"）。系统设置则相反 —— 它的失败被刻意吞掉，
// 理由见 loadSettings。
func (m *Manager) Load() error {
	m.loadSettings()
	if m.path == "" {
		return nil
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("usermgr: 读取 %s: %w", m.path, err)
	}
	var users []User
	if err := json.Unmarshal(raw, &users); err != nil {
		return fmt.Errorf("usermgr: 解析 %s: %w", m.path, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range users {
		u := users[i]
		if u.ID == "" || u.APIKey == "" {
			continue // 跳过残缺条目（不因此让整个加载失败）
		}
		if !u.Role.Valid() {
			u.Role = RoleUser
		}
		m.index(&u)
	}
	return nil
}

// index 建立索引（调用方持锁）。
func (m *Manager) index(u *User) {
	m.byID[u.ID] = u
	m.byKey[u.APIKey] = u
	if u.Username != "" {
		m.byName[strings.ToLower(u.Username)] = u
	}
}

// unindex 移除索引（调用方持锁）。
func (m *Manager) unindex(u *User) {
	delete(m.byID, u.ID)
	delete(m.byKey, u.APIKey)
	if u.Username != "" {
		delete(m.byName, strings.ToLower(u.Username))
	}
}

// Save 落盘（原子写）。可独立调用 —— 自行加读锁。
func (m *Manager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.saveLocked()
}

// saveLocked 落盘的内部实现：**调用方必须已持锁**（读锁或写锁均可）。
//
// 为什么拆成两个函数：sync.RWMutex 不可重入。Create/Delete/ResetKey/SetDisabled
// 都持有写锁，若它们直接调用 Save()（会再拿读锁）就会**自死锁** ——
// 这个 bug 是被单元测试挂住发现的，不是靠读代码看出来的。
func (m *Manager) saveLocked() error {
	if m.path == "" {
		return nil
	}
	users := make([]User, 0, len(m.byID))
	for _, u := range m.byID {
		users = append(users, *u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })

	b, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("usermgr: 序列化: %w", err)
	}
	dir := filepath.Dir(m.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("usermgr: 创建目录: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".users-*.tmp")
	if err != nil {
		return fmt.Errorf("usermgr: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("usermgr: 写临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("usermgr: 关闭临时文件: %w", err)
	}
	if err := os.Rename(name, m.path); err != nil {
		os.Remove(m.path)
		if err2 := os.Rename(name, m.path); err2 != nil {
			os.Remove(name)
			return fmt.Errorf("usermgr: 替换 %s: %w", m.path, err2)
		}
	}
	return nil
}

// Count 返回用户数。
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// FindByAPIKey 按 API Key 查用户（**常量时间比较**，避免时序侧信道爆破）。
//
// 由于 map 查找本身不是常量时间，这里对全部 key 做一次常量时间比较。
// 用户数量很小（本地工具），开销可忽略。
func (m *Manager) FindByAPIKey(key string) (*User, bool) {
	if key == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var hit *User
	for k, u := range m.byKey {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			hit = u
		}
	}
	if hit == nil || hit.Disabled {
		return nil, false
	}
	return hit, true
}

// ByID 按 ID 查用户。
func (m *Manager) ByID(id string) (*User, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byID[id]
	return u, ok
}

// ByUsername 按用户名查用户（**大小写不敏感**，与 Create 的去重规则一致）。
//
// 大小写必须一致：Create 用 strings.ToLower 做去重索引，若这里区分大小写，
// 就会出现"Alice 建得出来但登录不进去"这种自相矛盾的行为。
func (m *Manager) ByUsername(name string) (*User, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byName[strings.ToLower(name)]
	return u, ok
}

// List 返回全部用户（稳定顺序，副本）。
//
// 稳定性要求：控制台每次刷新都调它，若顺序随机跳动会让人以为数据在变。
// CreatedAt 只到秒，同一秒创建的用户会并列，故必须再用 ID（唯一）兜底 ——
// 否则并列项的相对顺序取决于 map 迭代顺序（单元测试抓到过这个问题）。
func (m *Manager) List() []User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]User, 0, len(m.byID))
	for _, u := range m.byID {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role == RoleAdmin // 管理员排前面
		}
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID // 最终 tiebreaker：保证全序
	})
	return out
}

// MaskKey 把 API Key 打码用于展示（保留前 8 后 4）。
func MaskKey(k string) string {
	if len(k) <= 14 {
		return "****"
	}
	return k[:8] + "…" + k[len(k)-4:]
}

// Create 创建用户并生成专属 API Key（**不带密码**）。
//
// 不带密码的用户只能用 API Key 登录 —— 这是存量用户的既有状态，保持不变。
// 需要密码请用 CreateWithPassword，或事后调 SetPassword。
func (m *Manager) Create(username string, role Role, note string) (*User, error) {
	return m.create(username, role, note, "")
}

// CreateWithPassword 创建带密码的用户（自助注册用）。
//
// 为什么和 Create 共用一条 create 路径，而不是"先 Create 再 SetPassword"：
// 后者在第二步失败时（比如密码太短）会留下一个**已建好但没密码**的半成品用户，
// 调用方还得决定要不要回滚 —— 而 usermgr 是"内存优先"的，回滚本身也不可靠。
// 合成一次调用就没有中间态。
func (m *Manager) CreateWithPassword(username string, role Role, password, note string) (*User, error) {
	// 密码先校验：不合法就不要创建用户（否则又是一个半成品）
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	return m.create(username, role, note, password)
}

// create 是 Create / CreateWithPassword 的共用实现。
// password 为空表示不设密码。
func (m *Manager) create(username string, role Role, note, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("usermgr: 用户名不能为空")
	}
	if !role.Valid() {
		return nil, fmt.Errorf("usermgr: 非法角色 %q（可选 admin / user）", role)
	}
	// 密码派生放在**取锁之前**：PBKDF2 单次约 30ms，持写锁做这件事会把
	// 所有并发的读（每次 API 请求都要 FindByAPIKey）都堵住。
	var (
		hash, salt string
		iter       int
	)
	if password != "" {
		var err error
		hash, salt, iter, err = hashPassword(password)
		if err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.byName[strings.ToLower(username)]; dup {
		return nil, fmt.Errorf("usermgr: %w: %q", ErrUserExists, username)
	}
	// 首个用户必须是管理员 —— 这是一条防自锁规则。
	//
	// 原因：一旦存在用户，鉴权中间件就会禁止匿名通过（否则等于没鉴权）。
	// 若此时全局 api_key 也为空，而首个用户又不是管理员，则**没有任何人**
	// 能访问管理面（无法再创建用户、无法重置 Key），只能手工改 users.json。
	// 所以宁可在创建时就拒绝，也不留下这个坑。
	//
	// ⚠️ 自助注册**不**给这条规则开例外：让"第一个注册的人"直接拿到管理员，
	// 等于把管理权交给第一个访问者（开公网时尤其危险）。
	if len(m.byID) == 0 && role != RoleAdmin {
		return nil, fmt.Errorf("usermgr: 首个用户必须是管理员（role=admin），" +
			"否则创建后将无人能管理（匿名会被鉴权拒绝，而全局 api_key 为空）")
	}
	key, err := generateAPIKey()
	if err != nil {
		return nil, err
	}
	u := &User{
		ID:           "u" + randHex(6),
		Username:     username,
		APIKey:       key,
		Role:         role,
		CreatedAt:    time.Now().Format(time.RFC3339),
		Note:         note,
		PasswordHash: hash,
		Salt:         salt,
		PasswordIter: iter,
	}
	m.index(u)
	if err := m.saveLocked(); err != nil {
		// 落盘失败不回滚：内存已生效（内存优先），仅提示持久化问题
		return u, fmt.Errorf("usermgr: 用户已在内存中创建，但落盘失败（重启后会丢失）: %w", err)
	}
	return u, nil
}

// Delete 删除用户。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	if u.Role == RoleAdmin && m.countAdminsLocked() <= 1 {
		return fmt.Errorf("usermgr: 不能删除最后一个管理员（否则将无人能管理）")
	}
	m.unindex(u)
	return m.saveLocked()
}

// countAdminsLocked 统计可用管理员数（调用方持锁）。
func (m *Manager) countAdminsLocked() int {
	n := 0
	for _, u := range m.byID {
		if u.Role == RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n
}

// ResetKey 重新生成 API Key（旧 Key 立即失效）。
func (m *Manager) ResetKey(id string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	key, err := generateAPIKey()
	if err != nil {
		return nil, err
	}
	delete(m.byKey, u.APIKey)
	u.APIKey = key
	m.byKey[key] = u
	if err := m.saveLocked(); err != nil {
		return u, fmt.Errorf("usermgr: Key 已在内存中重置，但落盘失败: %w", err)
	}
	return u, nil
}

// SetRole 修改用户角色。
//
// ⚠️ 与 Delete/SetDisabled 同一条安全规则：**不能把最后一个管理员降级** ——
// 否则管理面会永久锁死（没人能再建用户、改配置、重置 Key）。
// 这正是"首个用户必须是管理员"那条规则要防的同一件事，只是换了个入口。
func (m *Manager) SetRole(id string, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("usermgr: 非法角色 %q（可选 admin / user）", role)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	if u.Role == role {
		return nil // 幂等：已经是该角色
	}
	// 管理员 → 普通用户：必须确认还剩至少一个管理员
	if u.Role == RoleAdmin && role != RoleAdmin {
		if m.countAdminsLocked() <= 1 {
			return fmt.Errorf("usermgr: 不能降级最后一个管理员 —— " +
				"否则管理面将永久锁死（没人能再建用户或重置 Key）")
		}
	}
	u.Role = role
	return m.saveLocked()
}

// SetDisabled 启用/禁用用户。
func (m *Manager) SetDisabled(id string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	if disabled && u.Role == RoleAdmin && m.countAdminsLocked() <= 1 {
		return fmt.Errorf("usermgr: 不能禁用最后一个管理员")
	}
	u.Disabled = disabled
	return m.saveLocked()
}

// SetQuota 设置用户的总配额（-1 或 0 表示无限制）。
func (m *Manager) SetQuota(id string, quota int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	u.Quota = quota
	return m.saveLocked()
}

// SetModelWhitelist 设置用户的允许模型白名单（空表示不限制）。
func (m *Manager) SetModelWhitelist(id string, models []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	clean := make([]string, 0, len(models))
	for _, mod := range models {
		mod = strings.TrimSpace(mod)
		if mod != "" {
			clean = append(clean, mod)
		}
	}
	u.ModelWhitelist = clean
	return m.saveLocked()
}

// DeductQuota 尝试扣除指定用户的配额。如果配额受限且不足，返回 false。
// delta 为本次消耗的 Token 数。
func (m *Manager) DeductQuota(id string, delta int64) (bool, int64, int64) {
	if delta <= 0 {
		return true, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return true, 0, 0
	}
	// 如果设置了配额且大于 0
	if u.Quota > 0 && (u.UsedQuota+delta > u.Quota) {
		return false, u.Quota, u.UsedQuota
	}
	u.UsedQuota += delta
	_ = m.saveLocked()
	return true, u.Quota, u.UsedQuota
}

// ---- 密码（PBKDF2-HMAC-SHA256）----
//
// ## 为什么是 PBKDF2 而不是单轮 sha256
//
// 单轮 sha256 对**离线爆破**几乎不设防：一张现代 GPU 每秒能算几十亿次，
// 8 位以内的口令几分钟就出结果。PBKDF2 用"反复迭代"把每次猜测的成本放大
// 10^5 倍，让离线爆破从"分钟级"变成"年级"。
// 参考实现（workbuddy2api）用的是 `sha256(password + ":" + salt)` 单轮 ——
// 我们**故意不照抄**这一点。
//
// ## 为什么手写而不用现成库
//
// 本项目的硬约束是**零第三方依赖**（go.mod 里不能出现 require），
// 所以不能用 golang.org/x/crypto/pbkdf2；而标准库至今**没有** PBKDF2。
// 实现本身只有十几行（RFC 8018 §5.2 的定义是明确且封闭的），
// 且已用 5 组公开测试向量验证（见 internal/server/auth_test.go）。

// 密码派生参数。
//
// 迭代次数取 210000 的理由（本机实测：100k≈16ms / 210k≈31ms / 600k≈91ms）：
//   - 契约下限是 100000，取它的 2.1 倍留出余量；
//   - 没有取 OWASP 目前建议的 600000：本项目是**本地自托管**工具，
//     登录路径前面通常没有独立的反向代理限流，迭代次数越高，越会把
//     "每次登录尝试"放大成一次 CPU 消耗（低成本 DoS）；31ms 是安全与
//     "登录不要卡"之间的平衡点；
//   - 参数**随哈希落盘**（User.PasswordIter），将来调高只需改这个常量，
//     老哈希仍按记录里的旧次数校验，**不需要让所有人重设密码**。
const (
	// PasswordIterations 是新密码使用的 PBKDF2 迭代次数。
	PasswordIterations = 210000
	// passwordSaltLen 是每用户盐的长度（字节）。
	passwordSaltLen = 16
	// passwordKeyLen 是派生密钥的长度（字节）。
	passwordKeyLen = 32
)

// MinPasswordLen 是用户密码的最短长度（按**字符数**算）。
//
// 为什么按字符数：中文密码按字节算会莫名其妙地"够长"（"密码"两个字 = 6 字节），
// 那会让长度校验形同虚设。
const MinPasswordLen = 6

// ValidatePassword 校验用户密码强度。
//
// ⚠️ 不做 TrimSpace：密码是**凭证**，首尾空格也是密码的一部分，
// 静默裁剪会导致"用户设的密码和他以为的不一样"。只拒绝全空白。
func ValidatePassword(pw string) error {
	if strings.TrimSpace(pw) == "" {
		return fmt.Errorf("usermgr: 密码不能为空")
	}
	if n := len([]rune(pw)); n < MinPasswordLen {
		return fmt.Errorf("usermgr: 密码至少需要 %d 位（当前 %d 位）", MinPasswordLen, n)
	}
	return nil
}

// PBKDF2SHA256 计算 PBKDF2-HMAC-SHA256 派生密钥（RFC 8018 §5.2）。
//
// 结构：
//
//	DK = T_1 || T_2 || ... || T_n          （n = ceil(keyLen / hLen)）
//	T_i = F(P, S, c, i)
//	F(P, S, c, i) = U_1 xor U_2 xor ... xor U_c
//	U_1 = HMAC(P, S || INT_32_BE(i))
//	U_j = HMAC(P, U_{j-1})
//
// 几个容易写错的地方：
//   - 块序号 i 是 **32 位大端**，不是小端也不是 1 字节；
//   - T_i 是 U_2..U_c 的**异或**，不是把它们拼起来；
//   - U_1 的输入是 salt 与块序号**拼接**后整体做一次 HMAC。
func PBKDF2SHA256(password, salt []byte, iter, keyLen int) []byte {
	if iter < 1 {
		// 迭代 0 次在 RFC 里非法；退化到 1 次而不是死循环
		iter = 1
	}
	if keyLen <= 0 {
		return nil
	}
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	idx := make([]byte, 4)
	u := make([]byte, hLen)
	t := make([]byte, hLen)
	for i := 1; i <= blocks; i++ {
		prf.Reset()
		prf.Write(salt)
		idx[0] = byte(i >> 24)
		idx[1] = byte(i >> 16)
		idx[2] = byte(i >> 8)
		idx[3] = byte(i)
		prf.Write(idx)
		u = prf.Sum(u[:0])
		copy(t, u)
		for j := 2; j <= iter; j++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// hashPassword 生成 (hash, salt, iter)，全部为 hex/整数，便于落盘。
func hashPassword(password string) (hash, salt string, iter int, err error) {
	sb := make([]byte, passwordSaltLen)
	if _, err := rand.Read(sb); err != nil {
		return "", "", 0, fmt.Errorf("usermgr: 生成密码盐失败: %w", err)
	}
	dk := PBKDF2SHA256([]byte(password), sb, PasswordIterations, passwordKeyLen)
	return hex.EncodeToString(dk), hex.EncodeToString(sb), PasswordIterations, nil
}

// VerifyPassword 校验密码是否与记录相符。
//
// 比较用 subtle.ConstantTimeCompare（常数时间）：用 `==` 比较 hex 字符串会在
// 第一个不同的字节处提前返回，攻击者可以靠测量响应时间逐字节猜出哈希。
//
// 任何"记录残缺"的情况（没设密码、salt 不是 hex、hash 不是 hex）都返回 false ——
// **不能**返回 true，否则一条残缺记录就成了万能钥匙。
func VerifyPassword(password, hash, salt string, iter int) bool {
	if hash == "" || salt == "" {
		return false // 没有密码 = 不允许密码登录（老用户只能走 API Key）
	}
	if iter <= 0 {
		iter = PasswordIterations // 早期记录没有该字段
	}
	sb, err := hex.DecodeString(salt)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(hash)
	if err != nil || len(want) == 0 {
		return false
	}
	got := PBKDF2SHA256([]byte(password), sb, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// BurnPasswordTime 对一次**注定失败**的登录做等价的密码派生，用于抹平时序差异。
//
// 为什么需要：PBKDF2 单次约 30ms。如果"用户名不存在"直接返回，它会比
// "密码错"快 30ms —— 响应耗时本身就成了**用户名枚举**的信道，
// 哪怕两者的响应体一字不差（错误信息一样，但快慢不一样）。
//
// 这里用一个固定盐做一次同参数的派生，把两条路径的耗时拉到同一量级。
func BurnPasswordTime(password string) {
	PBKDF2SHA256([]byte(password), []byte("trae2api-timing-equalizer"), PasswordIterations, passwordKeyLen)
}

// SetPassword 设置/重置用户密码（管理员代改与用户自助改密都走这里）。
//
// ⚠️ 这里**只动数据**，不碰任何会话：会话表在 server 层，不在 usermgr 里。
// "改密后作废该用户的全部会话"是调用方（auth.go）的职责。
//
// ⚠️ 派生在**取锁之前**完成；且落盘用 saveLocked() 而不是 Save() ——
// 后者会再拿一次读锁，而 RWMutex 不可重入，那就是**自死锁**。
func (m *Manager) SetPassword(id, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, salt, iter, err := hashPassword(password)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("usermgr: 用户不存在: %s", id)
	}
	u.PasswordHash, u.Salt, u.PasswordIter = hash, salt, iter
	if err := m.saveLocked(); err != nil {
		return fmt.Errorf("usermgr: 密码已在内存中更新，但落盘失败（重启后会失效）: %w", err)
	}
	return nil
}

// CheckPassword 校验指定用户的密码（常数时间比较）。
//
// 用户不存在 / 没设密码 / 密码错 → **都返回 false**。调用方不该据此区分原因
// （那等于送出一个用户名枚举接口）。需要抹平"用户不存在"与"密码错"的
// **耗时**差异时，先调 BurnPasswordTime。
func (m *Manager) CheckPassword(id, password string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byID[id]
	if !ok {
		return false
	}
	return VerifyPassword(password, u.PasswordHash, u.Salt, u.PasswordIter)
}

// ---- 系统设置（独立落盘，见 Manager.settingsPath）----

// systemSettings 是 system.json 的结构。
type systemSettings struct {
	AllowRegister bool `json:"allow_register"`
	// UpdatedAt 仅用于诊断（"这个开关是什么时候被改的"）。
	UpdatedAt string `json:"updated_at,omitempty"`
}

// AllowRegister 报告是否开放自助注册。
//
// 默认 false：注册是"任何人都能给自己开一个账号"的入口，**默认必须关**。
func (m *Manager) AllowRegister() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.allowRegister
}

// SetAllowRegister 设置"是否开放自助注册"并落盘。
//
// 内存先改、再落盘（与 Create 的"内存优先"一致），但把落盘失败**如实返回**：
// 开关是安全相关的设置，调用方需要知道它没被持久化。
func (m *Manager) SetAllowRegister(allow bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowRegister = allow
	if err := m.saveSettingsLocked(); err != nil {
		return fmt.Errorf("usermgr: 开放注册设置已在内存中生效，但落盘失败（重启后会恢复）: %w", err)
	}
	return nil
}

// loadSettings 读取系统设置。
//
// ⚠️ **刻意不返回错误、也不改变已有内存值**：
//   - 读不到时的默认值是"关闭注册"（fail-closed），这本身就是安全的一侧；
//   - 若把错误抛给 Load()，调用方（server.go）会把**用户表也一起丢掉**
//     （它把任何 Load 错误当成"以空用户表启动"）—— 那才是真正的安全降级：
//     RBAC 直接失效。两害相权，这里吞掉错误。
func (m *Manager) loadSettings() {
	if m.settingsPath == "" {
		return
	}
	raw, err := os.ReadFile(m.settingsPath)
	if err != nil {
		return // 文件不存在 = 还没改过，保持默认（关闭）
	}
	var st systemSettings
	if err := json.Unmarshal(raw, &st); err != nil {
		return // 损坏 = 按关闭处理（fail-closed），不冒险按开启处理
	}
	m.mu.Lock()
	m.allowRegister = st.AllowRegister
	m.mu.Unlock()
}

// saveSettingsLocked 落盘系统设置（原子写）。**调用方必须已持锁**。
func (m *Manager) saveSettingsLocked() error {
	if m.settingsPath == "" {
		return nil
	}
	b, err := json.MarshalIndent(systemSettings{
		AllowRegister: m.allowRegister,
		UpdatedAt:     time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("usermgr: 序列化系统设置: %w", err)
	}
	dir := filepath.Dir(m.settingsPath)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("usermgr: 创建目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".system-*.tmp")
	if err != nil {
		return fmt.Errorf("usermgr: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("usermgr: 写临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("usermgr: 关闭临时文件: %w", err)
	}
	if err := os.Rename(name, m.settingsPath); err != nil {
		// Windows 上目标已存在时 rename 可能失败，删掉再试一次
		os.Remove(m.settingsPath)
		if err2 := os.Rename(name, m.settingsPath); err2 != nil {
			os.Remove(name)
			return fmt.Errorf("usermgr: 替换 %s: %w", m.settingsPath, err2)
		}
	}
	return nil
}

// generateAPIKey 生成形如 sk-trae-<32hex> 的 API Key。
func generateAPIKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("usermgr: 生成 API Key: %w", err)
	}
	return "sk-trae-" + hex.EncodeToString(b), nil
}

// randHex 生成 n 字节随机数的 hex。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
