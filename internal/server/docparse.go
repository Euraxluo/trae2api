package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"trae2api/internal/docparse"
)

// 本文件把客户端消息里的**文件引用**展开成上游能理解的内容片段。
//
// 支持两种写法：
//
//  1. 显式片段（推荐，无歧义）：
//     {"role":"user","content":[{"type":"text","text":"看看这个"},
//                               {"type":"file","file":{"path":"/abs/path/main.go"}}]}
//
//  2. 文本里的 @路径（便利写法）：
//     {"role":"user","content":"看看 @/abs/path/main.go"}
//
// 关于 ② 的取舍：`@` 在自然语言里也可能是邮箱或提及，所以只在**能成功解析
// 到允许根目录内的真实文件**时才替换；解析失败就原样保留文本。
// 这样既方便又不会误伤，也不会泄露任何东西（读不到就是读不到）。
//
// 安全边界完全由 docparse.Expander 负责（根目录白名单 + 软链解析 + 大小限制）。

// fileRefPart 是显式文件引用的片段结构。
type fileRefPart struct {
	Type string `json:"type"`
	File *struct {
		Path string `json:"path"`
		// Name 可选，仅用于日志。
		Name string `json:"name"`
	} `json:"file"`
}

// expandFileRefs 展开请求体里的文件引用。
//
// 返回改写后的 body 与统计。**任何错误都不应让请求失败** ——
// 展开失败时保留原始引用文本，并把原因放进 Stats.Skipped，
// 这样用户能看到"为什么这个文件没被读到"，而不是整条请求 400。
func (s *Server) expandFileRefs(body []byte) ([]byte, docparse.Stats) {
	var stats docparse.Stats
	if !s.docs.Enabled() {
		return body, stats
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, stats
	}
	rawMsgs, ok := obj["messages"]
	if !ok {
		return body, stats
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body, stats
	}

	budget := s.docs.MaxTotalBytes()
	changed := false
	for _, m := range msgs {
		raw, ok := m["content"]
		if !ok {
			continue
		}
		// content 有两种形态：字符串 或 片段数组
		var text string
		if json.Unmarshal(raw, &text) == nil {
			// 字符串形态：只处理 @路径
			// 注意：**无论是否替换成功都要合并 stats** —— 失败原因
			//（例如"图片需改用片段形式"）要能传到日志里，否则用户
			// 只会看到"我明明写了 @x.png 却没反应"。
			nb, st, ch := s.expandAtRefs(text, budget)
			mergeStats(&stats, st)
			budget -= st.Bytes
			if ch {
				m["content"] = mustJSON(nb)
				changed = true
			}
			continue
		}
		var parts []json.RawMessage
		if json.Unmarshal(raw, &parts) != nil {
			continue
		}
		out := make([]json.RawMessage, 0, len(parts))
		for _, p := range parts {
			var fp fileRefPart
			if json.Unmarshal(p, &fp) == nil && fp.Type == "file" && fp.File != nil {
				part, n, err := s.docs.Load(fp.File.Path, budget)
				if err != nil {
					stats.Skipped = append(stats.Skipped, err.Error())
					// 保留一个可读的占位，避免模型"看不见"用户想附的文件却毫不知情
					out = append(out, mustJSON(map[string]any{
						"type": "text",
						"text": fmt.Sprintf("[附件读取失败：%s]", fp.File.Path),
					}))
					changed = true
					continue
				}
				budget -= n
				stats.Files++
				stats.Bytes += n
				if part.Type == "image" {
					stats.Images++
					out = append(out, mustJSON(map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": "data:" + part.MimeType + ";base64," + part.Base64,
						},
					}))
				} else {
					out = append(out, mustJSON(map[string]any{"type": "text", "text": part.Text}))
				}
				changed = true
				continue
			}
			// 文本片段里也可能有 @路径
			var tp struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(p, &tp) == nil && tp.Type == "text" && strings.Contains(tp.Text, "@") {
				nb, st, ch := s.expandAtRefs(tp.Text, budget)
				mergeStats(&stats, st)
				budget -= st.Bytes
				if ch {
					out = append(out, mustJSON(map[string]any{"type": "text", "text": nb}))
					changed = true
					continue
				}
			}
			out = append(out, p)
		}
		if len(out) != len(parts) {
			changed = true
		}
		m["content"] = mustJSON(out)
	}

	if !changed {
		return body, stats
	}
	// ⚠️ 必须把改过的 msgs **写回** obj：obj["messages"] 仍是原始的 raw JSON，
	// 只改切片而不回写的话，下面 json.Marshal(obj) 序列化的还是原内容
	//（表现为"统计说有文件，但请求体没变"——很难从现象反推）。
	obj["messages"] = mustJSON(msgs)
	nb := mustJSON(obj)
	if len(nb) == 0 || string(nb) == "null" {
		return body, stats
	}
	return nb, stats
}

// expandAtRefs 把文本里的 `@路径` 替换成文件内容。
//
// 只在解析成功时替换 —— 失败原样保留（见文件头注释的取舍说明）。
func (s *Server) expandAtRefs(text string, budget int64) (string, docparse.Stats, bool) {
	var stats docparse.Stats
	// 按空白切分，逐个 token 判断是否以 @ 开头
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return text, stats, false
	}
	replaced := false
	for i, tok := range fields {
		if !strings.HasPrefix(tok, "@") || len(tok) < 2 {
			continue
		}
		ref := strings.TrimPrefix(tok, "@")
		// 去掉可能的尾随标点（"看看 @a.go，" 这种中文标点很常见）
		ref = strings.TrimRight(ref, ",.;:!?，。；：！？）)】]")
		if ref == "" {
			continue
		}
		part, n, err := s.docs.Load(ref, budget)
		if err != nil {
			stats.Skipped = append(stats.Skipped, err.Error())
			continue
		}
		budget -= n
		stats.Files++
		stats.Bytes += n
		if part.Type == "image" {
			// 文本里的图片引用无法表达成 image_url（这里只处理文本流），
			// 明确说明而不是静默丢弃
			stats.Skipped = append(stats.Skipped,
				fmt.Sprintf("%s 是图片；请用 {\"type\":\"image_url\"} 片段引用", ref))
			continue
		}
		fields[i] = part.Text
		replaced = true
	}
	// 注意：即使一个都没替换成功，也要把 stats 带出去 ——
	// 否则"图片引用需要改用片段形式"这类说明会被静默丢弃，
	// 用户只会看到"我明明写了 @x.png 却没反应"。
	if !replaced {
		return text, stats, false
	}
	return strings.Join(fields, " "), stats, true
}

// mergeStats 合并两次展开的统计。
func mergeStats(dst *docparse.Stats, src docparse.Stats) {
	dst.Files += src.Files
	dst.Bytes += src.Bytes
	dst.Images += src.Images
	dst.Skipped = append(dst.Skipped, src.Skipped...)
	dst.Truncated = append(dst.Truncated, src.Truncated...)
}

// mustJSON 序列化（失败时返回一个安全的占位，绝不 panic）。
//
// 用 Encoder + SetEscapeHTML(false) 而不是 json.Marshal：
// Marshal 会把 `<` `>` `&` 转义成 \u003c 之类 —— 对文件内容（HTML/XML/Markdown）
// 来说这既撑大体积又让人无法在日志里读。
func mustJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return json.RawMessage(`null`)
	}
	// Encode 会补一个换行，去掉它
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// logFileStats 把展开结果写进日志（跳过项要能看见，否则用户不知道为什么没生效）。
func (s *Server) logFileStats(st docparse.Stats) {
	if st.Files == 0 && len(st.Skipped) == 0 {
		return
	}
	log.Printf("INFO: 附件展开：%d 个文件 / %d 字节（其中图片 %d）",
		st.Files, st.Bytes, st.Images)
	for _, sk := range st.Skipped {
		log.Printf("WARN: 附件未展开：%s", sk)
	}
}
