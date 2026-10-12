// Package prompt 实现 System Prompt 改写。
//
// 为什么需要它：很多 OpenAI 兼容客户端不发送 system 消息，或者发的内容不适合
// 当前上游（例如带一堆 Claude Code 专用指令）。网关侧统一注入/替换可以让
// 同一个上游服务多种客户端，而不必在每个客户端里各配一遍。
//
// 三种模式：
//   - passthrough（默认）：完全不动客户端的 system，行为与不接网关时一致。
//   - custom：**替换** —— 丢掉客户端的所有 system，只发网关配置的这一段。
//   - append：**追加** —— 保留客户端的 system，在末尾接上网关配置的这一段。
//
// 另有 Degraded 极简提示词：当请求因 400 被拒（可能是注入的提示词触发了上游的
// 内容校验）时，用一段最小提示词重试一次，而不是直接把失败抛给用户。
package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Mode 是改写模式。
type Mode string

const (
	// ModePassthrough 透传（默认）：不改写。
	ModePassthrough Mode = "passthrough"
	// ModeCustom 替换客户端的所有 system 消息。
	ModeCustom Mode = "custom"
	// ModeAppend 在客户端 system 消息末尾追加。
	ModeAppend Mode = "append"
)

// Valid 报告模式是否合法。
func (m Mode) Valid() bool {
	switch m {
	case ModePassthrough, ModeCustom, ModeAppend:
		return true
	}
	return false
}

// ParseMode 解析模式字符串（空 = passthrough，非法值报错而不是静默回落 ——
// 静默回落会让用户以为配置生效了）。
func ParseMode(s string) (Mode, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ModePassthrough, nil
	}
	m := Mode(s)
	if !m.Valid() {
		return "", fmt.Errorf("prompt: 未知模式 %q（可选 passthrough / custom / append）", s)
	}
	return m, nil
}

// Degraded 是降级重试时使用的极简提示词。
//
// 为什么需要它：若 custom/append 注入的文本触发了上游的内容校验（返回 400），
// 整条请求就会失败 —— 而这与用户的实际问题无关。此时用一段中立、无争议的
// 最小提示词重试一次，多数情况能成功。
//
// 刻意保持极短且不含任何指令性内容：降级的目的是"让请求能发出去"，
// 而不是"换一套人格"。
const Degraded = "You are a helpful assistant."

// Rewriter 按模式改写请求体里的 system 消息。
type Rewriter struct {
	mode Mode
	text string
}

// New 构造改写器。mode 为 passthrough 或 text 为空时等价于不改写。
func New(mode Mode, text string) *Rewriter {
	if !mode.Valid() {
		mode = ModePassthrough
	}
	return &Rewriter{mode: mode, text: strings.TrimSpace(text)}
}

// Enabled 报告是否会做改写（passthrough 或空文本时为 false）。
func (r *Rewriter) Enabled() bool {
	return r != nil && r.mode != ModePassthrough && r.text != ""
}

// Mode 返回当前模式。
func (r *Rewriter) Mode() Mode {
	if r == nil {
		return ModePassthrough
	}
	return r.mode
}

// Apply 对 OpenAI 请求体应用改写。
//
// 输入非 JSON、无 messages 字段、或改写未启用时**原样返回** ——
// 改写是增强功能，绝不该让一条本来能用的请求失败。
//
// 对 nil receiver 也安全（返回原 body）：这样调用方不必先判空，
// 少一处 `if s.prompt != nil` 就少一处漏判的机会。
func (r *Rewriter) Apply(body []byte) []byte {
	if r == nil {
		return body
	}
	return r.applyWith(body, r.mode, r.text)
}

// ApplyDegraded 用极简提示词改写（降级重试用）。
//
// 语义：把 system 消息**整体替换**为 Degraded，不管原模式是 custom 还是 append ——
// 降级的目的就是"去掉可能触发校验的内容"。
func (r *Rewriter) ApplyDegraded(body []byte) []byte {
	if r == nil || r.mode == ModePassthrough {
		return body
	}
	return r.applyWith(body, ModeCustom, Degraded)
}

func (r *Rewriter) applyWith(body []byte, mode Mode, text string) []byte {
	if r == nil || mode == ModePassthrough || strings.TrimSpace(text) == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // 不是 JSON：不动
	}
	rawMsgs, ok := obj["messages"]
	if !ok {
		return body // 没有 messages：不动（可能是 /v1/completions 之类）
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body
	}

	out := rewriteMessages(msgs, mode, text)
	nb, err := json.Marshal(out)
	if err != nil {
		return body
	}
	obj["messages"] = nb
	res, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return res
}

// isSystemRole 判定该消息是否属于"系统级"角色。
// 同时认 OpenAI 的 `system` 与较新的 `developer`（后者是前者的替代名）。
func isSystemRole(m map[string]json.RawMessage) bool {
	var role string
	if raw, ok := m["role"]; ok {
		_ = json.Unmarshal(raw, &role)
	}
	return role == "system" || role == "developer"
}

// messageText 取出消息的文本内容（字符串或片段数组）。
func messageText(m map[string]json.RawMessage) string {
	raw, ok := m["content"]
	if !ok {
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
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// systemMessage 构造一条 system 消息。
func systemMessage(text string) map[string]json.RawMessage {
	role, _ := json.Marshal("system")
	content, _ := json.Marshal(text)
	return map[string]json.RawMessage{"role": role, "content": content}
}

// rewriteMessages 执行实际改写。
func rewriteMessages(msgs []map[string]json.RawMessage, mode Mode, text string) []map[string]json.RawMessage {
	switch mode {
	case ModeAppend:
		// 找第一条系统消息追加；没有则在最前面插入
		for i, m := range msgs {
			if !isSystemRole(m) {
				continue
			}
			existing := messageText(m)
			merged := text
			if strings.TrimSpace(existing) != "" {
				merged = existing + "\n\n" + text
			}
			// 替换该条（同时把 developer 归一成 system，避免上游不认 developer）
			replaced := append([]map[string]json.RawMessage{}, msgs...)
			replaced[i] = systemMessage(merged)
			return replaced
		}
		return append([]map[string]json.RawMessage{systemMessage(text)}, msgs...)

	case ModeCustom:
		// 丢掉全部系统消息，在最前面放我们的
		filtered := make([]map[string]json.RawMessage, 0, len(msgs)+1)
		filtered = append(filtered, systemMessage(text))
		for _, m := range msgs {
			if isSystemRole(m) {
				continue
			}
			filtered = append(filtered, m)
		}
		return filtered
	}
	return msgs
}
