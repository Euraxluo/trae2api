package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api/internal/config"
)

func visionCfg(enabled bool) *config.Config {
	c := config.Default()
	c.Trae.VisionEnabled = enabled
	return c
}

// TestVisionRejectedWithClearError 图片在默认配置下必须**明确失败**，
// 而不是转上去让上游回一个笼统的 4001。
//
// 为什么这条重要：上游的 4001 只说 "param is invalid"，用户完全看不出是
// 图片的问题、更看不出怎么绕过。而这个功能的失败模式是"看起来请求发出去了"。
func TestVisionRejectedWithClearError(t *testing.T) {
	s := &Server{cfg: visionCfg(false)}

	cases := []struct {
		name    string
		content string
		want    bool // 是否应被拒
	}{
		{"OpenAI image_url", `[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`, true},
		{"Anthropic base64", `[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]`, true},
		{"Anthropic url", `[{"type":"image","source":{"type":"url","url":"https://x/a.png"}}]`, true},
		{"纯文本（不该被拒）", `"你好"`, false},
		{"文本片段数组（不该被拒）", `[{"type":"text","text":"你好"}]`, false},
	}
	for _, c := range cases {
		req := &chatRequest{Messages: []chatMessage{
			{Role: "user", Content: json.RawMessage(c.content)},
		}}
		w := httptest.NewRecorder()
		rejected := s.rejectVisionIfUnsupported(w, req)
		if rejected != c.want {
			t.Errorf("%s: 被拒 = %v want %v", c.name, rejected, c.want)
			continue
		}
		if !rejected {
			continue
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d want 400", c.name, w.Code)
		}
		body := w.Body.String()
		// 错误必须可行动：说清原因 + 给出替代方案
		for _, want := range []string{"vision_unavailable", "对象存储", "trae.vision_enabled"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: 错误信息应含 %q，实际: %s", c.name, want, body)
			}
		}
	}
}

// TestVisionEnabledPassesThrough 显式开启时放行（供将来实现上传链路后使用）。
func TestVisionEnabledPassesThrough(t *testing.T) {
	s := &Server{cfg: visionCfg(true)}
	req := &chatRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(
			`[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`)},
	}}
	w := httptest.NewRecorder()
	if s.rejectVisionIfUnsupported(w, req) {
		t.Error("vision_enabled=true 时应放行")
	}
}

// TestVisionStatus 状态端点要能解释"为什么发图不行"。
func TestVisionStatus(t *testing.T) {
	off := (&Server{cfg: visionCfg(false)}).visionStatus()
	if off["available"] != false {
		t.Error("默认应报告不可用")
	}
	if _, ok := off["workaround"]; !ok {
		t.Error("不可用时应给出替代方案")
	}
	if !strings.Contains(off["reason"].(string), "4001") {
		t.Error("原因里应带上实测证据（4001）")
	}

	on := (&Server{cfg: visionCfg(true)}).visionStatus()
	if on["available"] != true {
		t.Error("开启后应报告可用")
	}
	if _, ok := on["workaround"]; ok {
		t.Error("可用时不该再给替代方案")
	}
}

// TestHasImagePart 图片片段检测。
func TestHasImagePart(t *testing.T) {
	no := &chatRequest{Messages: []chatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if hasImagePart(no) {
		t.Error("纯文本不该被判定为含图片")
	}
	// 空 URL 的图片片段不算（parseContent 会丢掉它）
	empty := &chatRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":""}}]`)}}}
	if hasImagePart(empty) {
		t.Error("空 URL 不该算含图片")
	}
	yes := &chatRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]`)}}}
	if !hasImagePart(yes) {
		t.Error("应检测到图片")
	}
	// 多轮里任意一条含图都要检测到
	multi := &chatRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(`"第一轮"`)},
		{Role: "assistant", Content: json.RawMessage(`"回复"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]`)},
	}}
	if !hasImagePart(multi) {
		t.Error("多轮中后一条含图也应检测到")
	}
}
