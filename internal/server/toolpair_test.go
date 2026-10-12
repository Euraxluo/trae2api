package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// tc 是构造 tool_call 的简写。
func tc(id string) chatToolCall {
	var c chatToolCall
	c.ID = id
	c.Type = "function"
	return c
}

func assistantWithCalls(ids ...string) chatMessage {
	m := chatMessage{Role: "assistant"}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, tc(id))
	}
	return m
}

func toolResult(id string) chatMessage {
	return chatMessage{Role: "tool", ToolCallID: id}
}

func userMsg(text string) chatMessage {
	b, _ := json.Marshal(text)
	return chatMessage{Role: "user", Content: b}
}

func callIDsOf(m chatMessage) []string {
	var out []string
	for _, c := range m.ToolCalls {
		out = append(out, c.ID)
	}
	return out
}

// TestCleanupSymmetricTrimOnPartialBatch 是本文件最重要的一条测试。
//
// 批 [c1,c2] 只回了 c1 时，**两侧必须按同一个 id 集合裁剪**：
// 调用侧只留 c1、结果侧只留 tool{c1}。
//
// 曾经的写法是"批内全齐才保留整批"——那会删掉整个 tool_calls 而保留 tool{c1}，
// 出站载荷变成「无 tool_calls 的 assistant + 孤儿 tool」，
// 上游判 11148（tool calls and tool results do not match）并**顶死整条会话**。
func TestCleanupSymmetricTrimOnPartialBatch(t *testing.T) {
	msgs := []chatMessage{
		userMsg("hi"),
		assistantWithCalls("c1", "c2"),
		toolResult("c1"),
	}
	out, changed := cleanupToolPairing(msgs)
	if !changed {
		t.Fatal("部分配对应被裁剪")
	}
	if len(out) != 3 {
		t.Fatalf("消息数 = %d want 3（user + assistant + tool）", len(out))
	}
	// 调用侧：只剩 c1
	got := callIDsOf(out[1])
	if len(got) != 1 || got[0] != "c1" {
		t.Errorf("调用侧应只留 c1，实际 %v", got)
	}
	// 结果侧：tool{c1} 保留
	if out[2].Role != "tool" || out[2].ToolCallID != "c1" {
		t.Errorf("结果侧应保留 tool{c1}: %+v", out[2])
	}
	// 关键不变量：不能出现"无 tool_calls 的 assistant + 孤儿 tool"
	if len(out[1].ToolCalls) == 0 && len(out) > 2 && out[2].Role == "tool" {
		t.Fatal("产生了孤儿 tool 结果 —— 上游会判 11148 顶死会话")
	}
}

// TestCleanupDropsAllOrphans 完全没有配对的调用与结果都要删掉。
func TestCleanupDropsAllOrphans(t *testing.T) {
	msgs := []chatMessage{
		userMsg("hi"),
		assistantWithCalls("c1"), // 没有结果
		toolResult("c9"),         // 没有调用
	}
	out, changed := cleanupToolPairing(msgs)
	if !changed {
		t.Fatal("应发生裁剪")
	}
	if len(out) != 2 {
		t.Fatalf("应只剩 user + assistant，实际 %d 条: %+v", len(out), out)
	}
	if len(out[1].ToolCalls) != 0 {
		t.Errorf("孤儿调用应被删掉: %v", out[1].ToolCalls)
	}
	for _, m := range out {
		if m.Role == "tool" {
			t.Error("孤儿结果应被整条删除")
		}
	}
}

// TestCleanupKeepsCompletePairs 完整配对必须零改动。
func TestCleanupKeepsCompletePairs(t *testing.T) {
	msgs := []chatMessage{
		userMsg("hi"),
		assistantWithCalls("c1", "c2"),
		toolResult("c1"),
		toolResult("c2"),
		userMsg("next"),
	}
	out, changed := cleanupToolPairing(msgs)
	if changed {
		t.Errorf("完整配对不该被改动: %+v", out)
	}
	if len(out) != 5 {
		t.Errorf("消息数变了: %d", len(out))
	}
}

// TestCleanupNoToolTrafficIsNoop 没有工具流量时零改动（连分配都不该有）。
func TestCleanupNoToolTrafficIsNoop(t *testing.T) {
	msgs := []chatMessage{userMsg("a"), {Role: "assistant"}, userMsg("b")}
	out, changed := cleanupToolPairing(msgs)
	if changed {
		t.Error("无工具流量不该判定为改动")
	}
	if len(out) != 3 {
		t.Errorf("消息数变了: %d", len(out))
	}
}

// TestCleanupDuplicateIDs 重复 id 按集合处理，不应误删。
func TestCleanupDuplicateIDs(t *testing.T) {
	msgs := []chatMessage{
		assistantWithCalls("c1", "c1"),
		toolResult("c1"),
	}
	_, changed := cleanupToolPairing(msgs)
	if changed {
		t.Error("重复 id 但两侧都有，不该裁剪")
	}
}

// TestRepackMovesInterleavedMessage 插在两条 tool 结果中间的消息要被移到组后。
//
// 场景：客户端在 tool 输出后插了一条 developer 提示（如 image_resize_notice），
// 并行调用时它插在两份结果中间 → 配对断裂 → 上游判 11148。
func TestRepackMovesInterleavedMessage(t *testing.T) {
	notice := chatMessage{Role: "developer"}
	msgs := []chatMessage{
		assistantWithCalls("c1", "c2"),
		toolResult("c1"),
		notice,
		toolResult("c2"),
	}
	out, changed := repackToolResults(msgs)
	if !changed {
		t.Fatal("应发生重排")
	}
	if len(out) != 4 {
		t.Fatalf("消息数变了: %d", len(out))
	}
	// 顺序应为：assistant, tool c1, tool c2, developer
	if out[1].Role != "tool" || out[1].ToolCallID != "c1" {
		t.Errorf("out[1] 应为 tool c1: %+v", out[1])
	}
	if out[2].Role != "tool" || out[2].ToolCallID != "c2" {
		t.Errorf("out[2] 应为 tool c2（结果要连续）: %+v", out[2])
	}
	if out[3].Role != "developer" {
		t.Errorf("out[3] 应为被后移的 developer: %+v", out[3])
	}
}

// TestRepackDoesNotSwallowNextGroup ⚠️ 下一组 assistant.tool_calls 绝不能被当成
// "插入物"吞掉。
//
// 一旦被收进 between，它永远不再被外层循环当作组头处理，
// 它自己那批结果也就永远得不到重排 —— 真实会话里正是这样漏掉 msg[181] 的。
func TestRepackDoesNotSwallowNextGroup(t *testing.T) {
	notice := chatMessage{Role: "system"}
	msgs := []chatMessage{
		assistantWithCalls("c1", "c2"),
		toolResult("c1"),
		notice,
		toolResult("c2"),
		// 紧接着是**新一组**
		assistantWithCalls("d1", "d2"),
		toolResult("d1"),
		{Role: "system"}, // 第二个插入物（用非 tool 角色）
		toolResult("d2"),
	}
	out, _ := repackToolResults(msgs)
	// 新组头必须仍是 assistant 且带 tool_calls
	found := false
	for i, m := range out {
		if m.Role == "assistant" && len(m.ToolCalls) == 2 && m.ToolCalls[0].ID == "d1" {
			found = true
			// 它后面的两条 tool 结果必须连续
			if i+2 >= len(out) || out[i+1].Role != "tool" || out[i+2].Role != "tool" {
				t.Errorf("第二组的结果未连续: %+v", out[i:])
			}
		}
	}
	if !found {
		t.Fatal("第二组的组头被吞掉了")
	}
}

// TestRepackNoInterleaveIsNoop 没有插入物时零改动。
func TestRepackNoInterleaveIsNoop(t *testing.T) {
	msgs := []chatMessage{
		assistantWithCalls("c1", "c2"),
		toolResult("c1"),
		toolResult("c2"),
		userMsg("next"),
	}
	_, changed := repackToolResults(msgs)
	if changed {
		t.Error("无插入物不该判定为改动")
	}
}

// TestFixToolPairingOrder 两步自愈的顺序必须是"先重排、再裁剪"。
//
// 反过来的话，被插入物打断的那批结果会被当成"部分配对"而误裁。
func TestFixToolPairingOrder(t *testing.T) {
	notice := chatMessage{Role: "system"}
	msgs := []chatMessage{
		assistantWithCalls("c1", "c2"),
		toolResult("c1"),
		notice,
		toolResult("c2"),
	}
	out, changed := fixToolPairing(msgs)
	if !changed {
		t.Fatal("应发生修复")
	}
	// 两条调用都该保留（它们都有结果，只是顺序被打断）
	if len(out[0].ToolCalls) != 2 {
		t.Errorf("两条调用都该保留，实际 %v —— 说明先裁剪后重排了", callIDsOf(out[0]))
	}
	tools := 0
	for _, m := range out {
		if m.Role == "tool" {
			tools++
		}
	}
	if tools != 2 {
		t.Errorf("两条结果都该保留，实际 %d", tools)
	}
}

// TestIsTruncatedArguments 截断判定：只把"非空但解析不了"当截断。
func TestIsTruncatedArguments(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"", false, "空串 = 合法的无参工具"},
		{"   ", false, "纯空白同理"},
		{`{"city":"北京"}`, false, "完整 JSON"},
		{`null`, false, "null 是合法 JSON"},
		{`[]`, false, "数组是合法 JSON"},
		{`123`, false, "标量是合法 JSON"},
		{`{"city":"北`, true, "半截 JSON = 截断"},
		{`{"a":1`, true, "缺右括号 = 截断"},
		{`not json`, true, "根本不是 JSON"},
	}
	for _, c := range cases {
		if got := isTruncatedArguments(c.in); got != c.want {
			t.Errorf("isTruncatedArguments(%q) = %v want %v（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestToolAccumAggregates 工具增量按 index 正确聚合。
func TestToolAccumAggregates(t *testing.T) {
	buf := map[int]*toolAccum{}
	add := func(i int, id, name, args string) {
		b := buf[i]
		if b == nil {
			b = &toolAccum{}
			buf[i] = b
		}
		if id != "" {
			b.id = id
		}
		if name != "" {
			b.name = name
		}
		b.args.WriteString(args)
	}
	// 模拟流式分片
	add(0, "call_1", "get_weather", `{"city"`)
	add(0, "", "", `:"北京"}`)
	add(1, "call_2", "get_time", "")

	if buf[0].args.String() != `{"city":"北京"}` {
		t.Errorf("分片聚合错误: %q", buf[0].args.String())
	}
	if isTruncatedArguments(buf[0].args.String()) {
		t.Error("聚合后应是完整 JSON")
	}
	if buf[1].args.String() != "" {
		t.Errorf("无参工具应为空串: %q", buf[1].args.String())
	}
	if isTruncatedArguments(buf[1].args.String()) {
		t.Error("空串不该被判为截断")
	}
}

// TestToolPairingIntegrationWireFormat 端到端：清理后的消息序列化出来必须是
// 上游能接受的形状（不出现"无 tool_calls 的 assistant 紧跟 tool"）。
func TestToolPairingIntegrationWireFormat(t *testing.T) {
	msgs := []chatMessage{
		userMsg("hi"),
		assistantWithCalls("c1", "c2", "c3"),
		toolResult("c2"), // 只有中间那个有结果
	}
	out, _ := fixToolPairing(msgs)

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// 只应保留 c2
	if !strings.Contains(s, `"c2"`) {
		t.Errorf("c2 应保留: %s", s)
	}
	if strings.Contains(s, `"c1"`) || strings.Contains(s, `"c3"`) {
		t.Errorf("c1/c3 应被剔除: %s", s)
	}
	// 序列化后 assistant 必须仍有 tool_calls（不能变成裸 assistant + tool）
	var parsed []map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	for i, m := range parsed {
		if m["role"] == "assistant" {
			if _, ok := m["tool_calls"]; !ok {
				t.Errorf("assistant 丢了 tool_calls（第 %d 条）: %v", i, m)
			}
		}
	}
}

// TestIsBlankContent 空白 content 判定。
//
// 上游要求 assistant 消息 content 非空白，实测：
//
//	null / "" / 缺失 → 400（业务码 4001）
//	" "（一个空格）  → 200
func TestIsBlankContent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{``, true, "字段缺失"},
		{`null`, true, "null"},
		{`""`, true, "空字符串"},
		{`"   "`, false, "纯空白字符串 —— 上游接受（只要不是完全为空）"},
		{`" "`, false, "一个空格 —— 上游接受它，**必须**判为非空白（否则规整不幂等）"},
		{`"  "`, false, "两个空格同理"},
		{`"hello"`, false, "正常内容"},
		{`[]`, true, "空片段数组"},
		{`[{"type":"text","text":"x"}]`, false, "有内容的片段数组"},
	}
	for _, c := range cases {
		if got := isBlankContent([]byte(c.in)); got != c.want {
			t.Errorf("isBlankContent(%q) = %v want %v（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestNormalizeAssistantContent 把"无 tool_calls 且 content 空白"的 assistant
// 补成一个空格。
//
// 为什么必须做：cleanupToolPairing 会把 `content:null + tool_calls` 的 assistant
// 裁成"既无 tool_calls 又无 content"的空壳 —— 而上游对空白 content 返回 400。
// 也就是说**自愈反而制造了失败**。这是实测发现的，不是推理出来的。
func TestNormalizeAssistantContent(t *testing.T) {
	msgs := []chatMessage{
		userMsg("hi"),
		{Role: "assistant"}, // 空壳（缺失 content）
		{Role: "assistant", Content: []byte("null")},   // 空壳（null）
		{Role: "assistant", Content: []byte(`""`)},     // 空壳（空串）
		{Role: "assistant", Content: []byte(`"real"`)}, // 有内容 → 不动
		assistantWithCalls("c1"),                       // 有 tool_calls → 不动
		userMsg("bye"),
	}
	out, changed := normalizeAssistantContent(msgs)
	if !changed {
		t.Fatal("应发生规整")
	}
	// 前三个空壳应被补成 " "
	for i := 1; i <= 3; i++ {
		if isBlankContent(out[i].Content) {
			t.Errorf("第 %d 条仍为空白: %q", i, string(out[i].Content))
		}
	}
	// 有内容的不该被改
	if string(out[4].Content) != `"real"` {
		t.Errorf("有内容的 assistant 被改了: %q", string(out[4].Content))
	}
	// 有 tool_calls 的不该被改（它的 content 仍是 nil，但那不影响 ——
	// 带 tool_calls 的 assistant 上游是接受的）
	if len(out[5].ToolCalls) != 1 {
		t.Errorf("有 tool_calls 的不该被规整: %+v", out[5])
	}
}

// TestFixToolPairingNormalizesAfterCleanup 端到端：孤儿 tool_calls 被裁掉后，
// 剩下的空壳 assistant 必须被规整 —— 否则请求仍会被上游 400。
func TestFixToolPairingNormalizesAfterCleanup(t *testing.T) {
	msgs := []chatMessage{
		userMsg("hi"),
		{Role: "assistant", Content: []byte("null"), ToolCalls: []chatToolCall{tc("call_x")}},
		{Role: "tool", ToolCallID: "call_orphan", Content: []byte(`"orphan"`)},
		userMsg("next"),
	}
	out, changed := fixToolPairing(msgs)
	if !changed {
		t.Fatal("应发生修复")
	}
	// 孤儿被剔除
	if len(out) != 3 {
		t.Fatalf("应剩 3 条，实际 %d: %+v", len(out), out)
	}
	// 关键：剩下的 assistant 不能是空白 content
	for i, m := range out {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && isBlankContent(m.Content) {
			t.Errorf("第 %d 条仍是空壳 assistant —— 上游会返回 4001: %+v", i, m)
		}
	}
}
