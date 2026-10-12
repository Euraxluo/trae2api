package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 本文件实现 **OAuth 登录页的启动前预检**。
//
// ## 为什么需要它
//
// 实测踩到的场景：用户点了「OAuth 一键登录」，浏览器打开
// `https://www.trae.cn/authorization?...` 后显示「无法访问此页面」，
// 而网关这边**什么都不知道** —— 它只记了一句"等待浏览器授权"，
// 然后等满 10 分钟超时，报"登录超时（10m0s 内未完成）"。
// 用户完全看不出问题出在哪：是 URL 拼错了？是网关坏了？还是网络问题？
//
// 真实原因（实测）：`www.trae.cn` 由 volc-dcdn 提供，**按区域封锁**。
// 出口 IP 在美国（走代理）时返回：
//
//	HTTP 403  server=volc-dcdn  body="抱歉，您当前区域不支持访问"
//
// 注意这是个**很容易误判**的场景：
//   - `api.trae.cn` 与 `trae-api-cn.mchost.guru` **不受影响**（网关读凭据、续期、调模型全都正常），
//     所以"网关能用"并不代表"登录页能打开"；
//   - 登录 URL 本身**没问题** —— 逐参数对比过客户端
//     `out-build/vs/code/electron-main/oauth/userLogin/loginUrlBuilder.js` 的 `buildLoginUrl()`，
//     路径、参数顺序、取值都一致。
//
// 所以这个预检的价值是：**把"10 分钟后一句含糊的超时"变成"3 秒后一句可照做的错误"**。

// loginPreflightTimeout 是探测登录页的超时。
//
// 取 8 秒：正常站点 1 秒内就答；8 秒还没答说明网络真的有问题，
// 此时让用户等下去也没有意义（浏览器只会更慢）。
const loginPreflightTimeout = 8 * time.Second

// browserUA 是探测用的 User-Agent。
//
// ⚠️ 必须用浏览器 UA：这些站点前面有 WAF，Go 默认 UA（`Go-http-client/1.1`）
// 会被直接拒掉，从而**把"WAF 挡机器人"误判成"站点不可达"**。
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0"

// regionBlockMarkers 是"区域封锁"页面的特征串。
//
// 只认这些**明确的**特征，不做宽泛匹配 —— 宽泛匹配会把正常的业务错误
// 误判成区域封锁，那比不检测更糟（用户会去改一个没问题的代理配置）。
var regionBlockMarkers = []string{
	"区域不支持",
	"当前区域",
	"region is not supported",
	"not available in your region",
	"unsupported region",
}

// loginHostProblem 探测登录页主机，返回一个**可照做**的错误；nil 表示看起来正常。
//
// 判定策略（保守优先）：
//   - 明确的区域封锁特征 → 报错（这是最常见也最需要解释的场景）
//   - 连不上（DNS / 连接被拒 / 超时）→ 报错（浏览器同样连不上）
//   - 其它状态码（404/500 等）→ **不拦**，只当作"可能有别的问题"放行
//
// 为什么对其它状态码放行：我们只对**已经确认过**的失败模式负责。
// 一个没见过的状态码可能是 WAF 的正常拦截、可能是维护页、也可能是我们探测姿势不对，
// 拿它去阻止用户尝试登录是过度自信。
func loginHostProblem(ctx context.Context, host string) error {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return nil // 没配主机，交给下游走默认值
	}
	target := host + "/authorization"

	pctx, cancel := context.WithTimeout(ctx, loginPreflightTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(pctx, http.MethodGet, target, nil)
	if err != nil {
		return nil // URL 本身非法是配置问题，downstream 会报得更准
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")

	resp, err := loginProbeClient.Do(req)
	if err != nil {
		return fmt.Errorf("登录页 %s 连不上：%v。"+
			"浏览器打开时会是同样的结果 —— 请检查网络/代理是否能访问该域名"+
			"（注意：`api.trae.cn` 能通**不代表**登录页能通，两者是不同的 CDN）", host, err)
	}
	defer resp.Body.Close()

	// 只读前 4KB：区域封锁页只有几十字节，正常页面也不需要全读。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := string(body)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnavailableForLegalReasons {
		for _, m := range regionBlockMarkers {
			if strings.Contains(text, m) {
				return fmt.Errorf("登录页 %s 拒绝访问：HTTP %d，返回「%s」。"+
					"**该域名按区域封锁**，你当前的出口 IP 所在区域不被支持。"+
					"两条出路：① **改用导入**（推荐，不受区域限制）—— 在任意一台能正常登录的机器上"+
					"登录客户端，把 `storage.json` 拷过来，用控制台的「导入账号」贴进去即可；"+
					"② 若坚持走浏览器授权：在你的代理软件里为 `trae.cn` 与 `trae.com.cn` 添加 "+
					"**DIRECT（直连）** 规则，或切到国内节点后重试。"+
					"（对照：`api.trae.cn` 通常不受影响，所以网关读凭据、续期、调模型都正常，"+
					"**只有登录页打不开** —— 这是个很容易误判成「网关坏了」的现象）",
					host, resp.StatusCode, strings.TrimSpace(text))
			}
		}
	}

	// 其它情况放行（见函数头说明：只对确认过的失败模式负责）
	return nil
}

// loginProbeClient 是探测专用的 HTTP 客户端。
//
// 单独一个而不是复用共享客户端：探测要**短超时**，而对话客户端是长超时
// （推理模型可能几分钟）。用同一个会把两种截然不同的期望混在一起。
var loginProbeClient = &http.Client{
	Timeout: loginPreflightTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// 不跟随重定向：我们只想知道"这个地址能不能拿到页面"。
		// 跟随会让一次探测变成多次请求，也会把 403 之后的重定向掩盖掉。
		return http.ErrUseLastResponse
	},
}
