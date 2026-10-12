// Command models 列出网关暴露的模型目录。
//
// 用法：
//
//	models                     # 全部模型
//	models -realm trae         # 只看某个域
//	models -multimodal         # 只看支持图片的
//	models -reasoning          # 只看支持推理的
//	models -name glm           # 按名称过滤
//
// 这张表就是"客户端能填什么模型名"的权威答案 —— 表里的 id 列可直接粘进
// 任何 OpenAI 兼容客户端。
package models

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"trae2api/internal/cli"
)

// Run 执行 models 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

func run(args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	realm := fs.String("realm", "", "只看指定域（trae / solo）")
	name := fs.String("name", "", "按名称子串过滤")
	onlyMM := fs.Bool("multimodal", false, "只看支持图片的")
	onlyReason := fs.Bool("reasoning", false, "只看支持推理的")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return cli.Usagef("%v", err)
	}
	c := cli.NewClient(*base, *key)

	var resp struct {
		Data []struct {
			ID               string   `json:"id"`
			DisplayName      string   `json:"display_name"`
			Realm            string   `json:"realm"`
			Multiplier       string   `json:"multiplier"`
			ModelType        string   `json:"model_type"`
			ContextWindow    int      `json:"context_window"`
			MaxTokens        int      `json:"max_tokens"`
			Multimodal       bool     `json:"multimodal"`
			Reasoning        bool     `json:"reasoning"`
			ReasoningEfforts []string `json:"reasoning_efforts"`
			NativeFC         bool     `json:"native_tool_calling"`
			IsBeta           bool     `json:"is_beta"`
		} `json:"data"`
	}
	if err := c.Get("/v1/models", &resp); err != nil {
		return err
	}

	rows := resp.Data[:0]
	for _, m := range resp.Data {
		if *realm != "" && m.Realm != *realm {
			continue
		}
		if *name != "" && !strings.Contains(strings.ToLower(m.ID), strings.ToLower(*name)) &&
			!strings.Contains(strings.ToLower(m.DisplayName), strings.ToLower(*name)) {
			continue
		}
		if *onlyMM && !m.Multimodal {
			continue
		}
		if *onlyReason && !m.Reasoning {
			continue
		}
		rows = append(rows, m)
	}
	if *asJSON {
		cli.JSON(map[string]any{"object": "list", "data": rows, "count": len(rows)})
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("没有匹配的模型。")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Realm != rows[j].Realm {
			return rows[i].Realm < rows[j].Realm
		}
		return rows[i].ID < rows[j].ID
	})

	t := &cli.Table{Headers: []string{"模型 ID（直接填这个）", "显示名", "域", "倍率", "上下文", "图片", "推理档位", "工具"}}
	for _, m := range rows {
		ctx := "—"
		if m.ContextWindow > 0 {
			ctx = fmt.Sprintf("%dk", m.ContextWindow/1000)
		}
		efforts := "-"
		if len(m.ReasoningEfforts) > 0 {
			efforts = strings.Join(m.ReasoningEfforts, "/")
		} else if m.Reasoning {
			efforts = "支持"
		}
		tools := "-"
		if m.NativeFC {
			tools = "支持"
		}
		// 表格里用中文/ASCII 而不是 emoji：emoji 的终端宽度实现相关
		pic := "-"
		if m.Multimodal {
			pic = "支持"
		}
		t.Add(m.ID, cli.Truncate(m.DisplayName, 22), m.Realm, m.Multiplier, ctx, pic, efforts, tools)
	}
	fmt.Print(t.Render())
	fmt.Printf("共 %d 个模型\n", len(rows))
	fmt.Println()
	fmt.Println("提示：「倍率」是上游的积分倍率档位；「—」表示上游未下发（不等于免费）。")
	return nil
}
