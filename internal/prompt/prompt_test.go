package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// bodyOf 把请求体解析成通用结构，便于断言 messages。
func bodyOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v\n%s", err, raw)
	}
	return m
}

func messagesOf(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	m := bodyOf(t, raw)
	msgs, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages 字段缺失或类型不对: %v", m["messages"])
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, x := range msgs {
		mm, ok := x.(map[string]any)
		if !ok {
			t.Fatalf("消息不是对象: %v", x)
		}
		out = append(out, mm)
	}
	return out
}

// TestParseMode 模式解析：空 = passthrough；非法值报错（不静默回落）。
func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{
		"":            ModePassthrough,
		"passthrough": ModePassthrough,
		"PASSTHROUGH": ModePassthrough,
		" custom ":    ModeCustom,
		"append":      ModeAppend,
	} {
		got, err := ParseMode(in)
		if err != nil {
			t.Errorf("ParseMode(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %q want %q", in, got, want)
		}
	}
	// 非法值必须报错 —— 静默回落会让用户以为配置生效了
	if _, err := ParseMode("replace"); err == nil {
		t.Error("非法模式应报错")
	}
}

// TestPassthroughIsNoop passthrough 必须**逐字节**不动请求体。
func TestPassthroughIsNoop(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"原样"},{"role":"user","content":"hi"}],"temperature":0.7}`)
	for _, r := range []*Rewriter{
		New(ModePassthrough, "注入内容"),
		New(ModeCustom, ""), // 空文本也视为不启用
		nil,
	} {
		if r.Enabled() {
			t.Error("不应报告启用")
		}
		if got := r.Apply(body); string(got) != string(body) {
			t.Errorf("passthrough 改动了请求体:\n%s\n%s", body, got)
		}
	}
}

// TestCustomReplacesAllSystem custom 模式：丢掉全部系统消息，只留我们的。
func TestCustomReplacesAllSystem(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"客户端的系统提示"},
		{"role":"developer","content":"另一个系统级消息"},
		{"role":"user","content":"你好"}
	]}`)
	got := New(ModeCustom, "网关的系统提示").Apply(body)
	msgs := messagesOf(t, got)

	if len(msgs) != 2 {
		t.Fatalf("应只剩 2 条（我们的 system + user），实际 %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "网关的系统提示" {
		t.Errorf("首条应是我们注入的 system: %v", msgs[0])
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "你好" {
		t.Errorf("user 消息应原样保留: %v", msgs[1])
	}
	for _, m := range msgs {
		if m["content"] == "客户端的系统提示" || m["content"] == "另一个系统级消息" {
			t.Errorf("客户端的系统消息未被替换: %v", m)
		}
	}
	// 其他字段必须保留
	if bodyOf(t, got)["model"] != "m" {
		t.Error("model 字段丢失")
	}
}

// TestAppendToExistingSystem append 模式：保留并追加。
func TestAppendToExistingSystem(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"客户端提示"},{"role":"user","content":"hi"}]}`)
	got := New(ModeAppend, "网关追加").Apply(body)
	msgs := messagesOf(t, got)

	if len(msgs) != 2 {
		t.Fatalf("应仍为 2 条，实际 %d", len(msgs))
	}
	c, _ := msgs[0]["content"].(string)
	if !strings.HasPrefix(c, "客户端提示") || !strings.HasSuffix(c, "网关追加") {
		t.Errorf("追加结果不符: %q", c)
	}
}

// TestAppendWithoutExistingSystem 没有系统消息时应在最前面插入。
func TestAppendWithoutExistingSystem(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	got := New(ModeAppend, "网关追加").Apply(body)
	msgs := messagesOf(t, got)

	if len(msgs) != 3 {
		t.Fatalf("应变为 3 条，实际 %d", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "网关追加" {
		t.Errorf("应在最前插入 system: %v", msgs[0])
	}
	if msgs[1]["role"] != "user" {
		t.Errorf("原顺序应保持: %v", msgs[1])
	}
}

// TestDeveloperNormalizedToSystem developer 角色应归一成 system
// （上游不一定认识 developer）。
func TestDeveloperNormalizedToSystem(t *testing.T) {
	body := []byte(`{"messages":[{"role":"developer","content":"D"},{"role":"user","content":"u"}]}`)
	got := New(ModeAppend, "X").Apply(body)
	msgs := messagesOf(t, got)
	if msgs[0]["role"] != "system" {
		t.Errorf("developer 应归一成 system，得到 %v", msgs[0]["role"])
	}
}

// TestNonJSONUnchanged 非 JSON 请求体必须原样返回（改写绝不弄坏本来能用的请求）。
func TestNonJSONUnchanged(t *testing.T) {
	body := []byte(`not json at all`)
	got := New(ModeCustom, "X").Apply(body)
	if string(got) != string(body) {
		t.Errorf("非 JSON 被改动: %s", got)
	}
}

// TestNoMessagesUnchanged 没有 messages 字段时原样返回（可能是 /v1/completions 之类）。
func TestNoMessagesUnchanged(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"hello"}`)
	got := New(ModeCustom, "X").Apply(body)
	if string(got) != string(body) {
		t.Errorf("无 messages 时不该改动: %s", got)
	}
}

// TestContentArrayForm 内容为片段数组时也要能正确处理。
func TestContentArrayForm(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"数组形式"}]},{"role":"user","content":"u"}]}`)
	got := New(ModeAppend, "追加").Apply(body)
	msgs := messagesOf(t, got)
	c, _ := msgs[0]["content"].(string)
	if !strings.Contains(c, "数组形式") || !strings.Contains(c, "追加") {
		t.Errorf("片段数组形式的追加失败: %q", c)
	}
}

// TestApplyDegradedIsMinimal 降级改写：system 被换成极简提示词（不是追加）。
func TestApplyDegradedIsMinimal(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"可能触发校验的长提示"},{"role":"user","content":"u"}]}`)
	r := New(ModeAppend, "长注入")
	got := r.ApplyDegraded(body)
	msgs := messagesOf(t, got)

	if len(msgs) != 2 {
		t.Fatalf("应为 2 条，实际 %d", len(msgs))
	}
	if msgs[0]["content"] != Degraded {
		t.Errorf("降级后 system 应为极简提示词，得到 %q", msgs[0]["content"])
	}
	if strings.Contains(msgs[0]["content"].(string), "可能触发校验") {
		t.Error("降级应**替换**而不是追加")
	}
}

// TestApplyDegradedPassthroughNoop passthrough 下降级改写也是空操作
// （没注入过东西，就没什么可降级的）。
func TestApplyDegradedPassthroughNoop(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"原样"},{"role":"user","content":"u"}]}`)
	got := New(ModePassthrough, "X").ApplyDegraded(body)
	if string(got) != string(body) {
		t.Errorf("passthrough 下降级不该改动: %s", got)
	}
}

// TestDegradedIsShortAndNeutral 极简提示词应保持短小中立（降级目的是"能发出去"，
// 不是"换一套人格"）。
func TestDegradedIsShortAndNeutral(t *testing.T) {
	if len(Degraded) > 200 {
		t.Errorf("降级提示词过长（%d 字），应保持极简", len(Degraded))
	}
	if strings.Contains(strings.ToLower(Degraded), "you must") ||
		strings.Contains(strings.ToLower(Degraded), "never") {
		t.Error("降级提示词不应含强指令性内容")
	}
}

// TestEnabled 启用判定。
func TestEnabled(t *testing.T) {
	if !New(ModeCustom, "X").Enabled() {
		t.Error("custom + 非空文本应启用")
	}
	if New(ModeCustom, "   ").Enabled() {
		t.Error("空白文本不应启用")
	}
	if New(ModeAppend, "X").Enabled() == false {
		t.Error("append + 非空文本应启用")
	}
	if New(ModePassthrough, "X").Enabled() {
		t.Error("passthrough 不应启用")
	}
	var nilR *Rewriter
	if nilR.Enabled() {
		t.Error("nil 不应启用")
	}
	if nilR.Mode() != ModePassthrough {
		t.Error("nil 的 Mode 应为 passthrough")
	}
}

// TestInvalidModeFallsBackToPassthrough New 收到非法模式时回落而不是 panic。
func TestInvalidModeFallsBackToPassthrough(t *testing.T) {
	r := New(Mode("bogus"), "X")
	if r.Mode() != ModePassthrough || r.Enabled() {
		t.Errorf("非法模式应回落 passthrough，得到 %q enabled=%v", r.Mode(), r.Enabled())
	}
}

// TestPreservesOtherFields 改写不能丢请求体里的其他字段。
func TestPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"trae:glm-5.2","temperature":0.3,"max_tokens":100,"stream":true,
		"tools":[{"type":"function","function":{"name":"f"}}],
		"messages":[{"role":"user","content":"u"}]}`)
	got := New(ModeCustom, "S").Apply(body)
	m := bodyOf(t, got)
	for _, k := range []string{"model", "temperature", "max_tokens", "stream", "tools"} {
		if _, ok := m[k]; !ok {
			t.Errorf("字段 %q 丢失", k)
		}
	}
	if m["temperature"] != 0.3 || m["max_tokens"] != float64(100) || m["stream"] != true {
		t.Errorf("字段值被改动: %v", m)
	}
}
