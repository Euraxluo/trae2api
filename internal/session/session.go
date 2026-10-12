// Package session 实现会话粘性路由：同一多轮对话固定绑定同一账号。
//
// 为什么需要它（借鉴 workbuddy2api 的结论）：Trae 上游按会话做 prompt cache，
// 若多轮对话中途换账号，缓存必然全部失效，既慢又费额度。
//
// 键的选取：优先用客户端显式给出的会话标识（OpenAI 的 metadata / 常见
// conversationId 字段），缺失时回落到"首条 user 消息内容"的哈希 —— 这样
// 连没有会话概念的客户端（dsh / Codex 等）也能获得粘性。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Binding 是一次会话绑定。
type Binding struct {
	UID      string
	BoundAt  time.Time
	LastSeen time.Time
}

// Router 是会话粘性路由器（内存实现，进程内有效）。
//
// 持久化取舍：不落盘。粘性是**性能优化**而非正确性保证 —— 重启后重新绑定
// 只是损失一次 prompt cache 命中，不值得为此引入磁盘依赖与一致性风险。
type Router struct {
	mu  sync.Mutex
	m   map[string]*Binding
	ttl time.Duration
}

// New 构造路由器；ttl<=0 时用默认 30 分钟。
func New(ttl time.Duration) *Router {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Router{m: map[string]*Binding{}, ttl: ttl}
}

// Resolve 返回该会话已绑定的账号 UID（不存在或已过期则 ok=false）。
func (r *Router) Resolve(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.m[key]
	if !ok {
		return "", false
	}
	if time.Since(b.LastSeen) > r.ttl {
		delete(r.m, key)
		return "", false
	}
	b.LastSeen = time.Now()
	return b.UID, true
}

// Bind 绑定会话到账号（已存在则覆盖）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if b, ok := r.m[key]; ok {
		b.UID = uid
		b.LastSeen = now
		return
	}
	r.m[key] = &Binding{UID: uid, BoundAt: now, LastSeen: now}
}

// Unbind 解除绑定（账号不可用时调用，让下次请求重新选号）。
func (r *Router) Unbind(key string) bool {
	if key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[key]; !ok {
		return false
	}
	delete(r.m, key)
	return true
}

// Count 当前有效绑定数（顺带清理过期项）。
func (r *Router) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, b := range r.m {
		if now.Sub(b.LastSeen) > r.ttl {
			delete(r.m, k)
		}
	}
	return len(r.m)
}

// ---- 会话键提取 ----

// keyFields 是常见的会话标识字段（按优先级）。
// 覆盖 OpenAI 官方字段与主流客户端的自定义字段，尽量少让会话退化成无粘性。
var keyFields = []string{
	"conversation_id", "conversationId", "session_id", "sessionId",
	"chat_id", "chatId", "thread_id", "threadId",
}

// ExtractKey 从请求体中提取会话键；找不到返回空串。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	for _, f := range keyFields {
		if raw, ok := m[f]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return f + ":" + s
			}
		}
	}
	// OpenAI 的 metadata 嵌套形式
	if raw, ok := m["metadata"]; ok {
		var meta map[string]any
		if json.Unmarshal(raw, &meta) == nil {
			for _, f := range keyFields {
				if v, ok := meta[f]; ok {
					if s, ok := v.(string); ok && s != "" {
						return "metadata." + f + ":" + s
					}
				}
			}
		}
	}
	return ""
}

// FallbackKey 在客户端没有会话标识时，用首条 user 消息派生会话级键。
//
// 语义：同一条"开场白"视为同一个会话的起点 —— 对 dsh / Codex 这类每轮都
// 重发完整历史的客户端，首条 user 消息在整个对话中保持不变，因此能稳定粘住。
func FallbackKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		text := contentText(msg.Content)
		if text == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		return "fallback:" + hex.EncodeToString(sum[:8])
	}
	return ""
}

// contentText 把 content（字符串或片段数组）拍平成文本，仅用于派生键。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := ""
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			out += p.Text
		}
	}
	return out
}
