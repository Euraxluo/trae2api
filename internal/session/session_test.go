package session

import (
	"testing"
	"time"
)

func TestBindResolve(t *testing.T) {
	r := New(time.Minute)
	if _, ok := r.Resolve("k1"); ok {
		t.Fatal("未绑定不应命中")
	}
	r.Bind("k1", "uid-a")
	if uid, ok := r.Resolve("k1"); !ok || uid != "uid-a" {
		t.Fatalf("Resolve = (%q,%v) want (uid-a,true)", uid, ok)
	}
	// 覆盖绑定
	r.Bind("k1", "uid-b")
	if uid, _ := r.Resolve("k1"); uid != "uid-b" {
		t.Fatalf("重复 Bind 应覆盖，得到 %q", uid)
	}
	// 空键/空 uid 不应写入
	r.Bind("", "uid-c")
	r.Bind("k2", "")
	if _, ok := r.Resolve(""); ok {
		t.Error("空键不应命中")
	}
	if _, ok := r.Resolve("k2"); ok {
		t.Error("空 uid 不应绑定")
	}
}

func TestTTLExpiry(t *testing.T) {
	r := New(30 * time.Millisecond)
	r.Bind("k", "uid")
	if _, ok := r.Resolve("k"); !ok {
		t.Fatal("刚绑定应命中")
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := r.Resolve("k"); ok {
		t.Fatal("超过 TTL 应失效")
	}
}

// TestResolveRefreshesLastSeen 每次命中都应续期（活跃会话不该被 GC 掉）。
func TestResolveRefreshesLastSeen(t *testing.T) {
	r := New(60 * time.Millisecond)
	r.Bind("k", "uid")
	for i := 0; i < 3; i++ {
		time.Sleep(25 * time.Millisecond)
		if _, ok := r.Resolve("k"); !ok {
			t.Fatalf("第 %d 次 Resolve 应命中（活跃会话不应过期）", i+1)
		}
	}
}

func TestUnbind(t *testing.T) {
	r := New(time.Minute)
	r.Bind("k", "uid")
	if !r.Unbind("k") {
		t.Fatal("Unbind 已存在的键应返回 true")
	}
	if r.Unbind("k") {
		t.Fatal("Unbind 不存在的键应返回 false")
	}
	if _, ok := r.Resolve("k"); ok {
		t.Fatal("解绑后不应命中")
	}
}

func TestCount(t *testing.T) {
	r := New(30 * time.Millisecond)
	r.Bind("a", "1")
	r.Bind("b", "2")
	if n := r.Count(); n != 2 {
		t.Fatalf("Count = %d want 2", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := r.Count(); n != 0 {
		t.Fatalf("过期后 Count = %d want 0", n)
	}
}

func TestNewDefaultTTL(t *testing.T) {
	r := New(0) // <=0 应回落 30 分钟
	if r.ttl != 30*time.Minute {
		t.Errorf("ttl = %v want 30m", r.ttl)
	}
}

// TestExtractKey 覆盖各种会话标识字段与嵌套 metadata。
func TestExtractKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"conversation_id", `{"conversation_id":"c1"}`, "conversation_id:c1"},
		{"conversationId", `{"conversationId":"c2"}`, "conversationId:c2"},
		{"session_id", `{"session_id":"s1"}`, "session_id:s1"},
		{"thread_id", `{"thread_id":"t1"}`, "thread_id:t1"},
		{"metadata 嵌套", `{"metadata":{"session_id":"m1"}}`, "metadata.session_id:m1"},
		{"优先 conversation_id", `{"session_id":"s","conversation_id":"c"}`, "conversation_id:c"},
		{"无会话字段", `{"messages":[]}`, ""},
		{"空体", ``, ""},
		{"非法 JSON", `{`, ""},
		{"空值不采用", `{"conversation_id":""}`, ""},
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("%s: ExtractKey=%q want %q", c.name, got, c.want)
		}
	}
}

// TestFallbackKey 用首条 user 消息派生会话级键。
func TestFallbackKey(t *testing.T) {
	// 同一条开场白 → 同键（即使后续历史增长）
	b1 := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"你好"}]}`
	b2 := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"你好"},{"role":"assistant","content":"在"},{"role":"user","content":"继续"}]}`
	k1, k2 := FallbackKey([]byte(b1)), FallbackKey([]byte(b2))
	if k1 == "" {
		t.Fatal("应能派生键")
	}
	if k1 != k2 {
		t.Errorf("同一开场白应派生相同键: %q vs %q", k1, k2)
	}
	// 不同开场白 → 不同键
	b3 := `{"messages":[{"role":"user","content":"另一个问题"}]}`
	if FallbackKey([]byte(b3)) == k1 {
		t.Error("不同开场白应派生不同键")
	}
	// 内容片段数组形式
	b4 := `{"messages":[{"role":"user","content":[{"type":"text","text":"片段"}]}]}`
	if FallbackKey([]byte(b4)) == "" {
		t.Error("片段数组形式应能派生键")
	}
	// 无 user 消息
	if FallbackKey([]byte(`{"messages":[{"role":"system","content":"s"}]}`)) != "" {
		t.Error("无 user 消息应返回空键")
	}
	// 非法 JSON
	if FallbackKey([]byte(`{`)) != "" {
		t.Error("非法 JSON 应返回空键")
	}
}

// TestFallbackKeyIgnoresAssistant 只有 user 消息参与派生（assistant 内容会变，不能入键）。
func TestFallbackKeyIgnoresAssistant(t *testing.T) {
	a := `{"messages":[{"role":"user","content":"Q"}]}`
	b := `{"messages":[{"role":"user","content":"Q"},{"role":"assistant","content":"完全不同的回复"}]}`
	if FallbackKey([]byte(a)) != FallbackKey([]byte(b)) {
		t.Error("assistant 内容变化不应改变会话键")
	}
}
