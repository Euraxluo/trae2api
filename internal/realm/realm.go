// Package realm 定义 Trae2Api 支持的模型域（realm）。
//
// 设计说明（借鉴 workbuddy2api 的分域思路，代码独立实现）：
// realm 决定四件事 —— 模型名前缀、凭据来源、chat 请求体的 function 字段、
// 服务端下发的模型清单。把 realm 做成枚举而非散落的字面量，是为了新增渠道时
// 不必逐个 grep 修改（workbuddy2api 在这点上踩过坑：加第三个域要改 20 处）。
package realm

import "strings"

const (
	// Trae Trae CN —— function=chat_v3，凭据取自 %APPDATA%\Trae CN。
	Trae = "trae"
	// TraeIntl Trae 国际版（trae.ai）—— 与 Trae 是**两套独立账号体系与端点**。
	//
	// 为什么必须是独立 realm 而不是同一个：国内版与国际版的凭据不能互换，
	// 端点也不同。混在一起会让选号把国内账号派给国际请求（或反过来），
	// 表现为"账号明明健康却总是失败"。
	TraeIntl = "trae-intl"
	// SoloIntl TRAE SOLO 国际版。
	SoloIntl = "solo-intl"

	// Solo TRAE SOLO CN（TraeWork）—— function=solo_work_lite，
	// 凭据取自 %APPDATA%\TRAE SOLO CN。
	//
	// 与 Trae 的关系：**同一套协议、同一上游 host、同一账号与额度池**
	// （实测 ide_user_ent_usage 返回完全相同的数值）。差异只有三处：
	// 凭据目录、function 字段、服务端下发的模型清单。
	// 拆成两个 realm 是为了让模型名前缀（trae: / solo:）能表达"用哪个模型池"，
	// 并让选号与并发上限可以分开配置。
	Solo = "solo"
)

// ordered 已知域的稳定顺序（用于控制台/指标列展示，保证输出确定性）。
var ordered = []string{Trae, Solo, TraeIntl, SoloIntl}

var known = map[string]struct{}{
	Trae:     {},
	Solo:     {},
	TraeIntl: {},
	SoloIntl: {},
}

// Known 报告 r 是否为已登记的域（**精确匹配**：不裁剪空白、不归一大小写）。
//
// 精确匹配是刻意的：模型名前缀解析必须区分 "trae:xxx" 与 " trae:xxx" ——
// 后者是含冒号的裸模型名，不该被剥离。
func Known(r string) bool {
	_, ok := known[r]
	return ok
}

// All 返回全部已登记域（稳定顺序的副本）。
func All() []string {
	return append([]string(nil), ordered...)
}

// Normalize 归一化：已登记的域原样返回（去空白），其余（含空串）回落 Trae。
func Normalize(r string) string {
	r = strings.TrimSpace(r)
	if Known(r) {
		return r
	}
	return Trae
}

// ResolveModel 解析模型名前缀协议："[realm:]model"。
//
// 取第一个 ":"，前段是已登记的 realm 才剥离；否则视为裸名，realm=默认域。
// 大小写敏感（前缀必须是精确的小写枚举）。
func ResolveModel(model string) (r, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return Trae, model
	}
	prefix := model[:idx]
	if !Known(prefix) {
		return Trae, model
	}
	return prefix, model[idx+1:]
}
