package server

import "strings"

// 本文件实现出站请求的 **tool_call ↔ tool 结果配对自愈**。
//
// ## 要解决的问题
//
// OpenAI 兼容协议要求：带 `tool_calls` 的 assistant 消息，其**每一个** tool_call id
// 都必须有对应的 `role:"tool"` 结果消息；反之 tool 结果也必须有前置的 tool_call。
// 缺任一侧，上游会以 HTTP 400 拒绝**整个请求**。
//
// 而坏数据是常态：工具执行失败时（参数非法、超时、工具不存在……），客户端会把
// assistant 的 tool_calls 持久化进会话历史，却**写不回结果消息**。这条坏历史
// 随后被每次请求原样重放 —— 上游对之后**每一条**用户消息都返回 400，整条会话报废。
//
// 网关是最后一道防线：发出请求前剔除无法配对的条目让会话自愈。
// 宁可丢一轮工具上下文，也好过整条会话死亡。

// cleanupToolPairing 剔除无法配对的 tool_call 与 tool 结果。
//
// ## 关键设计：**两侧共用同一份 keepCalls 做对称裁剪**
//
// 曾经（也是很多实现的）写法是"批内每个 id 都齐才保留整批，否则删掉整个
// tool_calls 键"。那个写法会留下**无主结果**：
//
//	批 [c1,c2] 只回了 c1
//	→ 调用侧整批被删（因为不齐）
//	→ 而 tool{c1} 仍按 id 命中 keepCalls 得以保留
//	→ 出站载荷变成「无 tool_calls 的 assistant + 孤儿 tool」
//	→ 上游判 11148（tool calls and tool results do not match）→ **顶死整条会话**
//
// 正确做法是两侧**按同一个 id 集合**裁剪：任何输入都不会再产生半截配对。
//
// 返回清理后的切片（无改动时返回原切片）与是否发生删除。
func cleanupToolPairing(msgs []chatMessage) ([]chatMessage, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}

	// 1) 收集两侧的 id 集合
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			if m.ToolCallID != "" {
				resultIDs[m.ToolCallID] = true
				hasTraffic = true
			}
		case "assistant":
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					callIDs[tc.ID] = true
					hasTraffic = true
				}
			}
		}
	}
	// 完全没有工具流量 → 零分配零改动返回
	if !hasTraffic {
		return msgs, false
	}

	// 2) keepCalls = 两侧都有的 id（重复 id 与乱序都按集合处理）
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}

	changed := false

	// 3) 调用侧：按 keepCalls 对称裁剪，裁空则删掉整个 tool_calls
	for i := range msgs {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		orig := msgs[i].ToolCalls
		kept := make([]chatToolCall, 0, len(orig))
		for _, tc := range orig {
			if keepCalls[tc.ID] {
				kept = append(kept, tc)
			}
		}
		if len(kept) == len(orig) {
			continue // 整批齐全：零改动
		}
		changed = true
		msgs[i].ToolCalls = kept // 裁空时就是 nil，序列化会省略该字段
	}

	// 4) 结果侧：只有对应 tool_call 被保留才保留，孤儿结果整条删除
	kept := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" && !keepCalls[m.ToolCallID] {
			changed = true
			continue
		}
		kept = append(kept, m)
	}
	if !changed {
		return msgs, false
	}
	return kept, true
}

// repackToolResults 把插在 assistant.tool_calls 与其 tool 结果之间的**非 tool 消息**
// 挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// ## 为什么需要
//
// 有些客户端会在 tool 输出后面插一条 developer/system 消息（例如
// `<image_resize_notice>`）。并行工具调用时它插在两份结果中间：
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <notice>      ← 插在中间，配对断裂
//	tool c01
//
// 协议要求 tool 结果紧跟 assistant，中间插任何消息都算断裂。这里**只调顺序、
// 不改内容**：
//
//	assistant [c00 c01] | tool c00 | X | tool c01
//	→ assistant [c00 c01] | tool c00 | tool c01 | X
//
// 结果的相对顺序保持不变（同批 tool_call 的原顺序 = 结果顺序），不引入新的
// 顺序敏感问题。无插入消息时零改动零分配。
func repackToolResults(msgs []chatMessage) ([]chatMessage, bool) {
	if len(msgs) < 3 {
		return msgs, false
	}
	out := make([]chatMessage, 0, len(msgs))
	changed := false
	i := 0
	for i < len(msgs) {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			out = append(out, m)
			i++
			continue
		}
		want := map[string]bool{}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				want[tc.ID] = true
			}
		}
		// 组头先出去
		out = append(out, m)
		i++

		var results, between []chatMessage
		sawNonTool := false
		for i < len(msgs) {
			mm := msgs[i]
			if mm.Role == "tool" {
				if !want[mm.ToolCallID] {
					break // 不属于本组
				}
				results = append(results, mm)
				if sawNonTool {
					changed = true // 有插入物 → 需要重排
				}
				i++
				continue
			}
			if len(results) == 0 {
				// assistant 后没有结果：交给 cleanupToolPairing 处理，这里不动
				break
			}
			// ⚠️ 下一组 assistant.tool_calls 是**新的组头**，绝不能当插入物吞掉：
			// 一旦被收进 between，它永远不再被外层循环当作组头处理，
			// 它自己那批结果也就永远得不到重排。必须 break 交还外层循环。
			if mm.Role == "assistant" && len(mm.ToolCalls) > 0 {
				break
			}
			between = append(between, mm)
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

// fixToolPairing 是三步自愈的统一入口：先重排（让结果连续）→ 再裁剪（剔除孤儿）
// → 最后规整 assistant 的空白 content。
//
// 顺序不能反：
//   - 先裁剪的话，被插入物打断的那批结果会被当成"部分配对"而误裁；
//   - 规整必须**最后**做，因为裁剪可能把一个有 tool_calls 的 assistant
//     变成"既无 tool_calls 又无 content"的空壳（见 normalizeAssistantContent）。
func fixToolPairing(msgs []chatMessage) ([]chatMessage, bool) {
	reordered, c1 := repackToolResults(msgs)
	cleaned, c2 := cleanupToolPairing(reordered)
	normalized, c3 := normalizeAssistantContent(cleaned)
	return normalized, c1 || c2 || c3
}

// normalizeAssistantContent 把"无 tool_calls 且 content 为空"的 assistant 消息
// 的 content 补成一个空格。
//
// ## 为什么必须做（实测发现）
//
// 上游**要求 assistant 消息的 content 非空白**。实测各种形态：
//
//	content: "hello"   → 200
//	content: null      → 400（业务码 4001 "param is invalid"）
//	content: ""        → 400（同上）
//	无 content 键       → 400（同上）
//	content: " "       → 200   ← 一个空格就够
//
// 而 cleanupToolPairing 恰好会制造出这种空壳：一条 `content:null + tool_calls`
// 的 assistant 被裁掉孤儿 tool_calls 后，就成了"既无 tool_calls 又无 content"，
// 于是**整个请求**被上游 400 —— 自愈反而制造了失败。
//
// 为什么不直接删掉这条消息：删掉会改变消息条数与角色交替结构，
// 有些客户端/模型对交替敏感。补一个空格既满足上游校验，又不引入可见语义
// （模型看到的是一个空回合，与"这条消息本来就没内容"一致）。
func normalizeAssistantContent(msgs []chatMessage) ([]chatMessage, bool) {
	changed := false
	for i := range msgs {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) > 0 {
			continue
		}
		if !isBlankContent(msgs[i].Content) {
			continue
		}
		msgs[i].Content = []byte(`" "`)
		changed = true
	}
	return msgs, changed
}

// isBlankContent 判定消息 content 是否为"**上游会拒收的空白**"。
//
// ⚠️ 语义要点：这里的"空白"不是"视觉上没内容"，而是"上游会返回 400 的那些形态"。
// 实测被拒的只有四种：字段缺失、`null`、`""`（空字符串）、`[]`（空片段数组）。
//
// 而 `" "`（含一个空格的字符串）上游**接受** —— 所以它**必须**判为非空白。
// 否则会破坏本函数的幂等性：normalizeAssistantContent 正是把空壳改成 `" "`，
// 若 `" "` 也算空白，规整就会每次都被判定为"有改动"，反复施加。
func isBlankContent(raw []byte) bool {
	if len(raw) == 0 {
		return true // 字段缺失
	}
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", `""`, "[]":
		return true
	}
	return false
}
