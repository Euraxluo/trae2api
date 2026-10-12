// Command signin 触发一次签到。
//
// 用法：
//
//	signin                  # 给所有账号签到（幂等：已签的会跳过）
//	signin -uid <uid>       # 只签某个账号
//
// 签到是**幂等**的：已签到时上游返回业务码非 0 而不是 500。
// 本工具把"今天已签"当成正常路径，退出码仍是 0。
package signin

import (
	"flag"
	"fmt"
	"sort"

	"trae2api/internal/cli"
)

// Run 执行 signin 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

func run(args []string) error {
	fs := flag.NewFlagSet("signin", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	uid := fs.String("uid", "", "只签指定账号")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return cli.Usagef("%v", err)
	}
	c := cli.NewClient(*base, *key)

	var resp struct {
		Checked int `json:"checked"`
		Already int `json:"already"`
		Failed  int `json:"failed"`
		Results []struct {
			UID     string `json:"uid"`
			Status  string `json:"status"`
			Detail  string `json:"detail"`
			Credits int64  `json:"credits"`
		} `json:"results"`
	}
	if err := c.Post("/admin/checkin", nil, &resp); err != nil {
		return err
	}

	rows := resp.Results
	if *uid != "" {
		filtered := rows[:0]
		for _, r := range rows {
			if r.UID == *uid {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	if *asJSON {
		cli.JSON(resp)
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("没有账号可签到。")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })

	t := &cli.Table{Headers: []string{"UID", "结果", "说明", "本次积分"}}
	for _, r := range rows {
		// 表格单元格里不用 emoji（终端宽度实现相关），只用中文/ASCII
		mark := map[string]string{
			"checked": "已签", "already": "今日已签", "failed": "失败",
		}[r.Status]
		if mark == "" {
			mark = r.Status
		}
		credits := "—"
		if r.Credits > 0 {
			credits = fmt.Sprintf("+%d", r.Credits)
		}
		t.Add(r.UID, mark, cli.Truncate(r.Detail, 40), credits)
	}
	fmt.Print(t.Render())
	fmt.Printf("新签 %d，今日已签 %d，失败 %d\n", resp.Checked, resp.Already, resp.Failed)

	// 有失败时以非 0 退出，便于脚本/定时任务感知
	if resp.Failed > 0 {
		return fmt.Errorf("%d 个账号签到失败", resp.Failed)
	}
	return nil
}
