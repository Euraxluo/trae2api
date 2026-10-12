package traectl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestProbeImageShapes 直接对上游试多种**图片片段形态**，找出被接受的那一种。
//
// 为什么需要它：上游对结构错误只返回笼统的业务码 4001「param is invalid」，
// 不说是哪个字段错。而带图的请求会**整体失败**，从外部看与"入站格式没解析对"
// 完全一样 —— 极易误判。所以只能拿真实凭据逐个形态试。
//
// 需要 TRAE_LIVE_IMAGE=1（会真实消耗额度，但极少量）。
func TestProbeImageShapes(t *testing.T) {
	if os.Getenv("TRAE_LIVE_IMAGE") != "1" {
		t.Skip("需 TRAE_LIVE_IMAGE=1（会对上游发真实请求）")
	}
	st, err := OpenStore(Products[0], "")
	if err != nil {
		t.Skipf("打开真实凭据失败: %v", err)
	}
	if err := st.Refresh(context.Background()); err != nil {
		t.Skipf("续期失败: %v", err)
	}
	cred, machineID, _ := st.Snapshot()

	// 256x256 左红右蓝 PNG（内联避免依赖文件）
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAEklEQVR4nGP4z8CAFWEXHbQSACj/P8Fu7N9hAAAAAElFTkSuQmCC"

	// 每个候选形态就是一个完整的 content 数组
	candidates := []struct {
		name    string
		content string
	}{
		{"A 文本用 text 字段 + 图片 image_url/value/mimeType",
			`[{"type":"text","text":"这张图什么颜色？只回答颜色。"},
			  {"type":"image_url","value":{"mimeType":"image/png","data":"` + pngB64 + `"}}]`},

		{"B 文本用 value 字段 + 图片 image_url/value/mimeType",
			`[{"type":"text","value":"这张图什么颜色？只回答颜色。"},
			  {"type":"image_url","value":{"mimeType":"image/png","data":"` + pngB64 + `"}}]`},

		{"C image_url + value/mime_type（snake）",
			`[{"type":"text","value":"这张图什么颜色？只回答颜色。"},
			  {"type":"image_url","value":{"mime_type":"image/png","data":"` + pngB64 + `"}}]`},

		{"D image + value/mimeType",
			`[{"type":"text","value":"这张图什么颜色？只回答颜色。"},
			  {"type":"image","value":{"mimeType":"image/png","data":"` + pngB64 + `"}}]`},

		{"E image_url 扁平 mimeType/data",
			`[{"type":"text","value":"这张图什么颜色？只回答颜色。"},
			  {"type":"image_url","mimeType":"image/png","data":"` + pngB64 + `"}]`},

		{"F image_url + image_url.url（OpenAI 风格）",
			`[{"type":"text","value":"这张图什么颜色？只回答颜色。"},
			  {"type":"image_url","image_url":{"url":"data:image/png;base64,` + pngB64 + `"}}]`},

		{"I 只有图片（无文本）",
			`[{"type":"image_url","value":{"mimeType":"image/png","data":"` + pngB64 + `"}}]`},

		{"J 图片在前，文本在后",
			`[{"type":"image_url","value":{"mimeType":"image/png","data":"` + pngB64 + `"}},
			  {"type":"text","text":"这张图什么颜色？"}]`},

		{"K image_url + value + audience",
			`[{"type":"text","text":"这张图什么颜色？"},
			  {"type":"image_url","value":{"mimeType":"image/png","data":"` + pngB64 + `"},"audience":"user"}]`},

		{"L type=data + mimeType + data",
			`[{"type":"text","text":"这张图什么颜色？"},
			  {"type":"data","mimeType":"image/png","data":"` + pngB64 + `"}]`},

		{"M image_url + value{data 不带前缀}",
			`[{"type":"text","text":"这张图什么颜色？"},
			  {"type":"image_url","value":{"mimeType":"image/png","data":"` + pngB64 + `"}}]`}, {"G 纯文本对照（value 字段）",
			`[{"type":"text","value":"只回复两个字：你好"}]`},

		{"H 纯文本对照（text 字段）",
			`[{"type":"text","text":"只回复两个字：你好"}]`},
	}

	for _, c := range candidates {
		body := fmt.Sprintf(`{"messages":[{"role":"user","content":%s}],
			"model":"Doubao-Seed-2.1-Pro","function":"chat_v3","stream":true,
			"request_id":%q,"session_id":%q}`,
			c.content, randHexN(16), randHexN(16))

		status, resp := rawChat(t, st, cred, machineID, []byte(body))
		s := string(resp)
		verdict := "❌ 拒绝"
		switch {
		case bytes.Contains(resp, []byte(`"code":4001`)):
			verdict = "❌ 4001"
		case bytes.Contains(resp, []byte(`sse:error`)):
			verdict = "❌ 其它错误"
		case status == 200 && (bytes.Contains(resp, []byte("content")) ||
			bytes.Contains(resp, []byte("reasoning"))):
			verdict = "✅ 接受"
		case status == 200:
			verdict = "❓ 无内容"
		}
		// 把错误原文也打出来（上游只给笼统的 "param is invalid"，但值得确认）
		detail := ""
		if i := strings.Index(s, `"message":"`); i >= 0 {
			end := strings.Index(s[i+11:], `"`)
			if end > 0 {
				detail = s[i+11 : i+11+min(end, 70)]
			}
		}
		t.Logf("%s  %-46s  HTTP %d  len=%d  %s", verdict, c.name, status, len(resp), detail)
	}
}

// rawChat 用给定 body 直接打上游（绕过 buildBody，便于试任意形态）。
func rawChat(t *testing.T, st *Store, cred Credentials, machineID string, body []byte) (int, []byte) {
	t.Helper()
	c := &Client{Store: st, IdleTimeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		DefaultChatBase+chatIDEPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.headers(cred, machineID) {
		req.Header.Set(k, v)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	// ⚠️ 必须读**完整流**：上游的 4001 是在 SSE 流中间发出来的，
	// 只读前几 KB 会拿到心跳帧（id:1），判定完全不可靠。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

// randHexN 生成 n 字节的 hex（测试内联，避免依赖 server 包的实现）。
func randHexN(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(time.Now().UnixNano() >> (i % 8))
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hexd[x>>4]
		out[i*2+1] = hexd[x&0x0f]
	}
	return string(out)
}

// 保证 json 被使用（部分形态构造用到）。
var _ = json.Marshal

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
