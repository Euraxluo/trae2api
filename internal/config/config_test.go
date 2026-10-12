package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDefault 默认配置应可直接使用（缺省即可跑）。
func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen == "" || c.DataDir == "" {
		t.Fatal("默认 Listen/DataDir 不应为空")
	}
	if c.Pool.MaxInFlight <= 0 {
		t.Error("默认 max_in_flight 应 > 0")
	}
	if c.SessionSticky.TTL == "" {
		t.Error("默认粘性 TTL 不应为空")
	}
	// 默认值必须能通过 normalize（否则"缺省即可跑"就是假的）
	if err := c.normalize(); err != nil {
		t.Fatalf("默认配置未通过校验: %v", err)
	}
	// 各时长字段可解析
	for name, f := range map[string]func() (time.Duration, error){
		"breaker_cooldown": c.BreakerCooldownDur,
		"soft_rate":        c.SoftRateDur,
		"soft_rate_max":    c.SoftRateMaxDur,
		"model_cooldown":   c.ModelCooldownDur,
		"pick_gap":         c.PickGapDur,
		"sticky_ttl":       c.StickyTTL,
	} {
		if _, err := f(); err != nil {
			t.Errorf("%s 默认值解析失败: %v", name, err)
		}
	}
}

// TestLoadMissingCreatesTemplate 首次运行应生成模板并返回默认值。
func TestLoadMissingCreatesTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != Default().Listen {
		t.Errorf("首次加载应返回默认值，得到 %q", cfg.Listen)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应生成配置模板: %v", err)
	}
	// 生成的模板必须能被再次加载
	if _, err := Load(path); err != nil {
		t.Fatalf("模板无法回读: %v", err)
	}
}

// TestLoadPartialMerge 旧配置缺新字段时应自动获得默认值（前向兼容）。
func TestLoadPartialMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 只写 listen 与 trae.accounts，其余全缺
	body := `{"listen":"127.0.0.1:9999","trae":{"accounts":[{"product":"trae-cn","label":"主号"}]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("显式值应生效，得到 %q", cfg.Listen)
	}
	if cfg.Pool.MaxInFlight != Default().Pool.MaxInFlight {
		t.Errorf("缺失字段应回落默认，得到 %d", cfg.Pool.MaxInFlight)
	}
	if cfg.DataDir != Default().DataDir {
		t.Errorf("缺失 DataDir 应回落默认，得到 %q", cfg.DataDir)
	}
	if len(cfg.Trae.Accounts) != 1 {
		t.Fatalf("账号数 = %d want 1", len(cfg.Trae.Accounts))
	}
	if cfg.Trae.Accounts[0].Owner != "public" {
		t.Errorf("未指定 owner 应回落 public，得到 %q", cfg.Trae.Accounts[0].Owner)
	}
}

// TestNormalizeRejectsBadValues 非法值必须报错而不是静默忽略
// （静默忽略会让用户以为配置生效了）。
func TestNormalizeRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		body string
	}{
		{"非法时长", func(c *Config) { c.Cooldown.SoftRate = "abc" }, ""},
		{"负时长", func(c *Config) { c.Cooldown.SoftRate = "-5s" }, ""},
		{"非法 breaker_cooldown", func(c *Config) { c.Pool.BreakerCooldown = "xx" }, ""},
		{"账号缺 product", nil, `{"trae":{"accounts":[{"label":"x"}]}}`},
		{"账号 product 非法", nil, `{"trae":{"accounts":[{"product":"nope"}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Load(path); err == nil {
					t.Fatal("应报错，却成功了")
				}
				return
			}
			c := Default()
			tc.mut(c)
			if err := c.normalize(); err == nil {
				t.Fatal("应报错，却成功了")
			}
		})
	}
}

// TestNormalizeFillsDefaults 零值字段应被补齐。
func TestNormalizeFillsDefaults(t *testing.T) {
	c := &Config{} // 全零
	if err := c.normalize(); err != nil {
		t.Fatalf("全零配置应能补齐默认值: %v", err)
	}
	if c.Listen == "" || c.DataDir == "" || c.Pool.MaxInFlight <= 0 || c.Pool.BreakerThreshold <= 0 {
		t.Errorf("零值未被补齐: %+v", c)
	}
}

// TestStateFile 状态文件路径应落在 DataDir 下。
func TestStateFile(t *testing.T) {
	c := Default()
	c.DataDir = "/tmp/x"
	if got := c.StateFile(); got != filepath.Join("/tmp/x", "state.json") {
		t.Errorf("StateFile = %q", got)
	}
}

// TestAccountProductValidation 两个合法产品都应通过。
func TestAccountProductValidation(t *testing.T) {
	for _, p := range []string{"trae-cn", "solo-cn"} {
		c := Default()
		c.Trae.Accounts = []Account{{Product: p}}
		if err := c.normalize(); err != nil {
			t.Errorf("product %q 应合法: %v", p, err)
		}
		if c.Trae.Accounts[0].Owner != "public" {
			t.Errorf("product %q 的 owner 应回落 public", p)
		}
	}
}

// TestLoadToleratesBOM 配置带 UTF-8 BOM 时必须能加载。
//
// Windows 上用记事本编辑、或 PowerShell 5.1 的 Set-Content -Encoding UTF8
// 都会写入 BOM。不容忍的话报错是 `invalid character '\ufeff'`，
// 用户完全联想不到是编辑器加的字节。
func TestLoadToleratesBOM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	// 用 json.Marshal 构造：Windows 路径里的反斜杠直接拼进 JSON 会变成非法转义
	body, _ := json.Marshal(map[string]string{"listen": "127.0.0.1:9999", "data_dir": dir})
	// 手动加 BOM
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, body...)
	if err := os.WriteFile(p, withBOM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("带 BOM 的配置应能加载: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}

	// 不带 BOM 的也要正常（回归）
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("不带 BOM 的配置应能加载: %v", err)
	}
}

// TestStripBOM 直接验证剥离逻辑。
func TestStripBOM(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\ufeff{}", "{}"},
		{"{}", "{}"},
		{"", ""},
		{"\ufeff", ""},
	}
	for _, c := range cases {
		if got := string(stripBOM([]byte(c.in))); got != c.want {
			t.Errorf("stripBOM(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// TestIntlDefaultsPresent 国际版的四个端点值必须有内置默认且格式正确。
//
// ## 这条测试守的是什么
//
// 这四个值**曾经是空的**，并要求用户自己去国际版客户端的 product.json 里抄。
// 当时的理由正当（够不到国际站，不能猜），但对用户是个硬门槛：
// 抄错一个字符的表现是"账号健康却总是失败"，报错还指不出根因。
//
// 后来从国际站 www.trae.ai 的 main.js 拿到了主机清单，并用
// "真 API 返回 JSON 错误信封 vs 非 API 返回 404 HTML" 的判据逐个验过，
// 所以现在能给默认值了。
//
// 这条测试的价值不在"值对不对"（那要联网才能验），而在于：
// **保证它们不会被人在重构中清空** —— 一旦清空，国际版功能会静默退化成
// "需要用户手填四个值"，而没有任何编译期或运行期报错。
func TestIntlDefaultsPresent(t *testing.T) {
	c := Default()
	cases := []struct{ name, got, wantScheme string }{
		{"intl.ug_host", c.Intl.UGHost, "https://"},
		{"intl.chat_host", c.Intl.ChatHost, "https://"},
		// 登录页主机**必须**是独立配置项：曾经用"把 api. 换成 www."的规则从 UG 主机
		// 派生，但那条规则只在国内版成立 —— 国际版的 ug-normal.trae.ai 会被派生成
		// www.ug-normal.trae.ai（不存在），表现为"登录页连不上"，且看起来像网络问题。
		{"intl.login_host", c.Intl.LoginHost, "https://"},
	}
	for _, tc := range cases {
		if tc.got == "" {
			t.Errorf("%s 被清空了 —— 国际版会静默退化成「需要用户手填」", tc.name)
			continue
		}
		if !strings.HasPrefix(tc.got, tc.wantScheme) {
			t.Errorf("%s = %q，应以 %s 开头", tc.name, tc.got, tc.wantScheme)
		}
	}
	if c.Intl.TraeClientID == "" || c.Intl.SoloClientID == "" {
		t.Error("国际版 ClientID 被清空了（实测与国内版共用同一对）")
	}
	if c.Intl.TraeClientID == c.Intl.SoloClientID {
		t.Error("Trae 与 SOLO 的 ClientID 不该相同 —— 它们是两套独立授权")
	}
	// 登录页主机不能是从 UG 主机机械派生出来的那个（那是个不存在的主机）
	if c.Intl.LoginHost == "https://www."+strings.TrimPrefix(c.Intl.UGHost, "https://") {
		t.Errorf("intl.login_host 看起来是从 ug_host 机械派生的（%s）—— "+
			"那条规则只在国内版成立，国际版会派生出不存在的主机", c.Intl.LoginHost)
	}
	if !strings.Contains(c.Intl.LoginHost, "trae.ai") {
		t.Errorf("intl.login_host = %q，看起来不像国际版站点", c.Intl.LoginHost)
	}
	// 默认**启用**：国内版与国际版是两套账号体系，用户可能只装了其中一个。
	// 默认全开才能"装了哪个就自动认哪个" —— 注册产品不等于建渠道，
	// 找不到凭据时只是记一条 INFO（「未检测到客户端」）跳过，所以开着没有代价。
	if !c.Intl.Enabled {
		t.Error("intl.enabled 默认应为 true（否则境外用户要手工开启，而开着的代价只是多尝两次目录）")
	}
}
