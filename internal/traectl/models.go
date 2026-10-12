package traectl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"trae2api/internal/vscdb"
)

// Model 是 Trae 服务端下发的模型条目（来自客户端 state.vscdb）。
//
// 字段全部来自实测的真实数据（见 Trae2API 阶段一验证报告 §1.5）：
// 服务端会把每个模型的 config_name / 显示名 / 多模态能力 / 上下文窗口 /
// 原生 function calling 开关下发到客户端。
type Model struct {
	// ID 是 config_name —— 上游 chat 请求的 model 字段必须用它（不是显示名）。
	ID string
	// Display 是 UI 显示名（可能与 ID 不同，如 DeepSeek-V4-Flash-Official → "DeepSeek-V4-Flash 正式版"）。
	Display string
	// Multimodal 是否支持多模态（图片输入）。
	//
	// 取"顶层 multimodal **或** 嵌套 multimodal.enable"：
	// 两者并存，且部分模型只填了其中一个 —— 只看一个会漏。
	// （教训：曾一度新增一个 MultimodalEnable 字段而忘了给这个赋值，
	//  结果所有模型都变成非多模态。语义重复的字段就该合并。）
	Multimodal bool
	// NativeFC 是否支持原生 function calling（实测 chat_v3 与 solo_work_lite 的
	// 官方模型全部为 true，故代理可做真正的 tools 映射而非提示词模拟）。
	NativeFC bool
	// ParallelFC 是否支持并行工具调用。实测**全部为 false** ——
	// 收到并行 tool_calls 时必须串行化。
	ParallelFC bool
	// ContextWindow 默认上下文窗口（token）。0 表示服务端未下发。
	ContextWindow int
	// PromptMaxTokens 提示词上限（token）。
	PromptMaxTokens int
	// IsBeta 是否 beta 模型。
	IsBeta bool
	// Function 该模型所属的 function 池（chat_v3 / solo_work_lite / ...）。
	Function string

	// ---- 以下字段来自 state.vscdb 里更完整的模型定义 ----
	// （2026-10 补齐：原实现只解析了少数字段，漏掉了倍率、图标、
	//  嵌套的多模态/推理开关等 —— 那些才是控制台要展示的东西。）

	// FeeModelLevel 是**积分倍率档位**（fee_model_level）。
	// 官方模型实测全为 2，自建渠道（custom_openai_compatible//…）为 0（未下发）。
	// 0 表示"上游未下发"，展示时应显示为"—"而不是"0 倍"。
	FeeModelLevel int
	// ModelType 模型类型：chat_model / reasoning_model。
	ModelType string
	// Reasoning 是否支持推理（思维链）。
	//
	// 判定依据是 reasoning_effort_config.support_thinking，**不是** reasoning.enable：
	// 实测 chat_v3 池里 reasoning 字段**全部缺失**（我一开始就被它误导，
	// 以为所有模型都不支持推理），而 reasoning_effort_config 有明确值。
	// reasoning.enable 保留作兜底（别的池里会出现）。
	Reasoning bool
	// ReasoningEfforts 支持的推理档位（如 light / high / extra_high）。
	// 直接对应 OpenAI 的 reasoning_effort 参数。
	ReasoningEfforts []string
	// ReasoningDefault 默认推理档位。
	ReasoningDefault string
	// MaxTokens 单次输出上限（max_tokens）。
	MaxTokens int
	// MaxTurn 最大对话轮数（max_turn）。
	MaxTurn int
	// DevContext / MaxContext 上下文窗口（来自嵌套 context_windows.data）。
	// 比顶层 context_window_size 更精确（后者实测多为 null）。
	DevContext int
	MaxContext int
	// IsDefault / IsNew 是否默认模型 / 新模型（UI 标记）。
	IsDefault bool
	IsNew     bool
	// IconURL 模型图标（深色主题那一份）。
	IconURL string
	// Tags 模型标签。
	Tags []string
	// MaxMode 是否支持 Max 模式（更强的推理档，消耗更多积分）。
	MaxMode bool
}

// MultiplierDisplay 返回倍率的人类可读形式。
//
// 为什么返回字符串而不是数字：**"未下发"和"0 倍"是两件事**。
// 自建渠道模型没有 fee_model_level，把它显示成"0 倍"会让人以为免费。
// 所以未下发时返回 "—"，由调用方原样展示。
func (m Model) MultiplierDisplay() string {
	if m.FeeModelLevel <= 0 {
		return "—"
	}
	return fmt.Sprintf("%dx", m.FeeModelLevel)
}

// Catalog 是某产品在某个 function 池下的模型目录。
type Catalog struct {
	// Product 所属产品。
	Product Product
	// Function 本目录对应的 function 池。
	Function string
	// Models 过滤后的可用模型（已排除用户自建渠道与内部任务模型）。
	Models []Model
	// AllPools 记录该产品在 state.vscdb 中出现的所有 function 池及其模型数，
	// 仅用于诊断（便于发现服务端新增了池子）。
	AllPools map[string]int
	// SourcePath state.vscdb 路径（诊断用）。
	SourcePath string
}

// ByID 按 config_name 查找模型。
func (c *Catalog) ByID(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// IDs 返回全部模型 ID（稳定顺序）。
func (c *Catalog) IDs() []string {
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.ID)
	}
	return out
}

// 用户自建渠道前缀：这些不是 Trae 官方模型，**绝不能暴露给网关**，
// 否则网关会变成开放代理（把用户自己的 OpenAI 兼容端点转发出去）。
var customModelPrefixes = []string{
	"zai-plan//",
	"custom_openai_compatible//",
	"custom_",
}

// isCustomModel 判定是否为用户自建渠道模型。
func isCustomModel(name string) bool {
	for _, p := range customModelPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	// 形如 "provider//model" 的命名空间写法一律视为自建
	if strings.Contains(name, "//") {
		return true
	}
	return false
}

// rawModelListMap 是 state.vscdb 里 model_list_map 的原始结构：
// {function: [ {name, display_name, multimodal, custom_config, ...} ]}
type rawModelListMap map[string][]rawModel

// multimodalField 兼容 `multimodal` 的两种形态：
//
//	"multimodal": true                      （旧形态，顶层布尔）
//	"multimodal": {"enable": true}          （新形态，嵌套对象）
//
// ⚠️ 为什么必须自定义类型：**同一层里不能有两个 json tag 都叫 multimodal 的字段**。
// Go 的 encoding/json 遇到同层同名会判定为冲突并**同时丢弃两者** ——
// 不报错、不警告，表现为该字段恒为 false。这个坑极难从现象反推
// （"所有模型都突然不支持多模态了"），必须靠类型层面避免。
type multimodalField bool

func (m *multimodalField) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj struct {
			Enable bool `json:"enable"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		*m = multimodalField(obj.Enable)
		return nil
	}
	// null / 缺失 → 保持 false
	if string(trimmed) == "null" {
		*m = false
		return nil
	}
	var v bool
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return err
	}
	*m = multimodalField(v)
	return nil
}

type rawModel struct {
	Name            string `json:"name"`
	DisplayName     string `json:"display_name"`
	CustomConfig    string `json:"custom_config"`
	PromptMaxTokens int    `json:"prompt_max_tokens"`
	IsBeta          *bool  `json:"is_beta"`
	ContextWindow   struct {
		Max     []int `json:"max"`
		Default int   `json:"default"`
	} `json:"context_window_size"`

	// ---- 2026-10 补齐：这些字段服务端一直在下发，只是原来没解析 ----

	// FeeModelLevel 积分倍率档位。官方模型为 2，自建渠道缺省（0）。
	FeeModelLevel int `json:"fee_model_level"`
	// ModelType chat_model / reasoning_model。
	ModelType string `json:"model_type"`
	// MaxTokens 单次输出上限。
	MaxTokens int `json:"max_tokens"`
	// MaxTurn 最大对话轮数。
	MaxTurn int `json:"max_turn"`
	// IsDefault / IsNew / MaxMode UI 标记。
	IsDefault bool `json:"is_default"`
	IsNew     bool `json:"is_new"`
	MaxMode   bool `json:"max_mode"`
	// Tags 模型标签。
	Tags []string `json:"tags"`
	// Icon 图标（取深色主题那一份）。
	Icon struct {
		Dark  string `json:"dark"`
		Light string `json:"light"`
	} `json:"icon"`
	// Multimodal 多模态开关（兼容布尔与对象两种形态，见 multimodalField）。
	Multimodal multimodalField `json:"multimodal"`
	// ReasoningCfg 嵌套的推理开关（实测 chat_v3 里该字段全部缺失，
	// 真正的信号在 ReasoningEffortCfg，这里保留作兜底）。
	ReasoningCfg struct {
		Enable bool `json:"enable"`
	} `json:"reasoning"`
	// ReasoningEffortCfg 推理档位配置 —— 这才是 chat_v3 里真实存在的字段。
	ReasoningEffortCfg struct {
		SupportThinking bool     `json:"support_thinking"`
		Options         []string `json:"options"`
		DefaultLevel    string   `json:"default_level"`
	} `json:"reasoning_effort_config"`
	// ContextWindows 嵌套的上下文窗口（比顶层 context_window_size 精确，
	// 后者实测多为 null）。
	ContextWindows struct {
		Data struct {
			DevContext int `json:"dev_context"`
			MaxContext int `json:"max_context"`
			DevTurns   int `json:"dev_turns"`
			MaxTurns   int `json:"max_turns"`
		} `json:"data"`
	} `json:"context_windows"`
}

// customConfig 是 custom_config 字段（它本身是一个 JSON 字符串）的结构子集。
type customConfig struct {
	NativeFunctionCall  bool `json:"native_function_call"`
	ParallelToolCalling bool `json:"parallel_tool_calling"`
}

// LoadCatalog 从本机 state.vscdb 读取该产品的模型目录。
//
// 数据来源是**服务端下发的真实清单**（客户端把原生服务返回的结果缓存到
// state.vscdb 的 ItemTable），比任何硬编码表都准确 —— 模型与能力随服务端
// 灰度变化，硬编码必然会过期。
//
// dbPath 为空时用 DefaultStateDBPath。
func LoadCatalog(p Product, dbPath string) (*Catalog, error) {
	if dbPath == "" {
		var err error
		dbPath, err = DefaultStateDBPath(p)
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("traectl: 找不到 %s 的 state.vscdb（%s）: %w", p.Label, dbPath, err)
	}

	// userId 用于拼 key
	st, err := OpenStore(p, "")
	if err != nil {
		return nil, fmt.Errorf("traectl: 读取 userId 失败（模型目录 key 依赖它）: %w", err)
	}
	uid := st.Cred.UserID

	db, err := vscdb.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("traectl: 打开 %s: %w", dbPath, err)
	}
	defer db.Close()

	raw, err := readModelMap(db, p, uid)
	if err != nil {
		return nil, err
	}

	cat := &Catalog{
		Product:    p,
		Function:   p.Function,
		AllPools:   map[string]int{},
		SourcePath: dbPath,
	}
	for fn, list := range raw {
		cat.AllPools[fn] = len(list)
	}

	list, ok := raw[p.Function]
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("traectl: %s 的 state.vscdb 中没有 function=%q 的模型池（现有池：%s）",
			p.Label, p.Function, strings.Join(sortedPoolNames(cat.AllPools), ", "))
	}

	seen := map[string]bool{}
	for _, rm := range list {
		name := strings.TrimSpace(rm.Name)
		if name == "" || isCustomModel(name) || seen[name] {
			continue
		}
		seen[name] = true

		var cc customConfig
		if rm.CustomConfig != "" {
			_ = json.Unmarshal([]byte(rm.CustomConfig), &cc)
		}
		m := Model{
			ID:              name,
			Display:         strings.TrimSpace(rm.DisplayName),
			NativeFC:        cc.NativeFunctionCall,
			ParallelFC:      cc.ParallelToolCalling,
			ContextWindow:   rm.ContextWindow.Default,
			PromptMaxTokens: rm.PromptMaxTokens,
			Function:        p.Function,

			// 补齐的字段
			FeeModelLevel: rm.FeeModelLevel,
			ModelType:     rm.ModelType,
			MaxTokens:     rm.MaxTokens,
			MaxTurn:       rm.MaxTurn,
			IsDefault:     rm.IsDefault,
			IsNew:         rm.IsNew,
			MaxMode:       rm.MaxMode,
			Tags:          rm.Tags,
			IconURL:       firstNonEmpty(rm.Icon.Dark, rm.Icon.Light),
			// 多模态取"顶层 or 嵌套"：两者并存，部分模型只填了一个
			Multimodal:       bool(rm.Multimodal),
			Reasoning:        rm.ReasoningEffortCfg.SupportThinking || rm.ReasoningCfg.Enable,
			ReasoningEfforts: rm.ReasoningEffortCfg.Options,
			ReasoningDefault: rm.ReasoningEffortCfg.DefaultLevel,
			DevContext:       rm.ContextWindows.Data.DevContext,
			MaxContext:       rm.ContextWindows.Data.MaxContext,
		}
		// 上下文窗口：顶层 context_window_size 实测多为 null，
		// 此时用嵌套的 dev_context 兜底（它才是客户端实际用的那个）。
		if m.ContextWindow == 0 {
			m.ContextWindow = firstNonZero(rm.ContextWindows.Data.DevContext, rm.ContextWindows.Data.MaxContext)
		}
		if m.MaxTurn == 0 {
			m.MaxTurn = rm.ContextWindows.Data.MaxTurns
		}
		if rm.IsBeta != nil {
			m.IsBeta = *rm.IsBeta
		}
		if m.Display == "" {
			m.Display = name
		}
		cat.Models = append(cat.Models, m)
	}
	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("traectl: %s 的 function=%q 池过滤后无可用模型", p.Label, p.Function)
	}
	sort.Slice(cat.Models, func(i, j int) bool { return cat.Models[i].ID < cat.Models[j].ID })
	return cat, nil
}

// firstNonEmpty 返回第一个非空字符串（用于图标取深色优先、回退浅色）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstNonZero 返回第一个非零整数。
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// readModelMap 依次尝试两种 key 形式，返回解析后的原始结构。
func readModelMap(db *vscdb.DB, p Product, uid string) (rawModelListMap, error) {
	if uid == "" {
		return nil, errors.New("traectl: userId 为空，无法构造模型目录 key")
	}
	var lastErr error
	for _, key := range p.ModelMapKeys(uid) {
		val, err := db.GetItem(key)
		if err != nil {
			lastErr = err
			continue
		}
		if len(val) == 0 {
			continue
		}
		var raw rawModelListMap
		if err := json.Unmarshal(val, &raw); err != nil {
			lastErr = fmt.Errorf("解析 %s 失败: %w", key, err)
			continue
		}
		if len(raw) == 0 {
			continue
		}
		return raw, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("traectl: 未能在 state.vscdb 中读到 %s 的模型目录: %w", p.Label, lastErr)
	}
	return nil, fmt.Errorf("traectl: 未能在 state.vscdb 中读到 %s 的模型目录（key 前缀 %q，客户端可能未登录或未拉取过模型列表）",
		p.Label, uid)
}

func sortedPoolNames(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
