// Command credit 查询账号额度。
//
// 用法：
//
//	credit                  # 全部账号（走网关缓存，快）
//	credit -refresh         # 强制实时刷新（会打上游，慢几秒）
//	credit -uid <uid>       # 只看某个账号
package credit

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"trae2api/internal/cli"
)

// Run 执行 credit 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

func run(args []string) error {
	fs := flag.NewFlagSet("credit", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	uid := fs.String("uid", "", "只看指定账号")
	refresh := fs.Bool("refresh", false, "强制实时刷新（会打上游）")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return cli.Usagef("%v", err)
	}
	c := cli.NewClient(*base, *key)

	if *refresh {
		var out map[string]any
		if err := c.Post("/admin/credits/refresh", nil, &out); err != nil {
			return err
		}
		fmt.Println("已触发实时刷新。")
	}

	var resp struct {
		Data []struct {
			UID              string  `json:"uid"`
			Label            string  `json:"label"`
			Remaining        float64 `json:"remaining"`
			Total            float64 `json:"total"`
			ConsumptionRatio float64 `json:"consumption_ratio"`
			EntitlementPacks int     `json:"entitlement_packs"`
			CheckedInToday   bool    `json:"checked_in_today"`
			UpdatedAt        string  `json:"updated_at"`
		} `json:"data"`
		Count          int     `json:"count"`
		TotalRemaining float64 `json:"total_remaining"`
	}
	if err := c.Get("/v1/credits", &resp); err != nil {
		return err
	}
	if *uid != "" {
		filtered := resp.Data[:0]
		for _, d := range resp.Data {
			if strings.Contains(d.UID, *uid) {
				filtered = append(filtered, d)
			}
		}
		resp.Data = filtered
	}
	if *asJSON {
		cli.JSON(resp)
		return nil
	}
	if len(resp.Data) == 0 {
		fmt.Println("没有账号，或没有匹配的账号。")
		return nil
	}
	t := &cli.Table{Headers: []string{"UID", "标签", "剩余", "总量", "已用", "权益包", "今日签到"}}
	sort.Slice(resp.Data, func(i, j int) bool { return resp.Data[i].UID < resp.Data[j].UID })
	for _, d := range resp.Data {
		t.Add(d.UID, cli.Truncate(d.Label, 20),
			fmt.Sprintf("%.2f", d.Remaining),
			fmt.Sprintf("%.2f", d.Total),
			fmt.Sprintf("%.1f%%", d.ConsumptionRatio*100),
			fmt.Sprintf("%d", d.EntitlementPacks),
			cli.Bool(d.CheckedInToday))
	}
	fmt.Print(t.Render())
	fmt.Printf("合计剩余 %.2f\n", resp.TotalRemaining)
	fmt.Println()
	fmt.Println("提示：数值来自网关缓存的额度快照。要拿最新值加 -refresh（会打上游，慢几秒）。")
	return nil
}
