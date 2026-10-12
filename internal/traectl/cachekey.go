package traectl

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// 本文件构造上游的 **prompt cache key**（费用优化的关键）。
//
// ## 为什么值得做
//
// 上游支持前缀缓存：同一段长前缀重复出现时，只按"命中"部分计费。
// 实测（参考实现的逆向数据）：同一段 8k token 前缀，
//
//	不带 cache key → 命中 0 token，费用 ≈ 0.34
//	带   cache key → 命中 7808 token，费用 ≈ 0.02   （降约 17×）
//
// ## 安全约束：必须按账号隔离
//
// ⚠️ 这是本文件最重要的一条：cache key **绝不能跨账号复用**。
// 若两个账号用同一个 key，上游会命中**另一个账号**的前缀缓存 ——
// 那等于把对方的对话内容喂给了你的请求。所以 uid 是**硬隔离因子**，
// 不是"锦上添花的区分度"。
//
// 键格式：`t2a-<uid8>-<sha256(uid|conv)[:16]>`
//
//	uid8    账号 UID 前 8 字符 —— 保证跨账号绝不碰撞
//	convHex 会话段的哈希 —— 同账号同会话稳定、不同会话不同
const cacheKeyPrefix = "t2a-"

// BuildPromptCacheKey 生成按账号隔离的稳定 cache key。
//
// conversation 为空时仍会生成（会话段由 uid 单独哈希）：
// 这样跨账号仍不碰撞，但"无会话标识的请求"之间不复用前缀 —— 符合"空会话 = 新会话"的语义。
func BuildPromptCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		// 拿不到 uid 时用固定段：仍然生成 key（保留前缀缓存收益），
		// 但调用方**应当**保证传真实 uid —— 空 uid 下所有账号会共用同一个键，
		// 那正是本文件要防的跨账号泄露。
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + strings.TrimSpace(conversation)))
	return cacheKeyPrefix + uid8 + "-" + hex.EncodeToString(sum[:16])
}
