// Command login 通过 OAuth 一键登录添加账号。
//
// 用法：
//
//	login                     # 登录 trae-cn（默认）
//	login -product solo-cn    # 登录 TRAE SOLO CN
//	login -no-browser         # 只打印 URL，不自动打开浏览器
//	login -timeout 5m         # 自定义等待时限
//
// 流程：网关起一个本地回调服务 → 你在这里看到的 URL 用浏览器打开 →
// 走完 SSO → 网关自动把凭据落盘并注册进池。
//
// 本工具只负责"发起 + 等结果"，真正的换 token 在网关侧完成。
package login

import (
	"flag"
	"fmt"
	"time"

	"trae2api/internal/cli"
)

// Run 执行 login 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

func run(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	product := fs.String("product", "trae-cn", "目标产品：trae-cn / solo-cn")
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器，只打印 URL")
	timeout := fs.Duration("timeout", 10*time.Minute, "等待登录完成的时限")
	if err := fs.Parse(args); err != nil {
		return cli.Usagef("%v", err)
	}
	c := cli.NewClient(*base, *key)

	var start struct {
		SessionID string `json:"session_id"`
		URL       string `json:"url"`
		Product   string `json:"product"`
	}
	if err := c.Post("/admin/login/start", map[string]any{
		"product":      *product,
		"open_browser": !*noBrowser,
	}, &start); err != nil {
		return err
	}

	fmt.Printf("已发起登录（产品 %s）\n\n", start.Product)
	fmt.Println("请在浏览器中打开下面的地址完成登录：")
	fmt.Println()
	fmt.Printf("  %s\n", start.URL)
	fmt.Println()
	if *noBrowser {
		fmt.Println("（已按 -no-browser 跳过自动打开）")
	} else {
		fmt.Println("（已尝试自动打开浏览器）")
	}
	fmt.Println()

	// 轮询状态。网关侧恒返 200，所以这里不会因一次 5xx 而中断。
	deadline := time.Now().Add(*timeout)
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	lastState := ""
	for {
		if time.Now().After(deadline) {
			_ = c.Post("/admin/login/cancel?session_id="+start.SessionID, nil, nil)
			return fmt.Errorf("登录超时（%s 内未完成）", *timeout)
		}
		<-tick.C

		var st struct {
			State   string `json:"state"`
			Elapsed int    `json:"elapsed_sec"`
			UserID  string `json:"user_id"`
			SavedAs string `json:"saved_as"`
			Saved   bool   `json:"saved"`
			SaveErr string `json:"save_error"`
			Error   string `json:"error"`
		}
		if err := c.Get("/admin/login/status?session_id="+start.SessionID, &st); err != nil {
			// 单次轮询失败不致命，继续等（网关可能正在重启）
			continue
		}
		if st.State != lastState {
			lastState = st.State
		}
		switch st.State {
		case "success":
			fmt.Printf("\n✅ 登录成功\n")
			fmt.Printf("   账号      : %s\n", st.UserID)
			fmt.Printf("   保存为    : %s\n", st.SavedAs)
			if st.Saved {
				fmt.Printf("   已注册进池: 是\n")
			}
			if st.SaveErr != "" {
				fmt.Printf("   ⚠️ %s\n", st.SaveErr)
			}
			fmt.Println()
			fmt.Println("可以关闭浏览器页面了。")
			return nil
		case "error":
			return fmt.Errorf("登录失败：%s", st.Error)
		case "unknown":
			return fmt.Errorf("登录会话已过期（%s）", st.Error)
		case "waiting":
			fmt.Printf("\r等待中… 已 %s", cli.Duration(st.Elapsed))
		}
	}
}
