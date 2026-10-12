package realm

import "testing"

// TestKnown 精确匹配：不裁剪空白、不归一大小写。
//
// 精确匹配是刻意的 —— 模型名前缀解析必须区分 "trae:x" 与 " trae:x"，
// 后者是含冒号的裸模型名。
func TestKnown(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{Trae, true},
		{Solo, true},
		{" trae", false},
		{"trae ", false},
		{"TRAE", false},
		{"SOLO", false},
		{"", false},
		{"cn", false},
		{"global", false},
	}
	for _, c := range cases {
		if got := Known(c.in); got != c.want {
			t.Errorf("Known(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{Trae, Trae},
		{Solo, Solo},
		{" solo ", Solo},
		{"", Trae}, // 空 → 默认域
		{"unknown", Trae},
		{"CN", Trae}, // 未知（大小写不符）→ 默认域
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestResolveModel 覆盖前缀协议的各种形态。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"trae:glm-5.2", Trae, "glm-5.2"},
		{"solo:glm-5.2", Solo, "glm-5.2"},
		{"glm-5.2", Trae, "glm-5.2"}, // 无前缀 → 默认域
		{"trae:", Trae, ""},          // 空前缀裸名仍剥离
		{"solo:DeepSeek-V4-Pro-Official", Solo, "DeepSeek-V4-Pro-Official"},
		{"deepseek:v3", Trae, "deepseek:v3"}, // 未登记前缀不剥离
		{"cn:glm-5.2", Trae, "cn:glm-5.2"},   // 本项目不认 cn 域
		{"", Trae, ""},
		{":model", Trae, ":model"},
		{"TRAE:glm-5.2", Trae, "TRAE:glm-5.2"},        // 大小写敏感
		{" trae:glm-5.2", Trae, " trae:glm-5.2"},      // 前导空白不裁剪
		{"trae:glm-5.2:extra", Trae, "glm-5.2:extra"}, // 只剥第一个冒号前段
	}
	for _, c := range cases {
		r, bare := ResolveModel(c.in)
		if r != c.wantRealm || bare != c.wantBare {
			t.Errorf("ResolveModel(%q)=(%q,%q) want (%q,%q)", c.in, r, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestAll 返回稳定顺序的副本，调用方修改不影响内部状态。
func TestAll(t *testing.T) {
	got := All()
	want := []string{Trae, Solo, TraeIntl, SoloIntl}
	if len(got) != len(want) {
		t.Fatalf("All() len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("All()[%d]=%q want %q", i, got[i], want[i])
		}
	}
	got[0] = "mutated"
	if All()[0] != Trae {
		t.Error("All() 返回值被修改后污染了内部状态")
	}
}
