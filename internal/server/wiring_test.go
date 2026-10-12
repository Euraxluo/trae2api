package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api/internal/config"
	"trae2api/internal/traectl"
)

// TestNewTraeClientWiring 客户端的安全字段必须都被真正赋值。
//
// ## 为什么需要这个测试
//
// `traectl.Client` 里有一批"配错就**静默失效**"的字段。最典型的是 `IdleTimeout`：
// 设成 0 时流中空闲监控会**直接不启用**，没有任何报错、没有任何日志，
// 表现只是"偶尔有请求永远挂住" —— 而那个"偶尔"可能几天才出现一次。
//
// 实际就踩过：我给 Client 加了 `IdleTimeout` 字段、实现了监控逻辑、
// 还写了单元测试，但**构造 Client 时忘了赋值** —— 于是整套监控是死代码，
// 而所有测试都是绿的（因为它们直接构造 Client，不走真实构造路径）。
//
// 这个测试走**真实构造路径**，所以能拦住这类疏漏。
func TestNewTraeClientWiring(t *testing.T) {
	cfg := config.Default()
	c := newTraeClient(cfg, traectl.ProductByKeyOrZero("trae-cn"))
	if c == nil {
		t.Fatal("newTraeClient 返回 nil")
	}

	// IdleTimeout 是本次事故的主角：默认配置下必须 > 0
	if cfg.Trae.IdleTimeoutSeconds > 0 && c.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout 未接线（配置 %d 秒，客户端 %v）—— 流中空闲监控会是死代码",
			cfg.Trae.IdleTimeoutSeconds, c.IdleTimeout)
	}
	if want := time.Duration(cfg.Trae.IdleTimeoutSeconds) * time.Second; c.IdleTimeout != want {
		t.Errorf("IdleTimeout = %v want %v", c.IdleTimeout, want)
	}

	// 显式负值 = 禁用
	neg := config.Default()
	neg.Trae.IdleTimeoutSeconds = -1
	if got := newTraeClient(neg, traectl.ProductByKeyOrZero("trae-cn")).IdleTimeout; got != 0 {
		t.Errorf("负值应表示禁用（0），得到 %v", got)
	}

	// 这些字段即使为空也是"有意义的值"（空 = 用内置默认），
	// 所以只断言构造函数确实把配置透传了，而不是断言非空。
	cfg2 := config.Default()
	cfg2.Trae.ChatBase = "https://example.invalid"
	cfg2.Trae.UserAgent = "UA-TEST"
	cfg2.Trae.IDEVersion = "9.9.9"
	cfg2.Trae.IDEVersionCode = "999"
	c2 := newTraeClient(cfg2, traectl.ProductByKeyOrZero("trae-cn"))
	if c2.Base != cfg2.Trae.ChatBase {
		t.Errorf("Base 未透传: %q", c2.Base)
	}
	if c2.UserAgent != cfg2.Trae.UserAgent {
		t.Errorf("UserAgent 未透传: %q", c2.UserAgent)
	}
	if c2.IDEVersion != cfg2.Trae.IDEVersion || c2.IDEVersionCode != cfg2.Trae.IDEVersionCode {
		t.Errorf("版本字段未透传: %q / %q", c2.IDEVersion, c2.IDEVersionCode)
	}
}

// TestNoStrayClientConstruction 不允许在 newTraeClient 之外直接构造 traectl.Client。
//
// 这是防止"接线分叉"的结构性约束：有两个构造点时，给 Client 加字段后
// 只改其中一个的意图、另一个保持默认 —— 这类 bug 单靠人眼审查很难发现。
//
// 用源码扫描而不是运行期断言：运行期只能覆盖到"这次跑到的路径"，
// 而漏掉的那个构造点往往在冷路径上（如运行时热导入账号）。
func TestNoStrayClientConstruction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := string(raw)
		// 只允许 channel.go（newTraeClient 所在文件）出现这个字面量
		if f == "channel.go" {
			continue
		}
		if strings.Contains(src, "traectl.Client{") {
			offenders = append(offenders, f)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("这些文件直接构造了 traectl.Client，应改用 newTraeClient："+
			"%v\n（原因：多个构造点会让新字段只在部分路径上生效）", offenders)
	}
}

// TestChatBaseByEdition 是**国际版支持的核心不变量**：chat base 必须按版本域选。
//
// ## 为什么这条最重要
//
// 国内版与国际版是**两套独立的上游主机**。若两者共用同一个 chat base，
// 国际账号的请求会打到国内上游（或反之）—— 表现为"账号明明健康却总是失败"，
// 而错误信息完全指不出根因。
//
// 这正是 `newTraeClient` 从"只接 cfg"改成"接 cfg + 产品"的原因：
// 主机必须由**产品**决定，不能全局一个值。
func TestChatBaseByEdition(t *testing.T) {
	cfg := config.Default()
	cfg.Trae.ChatBase = "https://cn.example.invalid"
	cfg.Intl.ChatHost = "https://intl.example.invalid"

	cn := traectl.ProductByKeyOrZero("trae-cn")
	if cn.Edition != traectl.EditionCN {
		t.Fatalf("trae-cn 的版本域 = %q want cn", cn.Edition)
	}
	if got := newTraeClient(cfg, cn).Base; got != cfg.Trae.ChatBase {
		t.Errorf("国内版应使用 trae.chat_base，得到 %q", got)
	}

	intl := traectl.Product{Key: "trae-intl", Edition: traectl.EditionIntl}
	if got := newTraeClient(cfg, intl).Base; got != cfg.Intl.ChatHost {
		t.Errorf("国际版应使用 intl.chat_host，得到 %q（会把国际请求打到国内上游）", got)
	}
}

// TestIsIntlHost 版本域判定（对齐参考实现"按域后缀判 realm"的做法）。
func TestIsIntlHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"https://api.trae.ai", true},
		{"https://trae-api.trae.ai", true},
		{"api.trae.ai", true},
		{"https://api.trae.ai:443", true},
		{"https://api.trae.ai/some/path", true},
		{"TRAE.AI", true},
		{"https://api.trae.cn", false},
		{"https://api.trae.com.cn", false},
		{"https://trae-api-cn.mchost.guru", false},
		{"", false},
		{"https://not-trae.ai", false}, // 不能把任意 .ai 域当国际版
		{"https://trae.ai.evil.com", false},
	}
	for _, c := range cases {
		if got := traectl.IsIntlHost(c.host); got != c.want {
			t.Errorf("IsIntlHost(%q) = %v want %v", c.host, got, c.want)
		}
	}
}

// TestConfigureIntlRequiresAllFields 启用国际版但缺配置时必须**明确报错**。
//
// 为什么不给默认值：国际版端点无法从国内客户端推导（实测 api.trae.ai 对
// Trae 的 API 路径返回 nginx 404）。猜错的表现是"账号健康却总是失败"，
// 报错还指不出根因 —— 宁可要求显式配置，也不要一个看起来能用、实际全错的默认值。
func TestConfigureIntlRequiresAllFields(t *testing.T) {
	// 不启用 → 不该注册任何国际产品，也不该报错
	if err := traectl.ConfigureIntl(traectl.IntlConfig{Enabled: false}); err != nil {
		t.Fatalf("未启用时不该报错: %v", err)
	}
	for _, p := range traectl.Products {
		if p.Edition == traectl.EditionIntl {
			t.Errorf("未启用时不该注册国际产品: %s", p.Key)
		}
	}

	// 启用但缺字段 → 报错，且错误信息要**指出从哪取值**
	err := traectl.ConfigureIntl(traectl.IntlConfig{Enabled: true})
	if err == nil {
		t.Fatal("启用但缺配置时应报错")
	}
	for _, want := range []string{"ug_host", "chat_host", "product.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q: %v", want, err)
		}
	}

	// 配全 → 注册两个国际产品
	err = traectl.ConfigureIntl(traectl.IntlConfig{
		Enabled: true, UGHost: "https://api.trae.ai",
		ChatHost: "https://chat.trae.ai", TraeClientID: "cid-t", SoloClientID: "cid-s",
	})
	if err != nil {
		t.Fatalf("配全后应成功: %v", err)
	}
	var found []string
	for _, p := range traectl.Products {
		if p.Edition == traectl.EditionIntl {
			found = append(found, p.Key)
		}
	}
	if len(found) != 2 {
		t.Errorf("应注册 2 个国际产品，实际 %v", found)
	}
	// 幂等：重复调用不该重复注册
	_ = traectl.ConfigureIntl(traectl.IntlConfig{
		Enabled: true, UGHost: "https://api.trae.ai",
		ChatHost: "https://chat.trae.ai", TraeClientID: "cid-t", SoloClientID: "cid-s",
	})
	n := 0
	for _, p := range traectl.Products {
		if p.Edition == traectl.EditionIntl {
			n++
		}
	}
	if n != 2 {
		t.Errorf("重复配置后国际产品数 = %d want 2（应幂等）", n)
	}
	// 收尾：恢复到未启用，避免影响同包其他测试
	_ = traectl.ConfigureIntl(traectl.IntlConfig{Enabled: false})
}

// TestRealmOfProductIntl 国际产品必须映射到独立 realm。
//
// 混在同一 realm 里会让选号把国内账号派给国际请求（或反之）。
func TestRealmOfProductIntl(t *testing.T) {
	cases := map[string]string{
		"trae-cn":   "trae",
		"solo-cn":   "solo",
		"trae-intl": "trae-intl",
		"solo-intl": "solo-intl",
		"unknown":   "",
	}
	for key, want := range cases {
		if got := RealmOfProduct(key); got != want {
			t.Errorf("RealmOfProduct(%q) = %q want %q", key, got, want)
		}
	}
}
