package cli

import (
	"strings"
	"testing"
)

// TestRuneWidth 宽度判定：中文 2 列、ASCII 1 列、组合字符 0 列。
//
// 这是所有表格对齐的基础。搞错的话，只要内容里有中文就会整列错位，
// 而 `len()` 与 `utf8.RuneCountInString` 都会给出错误答案。
func TestRuneWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
		why  string
	}{
		{'a', 1, "ASCII 字母"},
		{'Z', 1, "ASCII 大写"},
		{'0', 1, "数字"},
		{' ', 1, "空格"},
		{'中', 2, "汉字"},
		{'文', 2, "汉字"},
		{'，', 2, "全角逗号"},
		{'。', 2, "全角句号"},
		{'（', 2, "全角括号"},
		{'あ', 2, "平假名"},
		{'ア', 2, "片假名"},
		{'한', 2, "韩文"},
		{'Ａ', 2, "全角字母"},
		{'\u0301', 0, "组合重音（不占位）"},
		{0, 0, "NUL"},
		{'✅', 1, "emoji **刻意**按 1 列（终端宽度实现相关，表格里因此不用 emoji）"},
		{'─', 1, "U+2500 制表符（East Asian Width=Ambiguous，按 1 列）"},
	}
	for _, c := range cases {
		if got := RuneWidth(c.r); got != c.want {
			t.Errorf("RuneWidth(%q) = %d want %d（%s）", c.r, got, c.want, c.why)
		}
	}
}

// TestDisplayWidth 字符串宽度。
func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"中文", 4},
		{"a中b", 4},
		{"Trae CN", 7},
		{"模型 ID", 7}, // 模(2)型(2)空格(1) I(1) D(1) = 7
	}
	for _, c := range cases {
		if got := DisplayWidth(c.s); got != c.want {
			t.Errorf("DisplayWidth(%q) = %d want %d", c.s, got, c.want)
		}
	}
}

// TestPad 右侧补空格，且**不截断**超宽内容。
func TestPad(t *testing.T) {
	// "中文" 占 4 列，补到 10 列 → 加 6 个空格
	got := Pad("中文", 10)
	if DisplayWidth(got) != 10 {
		t.Errorf("补后宽度 = %d want 10", DisplayWidth(got))
	}
	if !strings.HasPrefix(got, "中文") {
		t.Error("内容被改动")
	}
	// 超宽时不截断（截断会把中文切半变乱码）
	long := "中文中文中文"
	if got := Pad(long, 4); got != long {
		t.Errorf("超宽内容不应被截断: %q", got)
	}
}

// TestPadLeft 左侧补空格（数字列右对齐）。
func TestPadLeft(t *testing.T) {
	got := PadLeft("42", 6)
	if DisplayWidth(got) != 6 {
		t.Errorf("宽度 = %d want 6", DisplayWidth(got))
	}
	if !strings.HasSuffix(got, "42") {
		t.Error("内容应右对齐")
	}
}

// TestTruncateDoesNotSplitWideChar 截断**绝不能**把一个宽字符切一半。
//
// 切一半会产出非法 UTF-8，终端里显示成乱码，比"少显示一个字"糟得多。
func TestTruncateDoesNotSplitWideChar(t *testing.T) {
	s := "中文中文中文" // 12 列
	got := Truncate(s, 5)
	if DisplayWidth(got) > 5 {
		t.Errorf("截断后宽度 %d 超过上限 5", DisplayWidth(got))
	}
	// 必须是合法 UTF-8 且以省略号结尾
	if !strings.HasSuffix(got, "…") {
		t.Errorf("应带省略号: %q", got)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("出现替换字符，说明切坏了: %q", got)
		}
	}
	// 刚好放得下时不截断
	if got := Truncate("中文", 4); got != "中文" {
		t.Errorf("放得下时不该截断: %q", got)
	}
}

// TestTableAlignment 表格的每一行显示宽度必须完全一致。
//
// 这是"中文表格错位"的直接回归测试：只要宽度算错，某一行的分隔线
// 就会与内容对不上。
func TestTableAlignment(t *testing.T) {
	tbl := &Table{Headers: []string{"UID", "标签", "状态"}}
	tbl.Add("trae-cn-916348159159136", "Trae CN", "✅")
	tbl.Add("solo-cn-916348159159136", "TRAE SOLO CN", "✅")
	tbl.Add("short", "短", "—")

	out := strings.TrimRight(tbl.Render(), "\n")
	lines := strings.Split(out, "\n")
	// 结构：上分隔 + 表头 + 分隔 + 3 数据行 + 下分隔 = 7 行
	if len(lines) != 7 {
		t.Fatalf("行数 = %d want 7", len(lines))
	}
	want := DisplayWidth(lines[0])
	for i, ln := range lines {
		if got := DisplayWidth(ln); got != want {
			t.Errorf("第 %d 行宽度 %d != %d\n  %q", i, got, want, ln)
		}
	}
	// 分隔线必须只用 ASCII（制表符在中文终端里常被当 2 列宽，会错位）
	for _, ln := range lines {
		if strings.ContainsAny(ln, "─│┌┐└┘├┤┬┴┼") {
			t.Errorf("分隔线含制表符（中文终端下宽度不确定）: %q", ln)
		}
	}
}

// TestTableRaggedRows 行列数不齐时不应 panic，且仍能对齐。
func TestTableRaggedRows(t *testing.T) {
	tbl := &Table{Headers: []string{"A", "B", "C"}}
	tbl.Add("1")                // 少两列
	tbl.Add("1", "2", "3", "4") // 多一列
	out := tbl.Render()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := DisplayWidth(lines[0])
	for i, ln := range lines {
		if got := DisplayWidth(ln); got != want {
			t.Errorf("第 %d 行宽度 %d != %d: %q", i, got, want, ln)
		}
	}
}

// TestDuration 时长渲染。
func TestDuration(t *testing.T) {
	cases := []struct {
		sec  int
		want string
	}{
		{0, "0s"}, {59, "59s"}, {60, "1m0s"}, {90, "1m30s"},
		{3600, "1h0m"}, {3661, "1h1m"}, {86399, "23h59m"},
	}
	for _, c := range cases {
		if got := Duration(c.sec); got != c.want {
			t.Errorf("Duration(%d) = %q want %q", c.sec, got, c.want)
		}
	}
}

// TestBool 布尔渲染。
func TestBool(t *testing.T) {
	if Bool(true) != "是" || Bool(false) != "否" {
		t.Error("布尔渲染不符")
	}
}

// TestUsageErrorType 用法错误必须能被 errors.As 识别（决定退出码 2）。
func TestUsageErrorType(t *testing.T) {
	err := Usagef("缺少 %s", "参数")
	var ue *UsageError
	if !asUsage(err, &ue) {
		t.Fatal("Usagef 产出的错误应可被识别为用法错误")
	}
	if !strings.Contains(ue.Msg, "参数") {
		t.Errorf("消息不符: %q", ue.Msg)
	}
}

// asUsage 是 errors.As 的小包装（避免测试文件 import errors 只为这一处）。
func asUsage(err error, target **UsageError) bool {
	for err != nil {
		if ue, ok := err.(*UsageError); ok {
			*target = ue
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestClientBaseTrim 客户端应容忍尾部斜杠。
func TestClientBaseTrim(t *testing.T) {
	c := NewClient("http://127.0.0.1:7870///", "k")
	if c.Base() != "http://127.0.0.1:7870" {
		t.Errorf("尾部斜杠未清理: %q", c.Base())
	}
}

// TestAPIErrorFormat APIError 的消息要包含状态码与上游给的说明。
func TestAPIErrorFormat(t *testing.T) {
	e := &APIError{Status: 401, Code: "invalid_api_key", Message: "Invalid API key"}
	msg := e.Error()
	for _, want := range []string{"401", "invalid_api_key", "Invalid API key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误消息缺 %q: %s", want, msg)
		}
	}
	// 没有 code 时不应出现空洞的格式
	e2 := &APIError{Status: 500, Message: "boom"}
	if strings.Contains(e2.Error(), "  : ") {
		t.Errorf("无 code 时格式不对: %s", e2.Error())
	}
}
