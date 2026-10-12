package traectl

import "testing"

// TestCorrectEditionByHost 按凭据自带的 host 校正版本域。
//
// ## 为什么这条最重要
//
// 用户导入"国际版客户端的 storage.json"时，如果只按调用方传的 product 走，
// 会把它登记成**国内账号** —— 于是后续所有请求都打到国内上游，
// 表现为"账号明明健康却总是失败"，而报错完全指不出根因。
//
// 所以以**凭据自己说的**为准（对齐参考实现"按域后缀判 realm"的做法）。
func TestCorrectEditionByHost(t *testing.T) {
	// 注册国际版产品（供校正查找）
	if err := ConfigureIntl(IntlConfig{
		Enabled: true, UGHost: "https://api.trae.ai",
		ChatHost: "https://chat.trae.ai", TraeClientID: "cid-t", SoloClientID: "cid-s",
	}); err != nil {
		t.Fatalf("注册国际版失败: %v", err)
	}
	t.Cleanup(func() { _ = ConfigureIntl(IntlConfig{Enabled: false}) })

	cnIDE := ProductByKeyOrZero("trae-cn")
	cnSolo := ProductByKeyOrZero("solo-cn")
	intlIDE := ProductByKeyOrZero("trae-intl")

	cases := []struct {
		name      string
		host      string
		requested Product
		wantKey   string
		wantFix   bool
	}{
		{"国际凭据被当成国内导入（IDE）", "https://api.trae.ai", cnIDE, "trae-intl", true},
		{"国际凭据被当成国内导入（SOLO）", "https://api.trae.ai", cnSolo, "solo-intl", true},
		{"国内凭据本来就对（IDE）", "https://api.trae.cn", cnIDE, "trae-cn", false},
		{"国内凭据本来就对（SOLO）", "https://api.trae.cn", cnSolo, "solo-cn", false},
		{"host 为空 → 尊重调用方", "", cnIDE, "trae-cn", false},
		{"trae.com.cn 也是国内", "https://api.trae.com.cn", cnIDE, "trae-cn", false},
		{"mchost.guru 是国内 chat 域", "https://trae-api-cn.mchost.guru", cnIDE, "trae-cn", false},
		{"已是国际且请求也是国际 → 不校正", "https://api.trae.ai", intlIDE, "trae-intl", false},
	}
	for _, c := range cases {
		snap := &CredentialSnapshot{Host: c.host}
		got, fixed := correctEditionByHost(snap, c.requested)
		if got.Key != c.wantKey {
			t.Errorf("%s: 产品 = %q want %q", c.name, got.Key, c.wantKey)
		}
		if fixed != c.wantFix {
			t.Errorf("%s: 是否校正 = %v want %v", c.name, fixed, c.wantFix)
		}
	}
}

// TestCorrectEditionNoMatchingProduct 国际版未启用时不该强行校正
// （找不到对应产品就保持原样，让导入仍能成功）。
func TestCorrectEditionNoMatchingProduct(t *testing.T) {
	_ = ConfigureIntl(IntlConfig{Enabled: false})

	cnIDE := ProductByKeyOrZero("trae-cn")
	snap := &CredentialSnapshot{Host: "https://api.trae.ai"}
	got, fixed := correctEditionByHost(snap, cnIDE)
	if fixed {
		t.Error("找不到对应的国际产品时不该校正")
	}
	if got.Key != cnIDE.Key {
		t.Errorf("应保持原产品，得到 %q", got.Key)
	}
}

// TestConfigureIntlProductsDistinct 两个国际产品必须有不同的 Key / ClientID /
// PlatformCode —— 它们是两套独立的授权。
func TestConfigureIntlProductsDistinct(t *testing.T) {
	if err := ConfigureIntl(IntlConfig{
		Enabled: true, UGHost: "https://api.trae.ai", ChatHost: "https://chat.trae.ai",
		TraeClientID: "cid-ide", SoloClientID: "cid-solo",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureIntl(IntlConfig{Enabled: false}) })

	ide := ProductByKeyOrZero("trae-intl")
	solo := ProductByKeyOrZero("solo-intl")
	if ide.Key == solo.Key {
		t.Error("Key 不该相同")
	}
	if ide.ClientID == solo.ClientID {
		t.Error("ClientID 不该相同（两套独立授权）")
	}
	if ide.PlatformCode == solo.PlatformCode {
		t.Error("PlatformCode 不该相同")
	}
	if ide.AuthFrom == solo.AuthFrom {
		t.Error("AuthFrom 不该相同")
	}
	if !ide.IsSOLO == false || !solo.IsSOLO {
		t.Error("IsSOLO 标记不对")
	}
	// 都必须标成国际域
	if ide.Edition != EditionIntl || solo.Edition != EditionIntl {
		t.Error("版本域应为 intl")
	}
}
