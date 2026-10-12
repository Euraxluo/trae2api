package server

import (
	"fmt"
	"net/http"
	"strings"
)

// 本文件处理**图片输入的可用性**。
//
// ## 背景：一次彻底的实测
//
// 上游的 `chat_v3` 端点**不接受内联 base64 图片**。这不是猜测 —— 我拿真实凭据
// 直接对上游试了 **11 种** 出站片段形态（见 internal/traectl/probe_image_test.go，
// 需 TRAE_LIVE_IMAGE=1 才会跑）：
//
//	A  文本 text 字段 + image_url/value/mimeType
//	B  文本 value 字段 + image_url/value/mimeType
//	C  image_url + value/mime_type（snake_case）
//	D  image + value/mimeType
//	E  image_url 扁平 mimeType/data
//	F  image_url + image_url.url（OpenAI 风格）
//	G  纯文本对照（value 字段）
//	H  纯文本对照（text 字段）          ← **唯一被接受的**
//	I  只有图片（无文本）
//	J  图片在前、文本在后
//	K  image_url + value + audience
//	L  type=data + mimeType + data
//	M  image_url + value{data 不带前缀}
//
// **11 种全部返回业务码 4001「param is invalid」，只有 H（纯文本）成功。**
// 图片尺寸也不是原因（8x8 与 256x256 结果相同）。
//
// 顺带确认了一个**我的实现 bug**：文本片段必须用 `text` 字段，
// 用客户端内部的 `value` 字段会被拒（G 失败 / H 成功）。我原先是对的，
// 但一度按客户端内部结构改成了 `value` —— 实测把它挡了回来。
//
// ## 客户端的真实做法
//
// 客户端不内联发图，而是先**上传到对象存储**再引用 URI。从 ai-completion
// 扩展里找到的协议方法名：
//
//	multimodal/get_resource_upload_token   取上传凭证
//	multimodal/get_resource_url            由 TOS URI 换可访问 URL
//	multimodal/check_image_content         校验图片内容
//
// 即：取 token → 上传到 TOS → 拿 URI → 在消息里引用该 URI。
// 这些是**扩展与渲染进程之间的 IPC 方法名**，对应的 HTTP 端点与请求体
// 在扩展实现里，我未能可靠还原，所以**没有实现这条链路**。
//
// ## 因此本网关的处理
//
// **明确失败，而不是把请求转上去让上游回一个笼统的 4001。**
//
// 为什么：上游的 4001 只说"param is invalid"，用户完全看不出是图片的问题，
// 更看不出该怎么绕过。一个明确的 400 + 原因 + 替代方案，比一个语焉不详的
// 上游错误好得多 —— 尤其是这个功能的失败模式是"看起来请求发出去了"。
//
// 若将来还原出了上传链路，把 `trae.vision_enabled` 设为 true 即可放行
//（届时入站转换已经是正确形态）。

// hasImagePart 报告请求里是否含图片片段。
//
// 只看 OpenAI 与 Anthropic 两种入站形态 —— 这两种是 parseContent 支持的。
func hasImagePart(req *chatRequest) bool {
	for _, m := range req.Messages {
		for _, p := range parseContent(m.Content) {
			if p.Type == "image_url" && strings.TrimSpace(p.ImageURL) != "" {
				return true
			}
		}
	}
	return false
}

// rejectVisionIfUnsupported 在图片不受支持时**提前明确失败**。
//
// 返回 true 表示已写出错误响应，调用方应立即返回。
func (s *Server) rejectVisionIfUnsupported(w http.ResponseWriter, req *chatRequest) bool {
	if s.cfg != nil && s.cfg.Trae.VisionEnabled {
		return false // 显式开启 → 放行（见文件头说明）
	}
	if !hasImagePart(req) {
		return false
	}
	writeOpenAIError(w, http.StatusBadRequest, "vision_unavailable",
		"当前上游不接受内联图片：Trae 的 chat 接口要求图片先上传到对象存储、再以 URI 引用，"+
			"而本网关尚未实现该上传链路。"+
			"已实测 11 种出站片段形态（含官方客户端使用的 image_url+value{mimeType,data}），"+
			"全部被上游以业务码 4001 拒绝，仅纯文本可通。"+
			"请改用文本描述，或把 trae.vision_enabled 设为 true 强制放行（通常仍会失败）。")
	return true
}

// visionStatus 供 /status 与 /v1/models 透出图片可用性。
func (s *Server) visionStatus() map[string]any {
	enabled := s.cfg != nil && s.cfg.Trae.VisionEnabled
	m := map[string]any{
		"available": enabled,
		"reason": "上游 chat 接口不接受内联图片（实测 11 种形态全部 4001）；" +
			"官方客户端走「取上传凭证 → 上传对象存储 → 引用 URI」的链路，本网关未实现",
	}
	if !enabled {
		m["workaround"] = "用文本描述图片内容；或设 trae.vision_enabled=true 强制尝试"
	}
	return m
}

// visionErrorHint 给日志用的一行说明。
func visionErrorHint() string {
	return fmt.Sprintf("图片输入不可用：%s", "上游要求先上传到对象存储再引用 URI")
}
