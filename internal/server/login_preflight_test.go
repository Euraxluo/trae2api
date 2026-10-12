package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLoginPreflightRegionBlock 区域封锁必须被**识别**，且错误信息要能照做。
//
// 这是本文件最重要的一条：这个失败模式的危害不在于"登录不了"，
// 而在于**表现极具误导性** ——
//   - 网关读凭据、续期、调模型全都正常（那些走 api.trae.cn，不受区域封锁影响）；
//   - 登录 URL 本身逐参数正确；
//   - 浏览器只说"无法访问此页面"；
//   - 网关等到 10 分钟超时才报一句"登录超时"。
//
// 四者叠加，用户几乎不可能自己想到"是我的代理区域被封锁了"。
func TestLoginPreflightRegionBlock(t *testing.T) {
	// 复刻实测拿到的真实响应：volc-dcdn + 403 + 中文区域提示
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "volc-dcdn")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("抱歉，您当前区域不支持访问"))
	}))
	defer srv.Close()

	err := loginHostProblem(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("区域封锁没被识别出来 —— 用户会看到浏览器报错而网关毫无线索")
	}
	msg := err.Error()

	// 错误信息必须包含：现象、原因、**怎么修**
	for _, want := range []string{
		"区域",          // 现象
		"DIRECT",      // 怎么修（代理规则）
		"trae.cn",     // 改哪个域名
		"api.trae.cn", // 解释为什么其它功能正常 —— 帮用户排除误判
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q，实际：%s", want, msg)
		}
	}
}

// TestLoginPreflightRegionBlockEnglish 英文区域封锁页也要认。
func TestLoginPreflightRegionBlockEnglish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<h1>Sorry, this service is not available in your region.</h1>"))
	}))
	defer srv.Close()

	if err := loginHostProblem(context.Background(), srv.URL); err == nil {
		t.Error("英文区域封锁页没被识别")
	}
}

// TestLoginPreflightUnreachable 连不上要报"连不上"，而不是静默放行。
func TestLoginPreflightUnreachable(t *testing.T) {
	// 127.0.0.1:1 上不会有监听 → 连接被拒
	err := loginHostProblem(context.Background(), "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("连不上应当报错（浏览器同样连不上，放行等于让用户白等）")
	}
	if !strings.Contains(err.Error(), "连不上") {
		t.Errorf("错误信息应说明是连不上：%s", err)
	}
}

// TestLoginPreflightOtherStatusPasses 其它状态码**不拦**。
//
// 为什么：我们只对**确认过**的失败模式负责。一个没见过的状态码可能是
// WAF 的正常拦截、维护页、或我们探测姿势不对 —— 拿它阻止用户登录是过度自信。
// 误报比漏报更糟：用户会去改一个本来没问题的代理配置。
func TestLoginPreflightOtherStatusPasses(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("<html>normal page</html>"))
		}))
		err := loginHostProblem(context.Background(), srv.URL)
		srv.Close()
		if err != nil {
			t.Errorf("HTTP %d 不该被拦（误报会误导用户改配置），却报错：%v", code, err)
		}
	}
}

// TestLoginPreflight403WithoutRegionMarkerPasses 403 但**不是**区域封锁时不拦。
//
// 这条是防"宽泛匹配"的：不能一看到 403 就说是区域封锁 ——
// 那会把权限类、WAF 类问题全都归因到区域上。
func TestLoginPreflight403WithoutRegionMarkerPasses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Access denied by security policy"))
	}))
	defer srv.Close()

	if err := loginHostProblem(context.Background(), srv.URL); err != nil {
		t.Errorf("403 但不含区域特征时不该判定为区域封锁，却报错：%v", err)
	}
}

// TestLoginPreflightEmptyHost 空主机不报错（交给下游用默认值）。
func TestLoginPreflightEmptyHost(t *testing.T) {
	if err := loginHostProblem(context.Background(), ""); err != nil {
		t.Errorf("空主机不该报错：%v", err)
	}
	if err := loginHostProblem(context.Background(), "   "); err != nil {
		t.Errorf("空白主机不该报错：%v", err)
	}
}

// TestLoginPreflightSendsBrowserUA 探测必须带浏览器 UA。
//
// ⚠️ 这条容易被忽略但很关键：这些站点前面有 WAF，Go 默认 UA
// （`Go-http-client/1.1`）会被直接拒掉 —— 那会把"WAF 挡机器人"
// **误判成"站点不可达"**，让用户去查一个根本没问题的网络。
func TestLoginPreflightSendsBrowserUA(t *testing.T) {
	var gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := loginHostProblem(context.Background(), srv.URL); err != nil {
		t.Fatalf("正常站点不该报错：%v", err)
	}
	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("必须用浏览器 UA（否则 WAF 会挡，导致误判），实际：%q", gotUA)
	}
	if gotPath != "/authorization" {
		t.Errorf("探测路径应为 /authorization（登录页的真实路径），实际：%q", gotPath)
	}
}
