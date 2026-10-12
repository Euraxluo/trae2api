// Command acct 管理网关的账号池。
//
// 用法：
//
//	acct list                       # 列出账号与状态
//	acct clear <uid>                # 解除某账号的冷却
//	acct disable <uid>              # 停用
//	acct enable <uid>               # 启用
//	acct delete <uid>               # 删除（快照账号会同时删文件）
//	acct snapshots                  # 列出便携快照文件
//
// 注意：**不要直接改 state.json** —— 网关内存里的池状态会在下一次落盘时
// 覆盖你的修改。所有变更都要经网关 API（这正是本工具存在的意义）。
package acct

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"trae2api/internal/cli"
)

// Run 执行 acct 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

func run(args []string) error {
	fs := flag.NewFlagSet("acct", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	asJSON := fs.Bool("json", false, "以 JSON 输出（便于脚本处理）")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `acct —— 账号池管理

用法:
  acct list [flags]                 列出账号与状态
  acct snapshots [flags]            列出便携快照文件
  acct clear [flags] <uid>          解除账号冷却
  acct disable [flags] <uid>        停用账号
  acct enable [flags] <uid>         启用账号
  acct delete [flags] <uid>         删除账号

注意：子命令必须写在 flags 之前（acct list -json）。写成 acct -json list
会得到"未知子命令"—— Go 的 flag 包遇到第一个非 flag 参数就停止解析。

flags:
`)
		fs.PrintDefaults()
	}

	if len(args) == 0 {
		fs.Usage()
		return cli.Usagef("缺少子命令")
	}
	cmd := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return cli.Usagef("%v", err)
	}
	c := cli.NewClient(*base, *key)

	switch cmd {
	case "list", "ls":
		return listAccounts(c, *asJSON)
	case "snapshots", "snap":
		return listSnapshots(c, *asJSON)
	case "clear", "uncooldown":
		return accountAction(c, fs.Arg(0), "clear-cooldown", "已解除冷却")
	case "disable":
		return accountAction(c, fs.Arg(0), "disable", "已停用")
	case "enable":
		return accountAction(c, fs.Arg(0), "enable", "已启用")
	case "delete", "rm":
		return deleteAccount(c, fs.Arg(0))
	default:
		fs.Usage()
		return cli.Usagef("未知子命令 %q", cmd)
	}
}

// statusResp 是 /status 的响应（只取本工具需要的字段）。
type statusResp struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Disabled int `json:"disabled"`
	Users    int `json:"users"`
	Accounts []struct {
		UID               string `json:"uid"`
		Label             string `json:"label"`
		Realm             string `json:"realm"`
		Healthy           bool   `json:"healthy"`
		Cooling           bool   `json:"cooling"`
		CoolReason        string `json:"cool_reason"`
		CoolKind          string `json:"cool_kind"`
		Degraded          bool   `json:"degraded"`
		DegradeRemain     int64  `json:"degrade_remain_seconds"`
		UnclassifiedFails int    `json:"unclassified_fails"`
		Disabled          bool   `json:"disabled"`
		Credits           *int64 `json:"credits"`
		InFlight          int    `json:"in_flight"`
		MaxInFlight       int    `json:"max_in_flight"`
		OkCount           int64  `json:"ok_count"`
		ErrCount          int64  `json:"err_count"`
		Owner             string `json:"owner"`
		ModelsCooled      int    `json:"models_cooled"`
	} `json:"accounts"`
}

func listAccounts(c *cli.Client, asJSON bool) error {
	var st statusResp
	if err := c.Get("/status", &st); err != nil {
		return err
	}
	if asJSON {
		cli.JSON(st)
		return nil
	}
	if len(st.Accounts) == 0 {
		fmt.Println("没有账号。用 `acct import` 或控制台「OAuth 一键登录」添加。")
		return nil
	}
	t := &cli.Table{Headers: []string{"UID", "域", "标签", "健康", "冷却", "积分", "在途", "成功/失败"}}
	sort.Slice(st.Accounts, func(i, j int) bool { return st.Accounts[i].UID < st.Accounts[j].UID })
	for _, a := range st.Accounts {
		// 表格单元格里只用确定宽度的标记（中文 2 列 / ASCII 1 列），
		// 不用 emoji —— emoji 的终端宽度实现相关，会让整张表错位。
		health := "正常"
		if a.Disabled {
			health = "已停用"
		} else if !a.Healthy {
			health = "异常"
		}
		cool := "-"
		if a.Cooling {
			kind := a.CoolKind
			if kind == "" {
				kind = "冷却"
			}
			cool = kind
			if a.CoolReason != "" {
				cool += "（" + cli.Truncate(a.CoolReason, 18) + "）"
			}
		}
		// 连败降权也是"当前不可选"的一种，必须显示 —— 否则用户会看到
		// 一个"健康但选不中"的账号，完全不知道为什么。
		if a.Degraded {
			if cool != "-" {
				cool += " + "
			} else {
				cool = ""
			}
			cool += fmt.Sprintf("降权%d秒", a.DegradeRemain)
		}
		if a.UnclassifiedFails > 0 {
			cool += fmt.Sprintf("(连败%d)", a.UnclassifiedFails)
		}
		credits := "—"
		if a.Credits != nil {
			credits = fmt.Sprintf("%d", *a.Credits)
		}
		t.Add(a.UID, a.Realm, cli.Truncate(a.Label, 20), health, cool, credits,
			fmt.Sprintf("%d/%d", a.InFlight, a.MaxInFlight),
			fmt.Sprintf("%d/%d", a.OkCount, a.ErrCount))
	}
	fmt.Print(t.Render())
	fmt.Printf("共 %d 个账号：健康 %d，冷却 %d，停用 %d\n", st.Total, st.Healthy, st.Cooling, st.Disabled)
	return nil
}

func listSnapshots(c *cli.Client, asJSON bool) error {
	var resp struct {
		Data []struct {
			File      string `json:"file"`
			Product   string `json:"product"`
			Label     string `json:"label"`
			UserID    string `json:"userId"`
			ExpiredAt string `json:"expiredAt"`
		} `json:"data"`
		Count   int      `json:"count"`
		Skipped []string `json:"skipped"`
		Dir     string   `json:"dir"`
	}
	if err := c.Get("/admin/accounts", &resp); err != nil {
		return err
	}
	if asJSON {
		cli.JSON(resp)
		return nil
	}
	if resp.Count == 0 {
		fmt.Printf("没有快照账号（目录 %s）\n", resp.Dir)
		return nil
	}
	t := &cli.Table{Headers: []string{"文件", "产品", "标签", "userId", "Token 到期"}}
	for _, s := range resp.Data {
		t.Add(s.File, s.Product, cli.Truncate(s.Label, 20), s.UserID, strings.ReplaceAll(s.ExpiredAt, "T", " "))
	}
	fmt.Print(t.Render())
	fmt.Printf("共 %d 个快照（目录 %s）\n", resp.Count, resp.Dir)
	for _, sk := range resp.Skipped {
		fmt.Printf("  ⚠️ 跳过：%s\n", sk)
	}
	return nil
}

func accountAction(c *cli.Client, uid, action, okMsg string) error {
	if strings.TrimSpace(uid) == "" {
		return cli.Usagef("缺少 <uid>（先用 `acct list` 查）")
	}
	path := fmt.Sprintf("/admin/accounts/%s/%s", uid, action)
	var out map[string]any
	if err := c.Post(path, nil, &out); err != nil {
		return err
	}
	fmt.Printf("✅ %s：%s\n", uid, okMsg)
	return nil
}

func deleteAccount(c *cli.Client, uid string) error {
	if strings.TrimSpace(uid) == "" {
		return cli.Usagef("缺少 <uid>")
	}
	// 先查快照文件名（若该账号来自快照，删除时要连文件一起删）
	var resp struct {
		Data []struct {
			File   string `json:"file"`
			UserID string `json:"userId"`
		} `json:"data"`
	}
	_ = c.Get("/admin/accounts", &resp)
	for _, s := range resp.Data {
		if strings.HasSuffix(uid, s.UserID) {
			var out map[string]any
			if err := c.Delete("/admin/accounts/"+s.File, &out); err != nil {
				return err
			}
			fmt.Printf("✅ 已删除快照账号 %s（文件 %s）\n", uid, s.File)
			return nil
		}
	}
	// 不是快照账号（来自 config 或自动探测）→ 只能停用，不能从池里删
	fmt.Printf("⚠️ %s 不是快照账号（来自配置或自动探测），无法删除。\n", uid)
	fmt.Printf("   要让它不参与调度，请用 `acct disable %s`，或从 config.json 移除。\n", uid)
	return nil
}
