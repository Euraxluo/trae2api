package server

import (
	"testing"

	"trae2api/internal/pool"
)

// TestDeriveLoginHost 从 API 主机派生登录页主机。
//
// 依据国内版的实测对应关系：api.trae.cn（API）↔ www.trae.cn（登录页）。
// 派生而不是加配置项：少一个要填的值，且规则在 CN 上可验证。
func TestDeriveLoginHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.trae.cn", "https://www.trae.cn"},
		{"https://api.trae.ai", "https://www.trae.ai"},
		{"https://api.trae.cn/", "https://www.trae.cn"},
		{"api.trae.ai", "https://www.trae.ai"},
		{"http://api.example.com", "http://www.example.com"},
		{"https://custom.example.com", "https://www.custom.example.com"},
		{"https://www.example.com", "https://www.example.com"}, // 已是 www 则不动
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := deriveLoginHost(c.in); got != c.want {
			t.Errorf("deriveLoginHost(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// TestOtherRealmChannel 跨版本域兜底的配对关系。
//
// 配对：trae ↔ trae-intl、solo ↔ solo-intl（同 IsSOLO 的两个版本域）。
//
// ⚠️ 绝不能"随便挑一个"兜底：trae 与 solo 的模型清单与 function 都不同，
// 把 trae 的请求落到 solo 的池上会得到完全不同的模型（或直接失败）。
func TestOtherRealmChannel(t *testing.T) {
	mk := func(realms ...string) *Server {
		s := &Server{channels: map[string]*Channel{}}
		for _, r := range realms {
			s.channels[r] = &Channel{Realm: r, accounts: map[string]*pool.Account{}}
		}
		return s
	}

	// 只有 CN：请求国际应兜底到 CN
	s := mk("trae", "solo")
	if alt, ok := s.otherRealmChannel("trae-intl"); !ok || alt.Realm != "trae" {
		t.Errorf("trae-intl 应兜底到 trae，得到 %v %v", alt, ok)
	}
	if alt, ok := s.otherRealmChannel("solo-intl"); !ok || alt.Realm != "solo" {
		t.Errorf("solo-intl 应兜底到 solo，得到 %v %v", alt, ok)
	}
	// 反向：只有国际，请求 CN
	s2 := mk("trae-intl", "solo-intl")
	if alt, ok := s2.otherRealmChannel("trae"); !ok || alt.Realm != "trae-intl" {
		t.Errorf("trae 应兜底到 trae-intl，得到 %v %v", alt, ok)
	}

	// ⚠️ 关键：绝不能跨 trae↔solo 兜底
	s3 := mk("solo") // 只有 solo，请求 trae
	if alt, ok := s3.otherRealmChannel("trae"); ok {
		t.Errorf("trae 不该兜底到 solo（模型池与 function 都不同），得到 %v", alt.Realm)
	}
	if _, ok := s3.otherRealmChannel("trae-intl"); ok {
		t.Error("trae-intl 不该兜底到 solo")
	}

	// 配对域也不存在 → 不兜底
	s4 := mk()
	if _, ok := s4.otherRealmChannel("trae"); ok {
		t.Error("没有任何渠道时不该兜底")
	}
	// 未知域 → 不兜底
	if _, ok := s4.otherRealmChannel("bogus"); ok {
		t.Error("未知域不该兜底")
	}
}
