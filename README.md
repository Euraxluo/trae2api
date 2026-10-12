# Trae2Api

把 **Trae CN** 与 **TRAE SOLO CN** 的订阅额度，转成标准 **OpenAI 兼容 API** 的本地网关。

### 核心

- **零第三方依赖**：只用 Go 标准库（连 SQLite 读取、PDF 解析都是自实现的）
- **单一可执行文件**：约 13 MB，一个文件就是全部功能（`serve` / `acct` / `credit` / `models` / `signin` / `login` / `stats`）
- **OpenAI 兼容**：`/v1/chat/completions`（流式 SSE + 思维链）、`/v1/models`
- **原生工具调用**：上游支持 function calling，做真正的 tools 映射而非提示词模拟
- **附件展开**：消息里写 `file` 片段或 `@路径`，网关读文件转成文本/图片片段；
  支持 `.txt` / `.md` / `.json` / `.csv` / 各种代码文件 / **`.pdf`**（零依赖自实现 PDF 文本提取）
- **模型目录**：倍率、推理档位、图标、真实上下文窗口、Max 模式等完整字段

### 账号与调度

- **自动续期**：token 过期自动刷新并写回客户端凭据，不会把桌面客户端挤下线
- **账号池**：多账号轮换、加性加权选号、分级冷却（账号级 / 模型级）、熔断、并发租约、状态跨重启保留
- **快过期额度优先**：选号时优先消耗面临作废的额度，而不是均匀轮询到过期为止
- **并发排队**：名额满时排队等待而非立刻 503（只在"值得等"时才等）
- **会话粘性**：同一多轮对话固定绑同一账号，保住上游 prompt cache 命中
- **工具配对自愈**：坏会话历史（孤儿 tool_call / 结果）不再让整条会话报废
- **prompt cache key**：按账号隔离的前缀缓存键（实测费用降约 17×）
- **WAF IP 级 fail-fast**：短窗口内多个**不同**账号被拦截时判定为 IP 级封禁，立刻停止轮转而不是放大请求量
- **两级降级**：提示词降级闸（内容被拦后切中性提示词到次日零点）+ 连败降权（持续"说不清原因的失败"后临时出池）

### 版本域（国内 / 国际）

- **国际版 / 国内版**：`trae-intl` / `solo-intl` 独立 realm（`trae-intl:模型名` 前缀），
  端点可配置，导入时按凭据 host 自动识别版本域，跨域兜底
- **严格模型校验**：未知模型名直接 400 并列出可用清单，杜绝"静默回退到别的模型还照样计费"
- **同邮箱跨渠道**：见下文「已知限制」—— 上游凭据**不含**登录渠道字段，此项**做不到**，已如实记录

### 凭证与用户

- **凭证导入/导出**：粘贴文本 / 上传 `.json`·`.txt`·`.zip`；兼容客户端 `storage.json` 原文与多格式清洗；导出为可移植 ZIP
- **导入后异步校验**：续期 + 拉额度，区分「凭据问题」与「网络问题」，进度可轮询
- **脱离客户端运行**：导出的便携快照含设备密钥对，拷到没装 Trae 的机器上也能续期与调用
- **OAuth 一键登录**：浏览器授权即可新增账号（PKCE + 本地回调 + AuthCode 换 token）
- **多用户 RBAC**：用户专属 API Key、admin/user 角色、账号 owner 归属隔离

### 安全

- **控制台密码**：`web_password` 保护管理中枢（与 API Key 正交，常数时间比较，HttpOnly 会话，≥6 位）
- **密码与角色管理**：改密核验原密码并作废所有会话；不能降级最后一个管理员
- **登录防爆破**：按来源 IP 限流（5 分钟 5 次 → 锁 5 分钟），代理头**默认不采信**
- **全站安全响应头**：`nosniff` / `SAMEORIGIN` / `no-referrer` / CSP，加在**最外层**中间件（不会漏）

### 可观测与运维

- **公开门户 / 管理中枢分离**：`/` 只透出脱敏可用性，`/console` 才是管理面
- **Web 控制台**：账号健康度、冷却倒计时、剩余积分、一键签到、解除冷却、导入导出、用户管理
- **Playground 演练场**：控制台内直接选模型发请求（流式/非流式、可选显示思维链）；
  支持点选图片与代码文件、`Ctrl+V` 粘贴截图
- **Agent (dsh) 标签页**：控制台内一键启停 DeepSeek Harness，iframe 内嵌其网页 UI；**防孤儿**（Windows Job Object，网关被强杀也不会留下 dsh 进程）
- **指标与趋势**：`/v1/stats` 提供 global / by_model / by_account / by_realm / by_day 聚合，含 TTFB、耗时、token、缓存命中率
- **TUI 仪表盘**：`stats` 交互式查看账号池、全局指标、分模型、按日趋势（三层防御渲染 + CJK 宽度自实现）
- **桌面 GUI**：`serve -gui` 开一个原生窗口（内嵌 WebView2，**零 Go 依赖**；缺失时自动降级到浏览器 App 模式）；**关窗即优雅停机**
- **自动签到**：每日定时（默认 9/21 点），幂等，失败不影响其他账号
- **可观测**：`/status`、`/v1/credits`、`/v1/stats`、结构化请求日志（含账号、耗时、token）
- **连接层加固**：禁 h2（空 `TLSNextProto`）、建连/TLS/响应头超时、短 keepalive、失败清空闲池
- **流中空闲监控**：上游"永不吐数据"不再能挂住请求（区别于整体超时，不误杀长回复）
- **CI/CD**：GitHub Actions 跑测试（含 `-race`）、gofmt、依赖零约束校验、5 平台交叉编译、Docker 构建校验；打 tag 自动发布多平台产物 + SHA256


> 本项目**独立实现**，架构设计上参考了 [workbuddy2api](https://github.com/XCool-603/wrokbuddy2api)
> 的分域 / 账号池 / 冷却熔断思路（详见下文「架构」），但代码完全独立，
> **不修改也不合并进 workbuddy2api**。

---

## 快速开始

**单一可执行文件**，所有功能都是它的子命令：

```bash
# 1. 编译（只产出这一个文件）
go build -o trae2api ./cmd/trae2api

# 2. 运行（首次会生成 config.json 模板）
./trae2api                 # 不带子命令 = 启动网关
./trae2api serve           # 等价写法
```

前提：本机已安装并登录 **Trae CN** 或 **TRAE SOLO CN** 桌面客户端。
网关会自动探测并读取其凭据，无需手工配置。

```
Base URL : http://127.0.0.1:7870/v1
公开门户 : http://127.0.0.1:7870/
管理中枢 : http://127.0.0.1:7870/console
API Key  : 未设置（无需鉴权）
模型前缀 : trae:<模型名>  (Trae CN)
           solo:<模型名>  (TRAE SOLO CN)
```

### 子命令一览

| 子命令 | 用途 |
|---|---|
| `serve` | 启动网关（不带子命令时即此） |
| `acct` | 账号管理（list / add / del / enable / disable / cooldown） |
| `credit` | 额度查询（`-refresh` 强制实时刷新） |
| `models` | 模型清单（`-multimodal` 只看多模态） |
| `signin` | 每日签到 |
| `login` | OAuth 一键授权（`-product trae-cn / solo-cn / trae-intl / solo-intl`） |
| `stats` | 实时统计 TUI（`-once` 打印一次后退出） |
| `version` / `help` | 版本 / 帮助 |

> **为什么是单一可执行文件**：参考实现把"摒弃散落的辅助可执行文件"列为第一条特色。
> 本项目最初做成了 7 个 exe（`trae2api-server.exe` / `-acct` / `-credit` / …），
> 现已收拢 —— 分发只需一个文件、启动脚本不必串多个 exe、所有命令必然同版本。


任意支持自定义 OpenAI 地址的客户端（Cherry Studio / Chatbox / Cline / Cursor / NextChat）：

| 配置项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:7870/v1` |
| API Key | 留空，或填 `config.json` 里的 `api_key` |
| 模型名 | `trae:glm-5.2`、`trae:kimi-k3`、`solo:glm-5.3` … |

```bash
curl http://127.0.0.1:7870/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"trae:glm-5.2","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

---

## 架构

```
              ┌──────────────────────────────────────────────┐
  OpenAI 客户端 ──► │  server   /v1/chat/completions  /v1/models    │
              │           /status   /  (只读控制台)           │
              └───────────────┬──────────────────────────────┘
                              │ realm.ResolveModel("trae:glm-5.2")
                              ▼
              ┌──────────────────────────────────────────────┐
              │  realm      trae / solo 枚举 + 前缀解析        │
              ├──────────────────────────────────────────────┤
              │  pool       选号 · 分级冷却 · 熔断 · 并发租约   │
              ├──────────────────────────────────────────────┤
              │  session    会话粘性（保住上游 prompt cache）   │
              ├──────────────────────────────────────────────┤
              │  traectl    tc 凭据加解密 · ECDSA 续期 ·       │
              │             chat 客户端（SSE + 工具调用）      │
              │  vscdb      零依赖只读 SQLite（读模型目录）     │
              └───────────────┬──────────────────────────────┘
                              ▼
              api.trae.cn  /  trae-api-cn.mchost.guru
```

### 借鉴 workbuddy2api 的设计

| 设计点 | 为什么借鉴 |
|---|---|
| **realm 分域枚举** | workbuddy2api 曾因把 `cn`/`global` 硬编码在 20 处，加第三个域时极易漏改。本项目从第一天就把域做成枚举 + 前缀解析，新增渠道只需在 `internal/realm` 登记。 |
| **账号池 + 三因子加权选号** | 空闲补偿 × 成功率 × 余额。本项目独立实现，并明确「未知不当成差」——新账号不会因样本不足被永久冷落。 |
| **冷却与熔断分离** | 限流是短时避让（可兜底重试），凭证失效是"调了必然失败"（硬冷却，不参与兜底）。 |
| **模型级独立冷却** | 单个模型不可用不该冻掉整个账号。换模型即豁免。 |
| **会话粘性** | 上游按会话做 prompt cache，中途换号会让缓存全部失效。 |
| **状态落盘** | 冷却/熔断/计数跨重启保留；但**不落盘 token**——凭据唯一事实来源是客户端 `storage.json`。 |
| **零第三方依赖** | 便于交叉编译与离线部署。连 SQLite 都是自实现的只读读取器。 |

### 与 workbuddy2api 的差异

| | workbuddy2api | 本项目 |
|---|---|---|
| 上游 | 腾讯 CodeBuddy / WorkBuddy | 字节跳动 Trae CN / TRAE SOLO CN |
| 协议 | 自有 HTTP + SSE | 自有 HTTP + SSE（完全不同的字段与错误码） |
| 凭据 | `auths/*.json` | 客户端 `storage.json`（`tc` 加密信封） |
| 续期 | 简单 refresh | **ECDSA P-256 DeviceProof 签名** |
| 模型目录 | 上游 API 动态拉取 | 客户端 `state.vscdb`（SQLite） |
| 二进制 | 15.4 MB | 11.2 MB |

---

## 协议要点（实测踩过的坑）

以下全部来自对本机客户端的逆向 + 真实上游验证。这些点如果搞错，症状往往
**完全不指向根因**，非常难排查，故单独列出。

### 1. 凭据存储

```
%APPDATA%\<产品目录>\User\globalStorage\storage.json
  ├─ "iCubeAuthInfo://icube.cloudide"          ← 主凭据（tc 加密）
  ├─ "iCubeAuthInfo://icube-dc:<数字ID>"       ← 设备 EC P-256 密钥对
  └─ "telemetry.machineId"                     ← 设备指纹
```

产品目录：`Trae CN` / `TRAE SOLO CN`。

### 2. `tc` 信封解密

```
[6B 头][32B 随机盐][AES-128-CBC 密文]
明文 = [64B SHA-512 校验和][JSON]
key‖iv = SHA512( SHA512(随机盐) ‖ salt )   → key=前16B, iv=次16B
```

头部 `74 63 05 10 00 00`（ASCII `tc`）→ 盐 = `SALT_A ^ SALT_B`。
盐表可用 `TRAE_SALT_A..D` 环境变量覆盖，便于上游改表后热修。

> ⚠️ **坑**：必须先做 PKCS#7 去填充再切分 64B 校验和。Node 的 `decipher.final()`
> 会自动去填充，Go 的 `CryptBlocks` 不会 —— 漏掉这一步会让 SHA-512 **恒失败**。

### 3. 续期：只有一条正确路径

```
POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken
headers: Content-Type: application/json
         x-cloudide-token: ""        ← 必须空串
body:    { ClientID, ClientSecret:"", RefreshToken,
           DeviceInfo{...}, DeviceProof{Signature,Timestamp,Nonce}, IDEVersion }

签名原文 = "POST\n/trae/api/v3/oauth/ExchangeToken\n<ClientID>\n<RefreshToken>\n<unix秒>\n<32hex nonce>"
签名算法 = ECDSA P-256 / SHA-256 / DER / base64，私钥来自 iCubeAuthInfo://icube-dc:*
```

| 产品 | ClientID | PlatformCode |
|---|---|---|
| Trae CN | `ono9krqynydwx5` | `IDE_PC` |
| TRAE SOLO CN | `en1oxy7wnw8j9n` | `SOLO_PC` |

> ⚠️ **坑**：ClientID **由「是不是 SOLO」决定，不是由 CN/国际决定**（源码
> `gr(t) ? authConfig.SOLO : authConfig.TRAE`）。网上多篇分析各说一半，照抄会踩雷。
>
> ⚠️ **坑**：`DeviceProof` 字段名必须 PascalCase，小写会被服务端当成"未提供证明"
> 返回 20405。`x-cloudide-token` 带旧 token 会走到另一条鉴权分支，同样 20405。
>
> ⚠️ 旧的 `/cloudide/api/v3/trae/oauth/ExchangeToken` **在当前客户端中已完全不存在**。

### 4. Chat 上游

```
POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat
headers: Authorization: Cloud-IDE-JWT <token>
         X-Cloudide-Token / x-uid / x-app-id / x-device-id / x-machine-id
         x-ide-version / x-ide-version-code（必须纯数字）
body:    { messages, model, function, stream:true, request_id, session_id, tools? }
```

`function` 字段决定用哪个模型池：`chat_v3`（Trae CN）/ `solo_work_lite`（SOLO CN）。
`request_id` / `session_id` **必填** —— 缺失时上游返回 HTTP 200 + 误导性的
`4011`/`4001` 错误事件，极易误判为限流而错误冷却账号。

### 5. ⚠️ 最重要的一个坑：User-Agent 被用于机器人识别

上游**按 User-Agent 做限流/拦截**：

| 客户端 | 结果 |
|---|---|
| Node 默认 UA | ✅ 正常 |
| **Go 默认 UA（`Go-http-client/1.1`）** | ❌ **HTTP 200 + 业务码 4011「超出限流」** |
| 官方客户端 UA（内置 node-fetch） | ✅ 正常 |
| `axios/1.7.7` | ✅ 正常 |

错误信息完全指向"限流"，与真实原因（UA 被识别为爬虫）毫无关系，极难排查。
本项目固定使用官方客户端内置 node-fetch 的默认 UA，并加了回归测试防退化。

### 6. SSE 事件映射

| 上游事件 | 含义 | 处理 |
|---|---|---|
| `output{response}` | 正文增量 | → `delta.content` |
| `output{reasoning_content}` | 思维链增量 | → `delta.reasoning_content` |
| `output{tool_calls}` | 工具调用增量 | → `delta.tool_calls`（需按 index 聚合） |
| `token_usage` | 真实用量 | → `usage`（含 cached / reasoning 明细） |
| `done{finish_reason}` | 结束 | 见下方改写 |
| **`extra_info{content}`** | **完整思维链全文** | **必须丢弃** |
| `metadata` / `timing_cost` / `queue_*` | 元信息 | 忽略（排队位置绝不能进 content） |

> ⚠️ **坑 T1**：`extra_info.content` 是**完整思维链全文**，不是正文。当成正文累加
> 会把整段思考混进回复。
>
> ⚠️ **坑 T2**：`response` 与 `reasoning_content` 同帧互斥（另一为 `null`）。
>
> ⚠️ **坑 T3**：SSE 流含 `id:N` 行，解析器不能只认 `event:` / `data:`。
>
> ⚠️ **坑 T4**：上游发出 `tool_calls` 时 `finish_reason` 仍是 `"stop"`，
> **必须改写为 `"tool_calls"`**，否则客户端不会执行工具。

### 7. 工具调用

`tools[].function.parameters` **必须是 JSON 字符串**，传 object 会被上游以 HTTP 400
拒绝并抛出 Go 类型错误：

```
json: cannot unmarshal object into Go struct field
  FunctionDefinition.tools.function.parameters of type string
```

上游字段名是 `function_call`（非 `tool_call`），`name` 在首帧给出、
`arguments` 在后续帧流式续传，需按 `index` 聚合。

### 8. 模型目录

从客户端 `state.vscdb`（SQLite）的 `ItemTable` 读取，key 形如
`<userId>_AI.agent.model.model_list_map`。这是**服务端下发**的真实清单，
比任何硬编码表都准确。

> 本项目为此自实现了零依赖的只读 SQLite 读取器（`internal/vscdb`），
> 支持 overflow page 链与 WAL 只读解析，测试用真实文件的 SHA-256 与
> `node:sqlite` 对拍通过。

### 9. 签到与额度：鉴权头与 chat **不一样**

成长/商业类接口（签到、额度）走的是另一套鉴权：

| | chat 接口 | 签到/额度接口 |
|---|---|---|
| 端点 | `/api/agent/v3/llm_utils_chat` | `/trae/api/v2/ug/*`、`/trae/api/v2/pay/*` |
| 鉴权 | `Authorization` + `X-Cloudide-Token` | 只需 `Authorization: Cloud-IDE-JWT <token>` |
| 设备头 | `x-device-id` = **sha256(machineId) 前 32 位** | `x-device-id` = **数字设备 ID**（`icube-dc:` 后缀） |
| 其他头 | 全套指纹头 + `request-traffic-type` | 无 |

**混用会 401**。这两个 `x-device-id` 的取值形式完全不同，是最容易搞错的一处。

```
POST /trae/api/v2/ug/checkin_credits/status   → {checked_in, enable, credits, extra_credits}
POST /trae/api/v2/ug/checkin_credits/claim    → {code:0, message:"success"}（固定 100 积分）
POST /trae/api/v2/pay/ide_user_ent_usage      → {usage_summary:{consumed_amount,total_amount,consumption_ratio}, user_entitlement_pack_list:[...]}
```

> ⚠️ **坑**：签到是**幂等**的 —— 已签到时上游返回业务码非 0 而不是 500。
> 调用方必须把"今天已签"当成正常路径，否则定时任务会在已签到的日子里刷满告警。
> 本项目的 `CheckinAndReport` 先查状态、仅在未签到时才 claim。

---

## 配置说明（`config.json`）

```jsonc
{
  "listen": "127.0.0.1:7870",
  "api_key": "",              // 非空则 /v1/* 需 Bearer Token
  "web_password": "",
  "data_dir": "./data",       // 状态快照目录

  "pool": {
    "max_in_flight": 3,           // 单账号在途并发上限
    "breaker_threshold": 3,       // 连续失败熔断阈值
    "breaker_cooldown": "30m",
    "idle_weight_per_hour": 0.5,  // 空闲补偿：每空闲 1 小时 +0.5 权重
    "idle_weight_max": 5.0,
    "pick_gap": "100ms"           // 选号最小间隔，防惊群
  },

  "cooldown": {
    "soft_rate": "600s",      // 限流软冷却基数（按连续次数指数退避）
    "soft_rate_max": "2h",
    "model_cooldown": "600s"  // 模型级冷却基数
  },

  "trae": {
    // 空 = 自动探测本机已登录的产品（各一个账号）
    "accounts": [
      { "product": "trae-cn", "label": "主号" },
      { "product": "solo-cn", "label": "SOLO" },
      // 多账号：把另一份 storage.json 放到别处，指向它即可
      { "product": "trae-cn", "label": "小号",
        "storage_path": "D:/trae-b/User/globalStorage/storage.json",
        "owner": "alice", "max_in_flight": 2 }
    ],
    "chat_base": "https://trae-api-cn.mchost.guru",
    "user_agent": ""   // ⚠️ 强烈建议留空（见上文「最重要的一个坑」）
  },

  "session_sticky": { "enabled": true, "ttl": "30m" }
}
```

### 多账号

Trae 的额度是**按账号**的，同一账号在两个产品下共享同一额度池
（实测 `ide_user_ent_usage` 返回完全相同的数值），所以
**多开客户端不会增加额度**——真正的池化需要多个不同的 Trae 账号。

配置方式：把每个账号的 `storage.json` 放在不同路径，各写一条 `accounts` 条目。
网关会为每条注册一个池内账号，从而获得选号轮换、账号级冷却与会话粘性。

---

## 控制台与多用户（RBAC）

浏览器打开 `http://127.0.0.1:7870/`。页面顶部可填 API Key（存 localStorage），
之后所有管理操作都会带上它。

**页面提供**：账号池健康度与冷却原因（含倒计时）、剩余积分、模型级冷却明细、
一键全部签到、刷新额度、解除冷却、启停账号、用户增删与 Key 重置。

### 鉴权语义

| 状态 | 行为 |
|---|---|
| 无用户 且 `api_key` 为空 | 单机免密：任何请求视为管理员（零配置即可用） |
| 无用户 且 `api_key` 非空 | 必须带 `api_key`，视为管理员 |
| **有用户** | **禁止匿名**。带用户专属 Key → 按角色授权；带全局 `api_key` → 管理员 |

> ⚠️ **首个用户必须是管理员**。这是一条防自锁规则：一旦存在用户，鉴权就禁止匿名，
> 若此时全局 `api_key` 也为空，而首个用户又不是管理员，则**没有任何人**能再访问
> 管理面（无法建用户、无法重置 Key），只能手工改 `data/users.json`。
> 网关会在创建时直接拒绝并说明原因。

### 多租户隔离

普通用户只能调度 `owner` 为空（public）或等于自己 ID/用户名的账号：

```jsonc
{
  "trae": {
    "accounts": [
      { "product": "trae-cn", "label": "公共号" },                      // owner 缺省 = public，全员可用
      { "product": "trae-cn", "label": "alice 私有", "owner": "alice" } // 只有 alice 能调度
    ]
  }
}
```

管理员不受此限制。会话粘性键也带归属前缀，防止不同用户相同会话 ID 互相绑定。

### 管理端点

```bash
# 用户管理（需管理员）
curl -X POST http://127.0.0.1:7870/admin/users \
  -H "Authorization: Bearer <admin-key>" -H "Content-Type: application/json" \
  -d '{"username":"alice","role":"user","note":"给 Cline 用"}'
# → 返回 {"api_key":"sk-trae-…"}，**仅此一次明文返回**

curl http://127.0.0.1:7870/admin/users -H "Authorization: Bearer <admin-key>"   # 列表（Key 打码）
curl -X POST http://127.0.0.1:7870/admin/users/<id>/reset-key -H "Authorization: Bearer <admin-key>"
curl -X POST http://127.0.0.1:7870/admin/users/<id>/disable  -H "Authorization: Bearer <admin-key>"

# 运维
curl -X POST http://127.0.0.1:7870/admin/checkin            -H "Authorization: Bearer <admin-key>"
curl -X POST http://127.0.0.1:7870/admin/credits/refresh    -H "Authorization: Bearer <admin-key>"
curl -X POST http://127.0.0.1:7870/admin/accounts/<uid>/clear-cooldown -H "Authorization: Bearer <admin-key>"

# 额度（普通用户也可读自己的）
curl http://127.0.0.1:7870/v1/credits -H "Authorization: Bearer <key>"
```

---

## WAF IP 级 fail-fast

当上游按**来源 IP**（而不是按账号）拦截时，逐个换号重试是**反效果**的 ——
每换一个号就多打一次上游，把请求量放大到已被封的 IP 上，只会延长封禁窗口。

### 判定难点：怎么区分"账号问题"和"IP 问题"

单个账号吃 403 有很多原因（该账号被限、该模型无权限、请求不合法），这些都不该触发 IP 级闸。
只有**多个不同账号在同一短窗口内都吃 403**，才说明问题出在共同的那一层 —— 也就是来源 IP。

所以触发条件是「**不同** UID 数」而不是「事件数」：

| 场景 | 判定 | 为什么 |
|---|---|---|
| 同一账号连续 403 10 次 | **不激活** | 那是那个账号的问题 |
| 2 个不同账号各 403 一次（60s 内） | **激活** | 共同的那一层出了问题 |

阈值取 2 而不是 3：3 个太迟 —— 那时已经多打了两次上游，放大效应已经发生。

### 激活后的行为

- **终止本轮轮转**（不再换号），判定放在**取号之前**，否则本轮仍会多打一次；
- **账号冷却照记** —— 闸只负责省下无谓的上游请求，不该掩盖账号本身的问题；
- **403 不冷却账号也不冷却模型**：它既不是账号的问题、也不是模型的问题，
  在池里冷却只会污染状态；
- 状态是**进程内、重启清零**：IP 封禁是短时现象，持久化反而会让重启后带着
  一个过期的"已封禁"判断启动。

```bash
# 查看闸状态（未激活时也给出统计，便于判断"离触发有多近"）
curl http://127.0.0.1:7870/status | jq .waf_ip_gate
# {"active":false,"window_seconds":60,"min_distinct_uids":2,
#  "events_in_window":1,"distinct_uids":1,"activations_total":0}

# 确认限流已过、想立刻恢复轮转而不等窗口自然过期
curl -X POST http://127.0.0.1:7870/admin/waf/reset -H "Authorization: Bearer <admin-key>"
```

---

## System Prompt 改写

很多 OpenAI 兼容客户端不发 system 消息，或发的内容不适合当前上游。网关侧统一处理
可以让同一个上游服务多种客户端，而不必在每个客户端里各配一遍。

```jsonc
{
  "prompt": {
    // passthrough（默认）：完全不动客户端的 system，行为与不接网关时一致
    // custom：替换 —— 丢掉客户端所有 system，只发下面这段
    // append：追加 —— 保留客户端的 system，在末尾接上下面这段
    "mode": "append",
    "text": "你是一个严谨的编程助手。回答前先确认事实，不确定就说不确定。"
  }
}
```

**默认是 passthrough**：网关悄悄改用户的 system 是越界行为，必须显式开启。
配置里写非法模式会**报错而不是静默回落**（静默回落会让用户以为配置生效了）。

### 400 降级重试

若注入的文本触发了上游的内容校验（返回 400），整条请求就会失败 —— 而这与用户的
实际问题无关。此时自动用一段极简提示词（`You are a helpful assistant.`）**重试一次**。

只重试一次，且不按账号重复试（`degradedTried` 是全局标志）。

### 懒发首帧：为什么流式不再提前写 200

原来流式路径是**先写 `HTTP 200` 再调上游**的。这有个隐患：上游随后返回 400/403 时
状态码已经提交，只能塞一个 SSE error 帧 —— 调用方再也无法据此换号重试或降级重试
（HTTP 语义上"已经成功了"）。

现在改成**直到第一帧到达才写响应头**，非 2xx 因此能原样冒泡回上层。

代价是客户端要等上游开始响应才看到字节。为了不让它干等，上游在真正吐内容前发的
元信息帧（`metadata` / `request_wait_in_queue` / `timing_cost`）会被转成 **SSE 注释心跳**
（`: keepalive`，客户端会忽略，但字节到了）：

- 排队位置**绝不进正文**（否则会污染对话历史）；
- 心跳**限流到 2 秒一次**，否则上游这些帧很密，会把流灌满无意义的注释帧。

实测：首字节从 2395ms 提前到 1115ms。

---

## CI/CD

`.github/workflows/ci.yml`（推送 / PR / 手动）：

| 步骤 | 作用 |
|---|---|
| **零依赖校验** | `go.mod` 里出现 `require` 就**报错** —— 「零第三方依赖」是硬约束，不能靠自觉 |
| `go vet` + `gofmt -l` | 格式与静态检查 |
| `go test -race` | **必须带 `-race`**：池/闸/指标都有并发共享状态，普通测试会漏掉数据竞争 |
| 交叉编译矩阵 | linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64 |
| **Docker 构建校验** | 真的 build 一次，并断言容器内 **uid ≠ 0**、`/app/data` 可写 |

`.github/workflows/release.yml`（打 `v*` tag 或手动）：发布前先跑一遍测试
（防止 tag 打在绕过 CI 的提交上）→ 7 平台交叉编译 → 打包 → **SHA256SUMS** → 创建 Release。

本地验证过的交叉编译（7 个目标全部成功）：

```
linux/amd64 8.2MB   linux/arm64 7.6MB   linux/arm 8.1MB
windows/amd64 8.5MB windows/arm64 7.7MB
darwin/amd64 8.4MB  darwin/arm64 7.8MB
```

> ⚠️ 本项目的 CI **尚未在 GitHub 上实际运行过**（本地不是 git 仓库、也没有远程）。
> workflow 文件里的命令我都在本机逐条验证过（交叉编译、`go test -race`、`gofmt`），
> 但 `actions/*` 这些步骤本身没跑过。首次推送后请看一眼 CI 结果。

---

## OAuth 一键登录

不用手动去客户端目录里翻 `storage.json`，也不用在别处导出再导入 ——
直接在控制台点「开始登录」，浏览器里走一遍 SSO 即可。

### 流程（逆向自客户端 Electron 主进程）

```
1. 生成 PKCE（codeVerifier = randomBytes(48).base64url，
              codeChallenge = base64url(sha256(verifier))）
   与 EC P-256 设备密钥对
2. 在 127.0.0.1 起一个随机端口的本地 HTTP 服务
3. 打开浏览器到
   https://www.trae.cn/authorization?
     login_version=1&auth_from=trae|solo&login_channel=native_ide
     &plugin_version=local&auth_type=local&client_id=<ClientID>
     &redirect=0&login_trace_id=<hex>
     &auth_callback_url=http://127.0.0.1:<port>/authorize
     &machine_id=<hex>&device_id=<数字>
     &x_device_id=..&x_machine_id=..&x_app_type=IDE_PC|SOLO_PC&..
     &code_challenge=<S256>&code_challenge_method=S256
     [&hide_saas_login=true  ← 仅 solo]
4. 用户完成 SSO 后浏览器被重定向回本地服务，query 里带
   authCodeInfo（字符串化 JSON，含 AuthCode）与 userInfo
5. POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken
   {ClientID, AuthCode, CodeVerifier, DeviceInfo{...}, IDEVersion}
   → 拿到 Token / RefreshToken / ExpiredAt / UserID
6. 连同**设备私钥**一起落盘成便携快照，并注册进池
```

### 三个容易搞错的地方

1. **登录与续期是同一个端点**（`/trae/api/v3/oauth/ExchangeToken`），只是 body 不同：
   登录用 `AuthCode` + `CodeVerifier`，续期用 `RefreshToken` + `DeviceProof`。
2. **设备密钥对必须在登录时生成并整份保存**。服务端把 `DeviceInfo` 与账号绑定，
   之后每次续期都要用**同一把私钥**签 DeviceProof。只存 token 不存私钥的话，
   token 一过期这份凭据就废了 —— 这也是本项目凭据格式是"便携快照"的根本原因。
3. **PKCE 必须用无填充 base64url**。标准 base64 的 `+` `/` `=` 会被上游拒绝，
   而报错通常很含糊。

### 端点

```bash
# 启动（返回 url，让用户打开）
curl -X POST http://127.0.0.1:7870/admin/login/start \
  -H "Authorization: Bearer <admin-key>" -H "Content-Type: application/json" \
  -d '{"product":"trae-cn","open_browser":true}'
# → {"session_id":"...","url":"https://www.trae.cn/authorization?...","expires_in":600}

# 轮询（**恒返 200**，用 state 字段表达进度）
curl "http://127.0.0.1:7870/admin/login/status?session_id=..." -H "Authorization: Bearer <admin-key>"
# → {"state":"waiting"|"success"|"error"|"unknown","elapsed_sec":3,...}

# 取消
curl -X POST "http://127.0.0.1:7870/admin/login/cancel?session_id=..." -H "Authorization: Bearer <admin-key>"
```

> **为什么轮询恒返 200**：若轮询接口返回 5xx，前端轮询会中断，用户就看不到
> "到底卡在哪一步"了。所以状态一律用字段表达，连"会话不存在"也是 200 + `state=unknown`。
> （这条来自对 workbuddy2api 的复盘。）

> ⚠️ **登录成功后由网关后台自动落盘**，不依赖前端来"确认保存"——
> 前端可能被关掉，但凭据不该因此丢失。

> ⚠️ **诚实说明**：本功能的**浏览器授权那一段没有自动化验证过**（需要真人走 SSO）。
> 已验证的是：URL 参数构造、PKCE 配对、设备密钥可签 DeviceProof、
> 回调处理（用 mock 上游完整走通）、错误路径、状态机、取消、以及真实端点返回的 URL 形状。

---

## 附件展开（文件/文档解析）

很多 OpenAI 兼容客户端只能发纯文本消息，不能像 IDE 那样把工作区文件附进上下文。
本功能让网关侧承担这一步。

### 两种写法

```jsonc
// ① 显式片段（推荐，无歧义）
{"role":"user","content":[
  {"type":"text","text":"这个文件里定义了什么？"},
  {"type":"file","file":{"path":"C:/proj/main.go"}}
]}

// ② 文本里的 @路径（便利写法）
{"role":"user","content":"读 @C:/proj/note.md 然后总结"}
```

`@` 只在**能成功解析到允许根目录内的真实文件**时才替换；解析失败原样保留文本 ——
因为 `@` 在自然语言里也可能是邮箱或提及。这样既方便又不会误伤。

### 支持的类型

| 类型 | 处理 |
|---|---|
| 文本类（约 70 种扩展名：代码 / md / json / yaml / csv / 配置…） | 转成带 `<file path= language=>` 标注的文本片段 |
| 图片（png / jpg / gif / webp / bmp） | 转成 `image_url` 的 data URL |
| 其他（含 PDF） | **明确拒绝**，不做二进制猜测 |

### ⚠️ 安全：默认关闭，且必须配根目录

```jsonc
{
  "files": {
    "enabled": false,               // 默认关闭
    "allowed_roots": ["C:/proj"],   // 唯一的安全边界
    "max_file_bytes": 1048576,
    "max_total_bytes": 4194304,
    "max_files": 16
  }
}
```

从 API 请求里读**任意路径**等于把服务器文件系统开放给调用方 ——
一个 `@~/.ssh/id_rsa` 就能把私钥读走，还会跟着请求发到上游模型。所以：

- **`enabled=true` 但 `allowed_roots` 为空时强制关闭**（fail-closed）。
  配置写漏了必须变成"功能不可用"，而不是"开放整个文件系统"。
- 每个路径都先 `EvalSymlinks` 再校验前缀 —— 否则 `allowed/link -> /etc` 就能逃逸。
  用 `filepath.Rel` 而不是字符串前缀 —— 否则 `/data/allowed-evil` 会被误判为在
  `/data/allowed` 内。
- 内容不是合法 UTF-8 或含 NUL 时拒绝（即使扩展名像文本）。
- 展开失败**不会让请求失败**：读不到的附件变成一条占位文本并记日志，
  让用户知道"为什么没生效"，而不是整条请求 400。

**泄露验证**：用模型不可能知道的随机 canary 放在根目录外，
确认根外引用、`@` 引用、`../` 穿越三条路径都读不到，而根内文件正常可读。

---

## 模型目录与积分倍率

`/v1/models` 每个模型都带完整元信息：

```jsonc
{
  "id": "trae:glm-5.2",
  "multiplier": "2x",              // 积分倍率（fee_model_level）
  "fee_model_level": 2,            // 原值
  "model_type": "reasoning_model",
  "reasoning": true,
  "reasoning_efforts": ["light","high","extra_high"],  // 对应 OpenAI 的 reasoning_effort
  "reasoning_default": "high",
  "context_window": 116000,
  "dev_context": 256000,
  "max_context": 1000000,
  "max_tokens": 64000,
  "max_turn": 70,
  "max_mode": true,
  "multimodal": true,
  "native_tool_calling": true,
  "parallel_tool_calls": false,
  "icon_url": "https://lf-cdn.trae.com.cn/...",
  "is_beta": false, "is_new": true, "is_default": false
}
```

**倍率用 `multiplier` 字符串而不是裸数字**：`fee_model_level=0` 表示"上游未下发"
（自建渠道就是这种情况），显示成 `0x` 会让人以为免费。所以统一渲染成 `—`。
「缺观测」和「显式 0」是两件事 —— 与 `/v1/stats` 里那条纪律同源。

控制台有对应的模型目录表（倍率 / 类型 / 上下文 / 输出上限 / 多模态 / 推理档位 / 工具）。

---

## 可靠性与自愈

这一组能力**用户看不见，但决定稳定性**。它们全部来自"半死连接"与"坏会话历史"
这两类真实故障。

### 连接层加固（`internal/traectl/transport.go`）

TCP 连接可以进入"两端都以为还活着、但数据发不出也收不回"的状态
（NAT 表项过期、对端崩溃未发 RST、中间设备静默丢包）。此时：

| 默认行为的坑 | 后果 | 我们的取值 |
|---|---|---|
| `net.Dialer` **无超时上限** | 半开连接卡到系统 TCP 重传窗口（实测有单次 TTFB 卡 **936 秒**） | `dialTimeout = 10s` |
| keepalive 默认 **2 小时** | NAT 黑洞里死连接被反复复用 | `dialKeepAlive = 15s` |
| **完全没有 TLS 握手超时** | 握手挂起时无任何层兜底 | `tlsHandshakeTimeout = 10s` |
| 空闲连接池保留 90s | 上游常主动掐闲置连接，池里多半已死 | `idleConnTimeout = 30s` |
| 响应头无上限 | 半死连接单请求卡满整体超时 | `responseHeaderTimeout = 60s` |

**⚠️ 关于禁 h2 —— 唯一正确的写法**：

```go
TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper)
```

- 设 `ForceAttemptHTTP2: false` **不管用**：它只对自定义 Dial 生效，
  默认 TLS 栈仍会经 ALPN 协商出 h2。半死的 h2 流被复用后表现为
  `http2: timeout awaiting response headers` —— 排查时完全看不出是 h2 的问题。
- 留 `TLSNextProto` 为 **nil 也是错的**：nil 的含义是"让标准库注入默认 h2 映射"，
  恰好是反效果。
- 必须 `make` 出一个**空映射**，让 ALPN 协商完成后没有 h2 处理器可用。

**失败后清空闲连接池**：失败的连接可能仍留在池里（还没到 `IdleConnTimeout`），
下个请求会继续捡到它 —— 于是"一次网络抖动"变成"连续几次失败"。
所以传输层失败后立刻 `CloseIdleConnections()`，代价是下个请求多一次握手。

### 流中空闲监控（`internal/traectl/idle.go`）

`ResponseHeaderTimeout` **只管到响应头到达为止**。头一到，上游就可以
**永远不吐数据**而不触发任何超时 —— 连接占着、账号租约占着、客户端干等。

所以需要一个**流中**看门狗：只要还在吐数据就续命，静默超过阈值才断流。

- 监控周期 = `idle/4`，钳在 `[10ms, 1s]`
- 计时**从响应头之后**才开始 —— 首字节阶段由 Transport 负责，这里不抢跑
  （否则"上游正在冷启动"会被误判成"流中空闲"）
- 用 `cancel()` 而不是直接 `Close()`：让上层看到的是 `context.Canceled`
  （可归类为"流中空闲超时"），而不是含糊的 "use of closed connection"

**为什么不能用一个整体 `Client.Timeout` 代替**：那会把推理模型"思考几分钟"
这种正常长回复误杀。必须按**静默时长**而不是**总时长**判断。

### 换号退避（`internal/server/backoff.go`）

连续失败往往意味着上游在频控，而频控**按密度判罚** —— 立刻重试只会让请求更密、
加重处罚。所以轮转之间做指数退避（500ms 起、逐次翻倍、封顶 8s）。

**必须加 ±25% 抖动**：多个并发请求若"齐步走"，会在同一时刻一起重试，
以固定周期再次聚成洪峰。

等待是**可取消**的（`select` on `ctx.Done()`）：客户端可能已断开、网关可能在停机，
那种情况下继续睡满退避毫无意义，只会拖慢停机。

### 工具配对自愈（`internal/server/toolpair.go`）

OpenAI 协议要求带 `tool_calls` 的 assistant 消息，其**每一个** id 都要有对应的
`role:"tool"` 结果；反之亦然。缺任一侧，上游 400 拒绝**整个请求**。

坏数据是常态：工具执行失败时，客户端会把 assistant 的 tool_calls 存进历史却
**写不回结果**。这条坏历史被每次请求原样重放 —— 上游对之后**每一条**用户消息
都返回 400，**整条会话报废**。网关是最后一道防线。

**关键设计：两侧共用同一份 `keepCalls` 做对称裁剪。**

曾经（也是很多实现的）写法是"批内全齐才保留整批"。那个写法会留下**无主结果**：

```
批 [c1,c2] 只回了 c1
→ 调用侧整批被删（因为不齐）
→ 而 tool{c1} 仍按 id 命中 keepCalls 得以保留
→ 出站载荷 = 「无 tool_calls 的 assistant + 孤儿 tool」
→ 上游判 11148 → 顶死整条会话
```

正确做法是两侧按**同一个 id 集合**裁剪，任何输入都不会产生半截配对。

还有一步**重排**：有些客户端会在 tool 输出后插一条 developer 消息
（如 `<image_resize_notice>`），并行调用时它插在两份结果中间 → 配对断裂。
重排只调顺序、不改内容：

```
assistant [c00 c01] | tool c00 | X | tool c01
→ assistant [c00 c01] | tool c00 | tool c01 | X
```

⚠️ 重排时必须识别"下一组 assistant.tool_calls 是新的组头"，绝不能当插入物吞掉 ——
否则它自己那批结果永远得不到重排。

### ⚠️ 实测发现的上游约束：assistant 的 content 不能为空

这个约束**不在任何文档里**，是实测出来的：

| assistant 形态 | 上游 |
|---|---|
| 有非空 content | ✅ 200 |
| `content: null` | ❌ 400（业务码 4001 "param is invalid"） |
| `content: ""` | ❌ 400 |
| 无 content 键 | ❌ 400 |
| `content: " "`（一个空格） | ✅ 200 |

而**配对清理恰好会制造出这种空壳**：一条 `content:null + tool_calls` 的 assistant
被裁掉孤儿 tool_calls 后，就成了"既无 tool_calls 又无 content" ——
**自愈反而制造了失败**。

所以 `fixToolPairing` 的第三步是规整：把这种空壳的 content 补成一个空格。
不直接删掉那条消息，是因为删掉会改变消息条数与角色交替结构，
而有些客户端/模型对交替敏感；补空格既满足上游校验，又不引入可见语义。

> 💡 这个 bug 的发现过程值得记：我是**实测探测**各种 assistant 形态才发现的，
> 而不是从代码里读出来的。所以 `fixToolPairing` 的注释里写了"这是实测发现，
> 不是推理出来的"。

### prompt cache key（`internal/traectl/cachekey.go`）

上游支持前缀缓存。实测数据：同一段 8k token 前缀，
不带 key 命中 0 token（费用 ≈ 0.34），带 key 命中 7808 token（费用 ≈ 0.02）——
**降约 17×**。

**⚠️ 必须按账号隔离**：键格式 `t2a-<uid8>-<sha256(uid|conv)[:16]>`。

`uid8` 不是"锦上添花的区分度"，而是**硬隔离因子**：若两个账号用同一个 key，
上游会命中**另一个账号**的前缀缓存 —— 那等于把对方的对话内容喂给了你的请求。

所以它**只能在选号之后生成**（选号前不知道用哪个账号），不能提前放进请求。

### 工具参数截断防护（`internal/server/chat.go`）

SSE 被截断（连接中断 / `finish_reason==length`）时，工具调用的 `arguments`
会只剩半截 JSON。原样发给客户端，客户端 `JSON.parse` 抛错，
很多客户端会因此**卡死整个会话**。

所以工具调用**先缓冲不发出**，流结束时统一校验再发。

判定刻意收窄 —— 只把"**非空但解析不了**"当截断：

| 输入 | 判定 | 理由 |
|---|---|---|
| `""` / 纯空白 | 不是截断 | 合法的"无参工具" |
| `{"city":"北京"}` | 不是截断 | 完整 JSON |
| `null` / `[]` / `123` | 不是截断 | 任何合法 JSON 都算 |
| `{"city":"北` | **是截断** | 半截 JSON |

全部被丢弃时不能还宣称 `finish_reason=tool_calls` ——
那会让客户端等一个永远不来的工具调用，改成 `stop`。

### 请求不合法 ≠ 账号/模型有问题

实测发现：`4001 "param is invalid"`（如上面的空 content）原先落到 default 分支
去**冷却模型**。但这是**客户端请求本身**的问题 —— 同一账号、同一模型服务其他
客户端完全正常。冷却它会把一个健康模型对所有人封掉几分钟。

**这个误伤是我实测时自己撞上的**：连续探测被上一次 400 触发的冷却挡成了 503，
排查时才意识到分类错了。

现在 `isClientRequestError`（HTTP 400 或 code 4001）明确返回**不冷却任何东西**。

> 未实现的一项：`sanitize.go`（提示词脱敏）。它是**模型特定**的
> （参考实现里标注为 deepseek-only 开关），且其字符串字面量在读取时会被
> 工具链归一化改写、无法可靠复刻。Trae 侧是否需要同类处理尚未验证，
> 所以**刻意不做**而不是猜着做。

---

## 图片输入：**当前不可用**（附实测证据）

**结论：Trae 的 chat 接口不接受内联 base64 图片。** 默认配置下收到图片会返回明确的
`400 vision_unavailable`，而不是把请求转上去让上游回一个笼统的业务码 4001。

### 实测过程（不是猜测）

拿真实凭据**直接对上游**试了 11 种出站片段形态：

| # | 形态 | 结果 |
|---|---|---|
| A | 文本 `text` 字段 + `image_url/value/mimeType` | ❌ 4001 |
| B | 文本 `value` 字段 + `image_url/value/mimeType` | ❌ 4001 |
| C | `image_url` + `value/mime_type`（snake_case） | ❌ 4001 |
| D | `image` + `value/mimeType` | ❌ 4001 |
| E | `image_url` 扁平 `mimeType/data` | ❌ 4001 |
| F | `image_url` + `image_url.url`（OpenAI 风格） | ❌ 4001 |
| G | 纯文本对照（`value` 字段） | ❌ 4001 |
| **H** | **纯文本对照（`text` 字段）** | **✅ 接受** |
| I | 只有图片（无文本） | ❌ 4001 |
| J | 图片在前、文本在后 | ❌ 4001 |
| K | `image_url` + `value` + `audience` | ❌ 4001 |
| L | `type=data` + `mimeType` + `data` | ❌ 4001 |
| M | `image_url` + `value`（data 不带前缀） | ❌ 4001 |

图片尺寸**不是**原因（8x8 与 256x256 结果相同）。模型确实声明支持
（目录里 `multimodal: true` 来自真实字段，不是我的推断）。

复现：`TRAE_LIVE_IMAGE=1 go test ./internal/traectl/ -run TestProbeImageShapes -v`

### 客户端是怎么做的

客户端**不内联发图**，而是先上传到对象存储再引用 URI。从 `ai-completion` 扩展里
找到的协议方法名：

```
multimodal/get_resource_upload_token    取上传凭证
multimodal/get_resource_url             由 TOS URI 换可访问 URL
multimodal/check_image_content          校验图片内容
```

即「取 token → 上传 TOS → 拿 URI → 消息里引用该 URI」。这些是**扩展与渲染进程
之间的 IPC 方法名**，对应的 HTTP 端点与请求体在扩展实现里，**我未能可靠还原，
所以没有实现这条链路**。

### 顺带纠正了一个我自己的 bug

文本片段**必须**用 `text` 字段（形态 H 成功）；客户端内部用的 `value` 字段
**会被上游拒绝**（形态 G 失败）。我一度按客户端内部结构把出站文本改成了
`value` —— 实测把它挡了回来。这说明"照抄客户端内部数据结构"是危险的：
内部结构与线上协议之间还有一层序列化。

### 想要图片怎么办

- 用文本描述图片内容（推荐）
- 或设 `trae.vision_enabled: true` 强制放行 —— **通常仍会失败**，仅供你自行试验
- `/status` 的 `vision` 字段会直接告诉你当前状态与原因

---

## 公开门户与管理中枢

| 路径 | 内容 | 鉴权 |
|---|---|---|
| `/` | **公开门户** —— 脱敏的可用性指标、模型清单、一键复制的接入示例 | 无 |
| `/console`（别名 `/dashboard`） | **管理中枢** —— 完整控制台 | `web_password` |
| `/portal/status` | 门户专用的**脱敏**状态 | 无 |

**门户上刻意不出现**：账号 UID/标签、额度具体数值、冷却原因与上游错误原文、
用户列表、任何形式的 API Key。只给**数量**（几个账号就绪）与模型清单
（后者是"怎么接入"的必要信息，不敏感）。

`/portal/status` 也**只返回聚合数字**，不返回任何标识 —— 有测试断言它不含
`uid` / `credits` / 真实 userId。

桌面 GUI 直接打开 `/console`（门户是给"分享给别人看"用的）。

---

## 密码与角色管理

### 改控制台密码

```http
POST /admin/password
{"old_password":"…","new_password":"…"}
```

- 当前**有**密码 → 必须核验 `old_password`
- 当前**无**密码（单机免密）→ 允许直接设置（否则用户永远无法首次设置）
- `new_password` 留空 → 清除密码，恢复单机免密
- 非空 → **至少 6 位**（按**字符**算，中文密码不会因字节数而"莫名够长"）
- 改密成功 → **作废所有会话**（改密的常见动机就是"怀疑会话泄漏"，不作废等于没改）
- 原密码核验失败也会计入登录限流（防止用它做密码爆破）

**密码落盘在 `data/console.json`，不回写 `config.json`** —— 回写会重排用户的字段
顺序、改写格式（用户可能用 JSONC）。启动时该文件**优先于** `config.web_password`，
并且严格区分"文件不存在"（用 config）与"文件里是空串"（用户主动清除了密码）——
混同会导致"用户清了密码，重启后 config 里的旧密码又复活"。

### 改用户角色

```http
POST /admin/users/{id}/role
{"role":"admin"}
```

⚠️ **不能把最后一个管理员降级** —— 否则管理面永久锁死（没人能再建用户、改配置、
重置 Key）。这与"首个用户必须是管理员"是同一条安全规则，只是换了个入口。

---

## 导入后的异步校验

导入只解析并落盘凭据，**不验证**它们是否真能用。所以导入成功后会在后台并发校验
（续期一次 token + 拉一次额度），导入接口立即返回 `check_job_id`：

```jsonc
// POST /admin/accounts/import 的响应
{ "succeed": 1, "check_job_id": "d34e0e14…",
  "check_hint": "已启动后台校验（续期 + 拉额度）；轮询 …" }

// GET /admin/accounts/import/status?job_id=…
{ "state": "done", "total": 1, "done": 1, "ok": 1, "failed": 0, "transient": 0,
  "results": [{ "uid": "trae-cn-916…", "ok": true, "credits": 1190,
                "expired_at": "2026-10-23T…" }] }
```

**关键：区分「凭据问题」与「网络问题」**

| 情况 | `ok` | `transient` | 对用户的含义 |
|---|---|---|---|
| 上游明确拒绝凭据（`RefreshError`） | false | false | 账号废了，该删掉重新登录 |
| 网络异常 / 限流 | false | **true** | 账号是好的，只是这次没验成，重试即可 |

混在一起会让用户**误删好账号**，所以只有 `RefreshError` 才算"凭据问题"。

其他细节：并发度限制 4（20 个并发很容易触发频控，而频控会**污染**校验结果 ——
把"限流"误报成"凭据无效"）；任务结果保留 30 分钟；未知 `job_id` **恒返 200**
（`state: "unknown"`），前端轮询不会因一次 404 中断。

---

## Anthropic / Claude 多模态协议

入站同时接受两套形态：

```jsonc
// OpenAI
{"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}

// Anthropic（Claude 系客户端）
{"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}
{"type":"image","source":{"type":"url","url":"https://…"}}
```

只认 OpenAI 形态的话，Claude 系客户端发的图片会被**静默丢弃** ——
用户看到的是"模型说没收到图片"，而报错里什么都没说。未知片段类型会降级成
`[不支持的片段类型：xxx]` 文本，至少能在对话里看到线索。

（注意：解析支持 ≠ 能发出去。出站仍受上面「图片输入当前不可用」限制。）

---

## Playground：上传文件与粘贴截图

控制台的演练场支持：

- **点击上传**图片 / 文本 / 代码文件（多选）
- **直接 `Ctrl+V` 粘贴截图**（从剪贴板取 `kind: "file"` 的项；纯文本粘贴仍走浏览器默认行为）
- 附件以缩略图/文件名展示，可单个移除
- 有附件时 `content` 自动组装成片段数组；无附件时保持纯字符串（兼容性最好）
- 文本文件按 `<file name="…">…</file>` 内联

---

## 关闭窗口即优雅停机

内嵌 WebView2 模式下关闭窗口 → 网关**优雅停机**（对齐参考实现）。

为什么不是"关窗后继续跑"：用户双击 exe 打开窗口时，心里的模型是"这是个桌面应用" ——
关掉窗口就该退出。继续跑会留下一个**看不见的后台进程**占着端口，下次双击会因
端口占用而失败，而用户完全不知道为什么。

浏览器 App 模式**不**停机（那是独立进程，用户可能只是关掉窗口改用浏览器标签页，
停服务是误伤）。

---

## 选号权重：快过期额度优先

三个因子**相加**（不是相乘 —— 相乘时任一因子为 0 会让账号被完全饿死）：

```
权重 = 1.0 + 快过期占比×8 + 剩余占比×10 + 空闲补偿(≤5)
```

**快过期优先**：优先消耗面临作废风险的额度，而不是均匀轮询到过期为止。
按**占比**算而不是绝对量：同样是"3 天后过期 100 分"，在总额 200 的账号上是 50%
（很急），在总额 10000 的账号上是 1%（不急）。判定窗口 7 天
（太短则绝大多数账号都算出 0，因子失效；太长则失去区分度）。

> 实现取舍：上游权益包只有 `charge_amount`（面额）与 `end_time`，**没有"该包剩余多少"**。
> 所以用的是"面额之和"—— 它是**上界**，但表达"作废风险"的相对紧迫度已经够用。

---

## 国际版 / 国内版（版本域）

Trae 有**两套独立的账号体系**：

| 版本域 | 产品 | 账号体系 | 端点 |
|---|---|---|---|
| **国内版** `cn` | `trae-cn` / `solo-cn` | `*.trae.cn` | `api.trae.cn`、`trae-api-cn.mchost.guru` |
| **国际版** `intl` | `trae-intl` / `solo-intl` | `*.trae.ai` | `ug-normal.trae.ai`、`core-normal.trae.ai`（**已内置默认值**，见下） |

模型名前缀就是版本域：`trae:glm-5.2`（国内）、`trae-intl:glm-5.2`（国际）。

### 为什么是独立 realm 而不是同一个

国内版与国际版的**凭据不能互换、端点也不同**。混在一个域里会让选号把国内账号
派给国际请求（或反过来），表现为**"账号明明健康却总是失败"**，而报错完全指不出根因。

### 配置：**默认就同时支持两个版本，不用配**

```jsonc
{ }   // 什么都不用写
```

国内版与国际版**默认都启用**。网关启动时会把四个产品（`trae-cn` / `solo-cn` /
`trae-intl` / `solo-intl`）都注册上，然后**去各自的数据目录找凭据**：

| 产品 | 找哪个目录 |
|---|---|
| `trae-cn` | `%APPDATA%\Trae CN` |
| `solo-cn` | `%APPDATA%\TRAE SOLO CN` |
| `trae-intl` | `%APPDATA%\Trae` |
| `solo-intl` | `%APPDATA%\TRAE SOLO` |

**装了哪个就自动认哪个**。没装的会打一条 INFO 跳过：

```
INFO: [Trae International] 未检测到客户端（跳过）: C:\Users\x\AppData\Roaming\Trae\User\globalStorage\storage.json
INFO: 渠道 trae 就绪: Trae CN, 1 个账号, 16 个模型, function=chat_v3
```

> **为什么"客户端没装"打 INFO 而不是 WARN**：国际版默认开启后，绝大多数用户
> 只装国内版 —— 这是**完全正常**的状态。若一律打 WARN，每个这样的用户启动时都会
> 看到两条「账号加载失败」，以为网关坏了，然后去排查一个不存在的问题。
>
> 区分方式是 `isClientNotInstalled()`：**路径不存在** = 没装（INFO）；
> 权限问题、JSON 损坏等 = 真问题（WARN）。这里有个 Windows 的坑：
> `ERROR_PATH_NOT_FOUND` 与 `ERROR_FILE_NOT_FOUND` 是两个不同的码，
> 而 `errors.Is(err, fs.ErrNotExist)` **只保证覆盖后者** —— 所以还看了错误文本，
> 否则在 Linux 上测试全绿、到 Windows 上就误报。

四个端点值都已内置（且是**实测**出来的，不是猜的）：

| 配置项 | 内置默认值 | 怎么确定的 |
|---|---|---|
| `intl.ug_host` | `https://ug-normal.trae.ai` | 从国际站 `main.js` 的主机清单取得；判据是"真 API 返回 JSON 错误信封，非 API 返回 404 HTML" |
| `intl.chat_host` | `https://core-normal.trae.ai` | 它对 `/api/agent/v3/llm_utils_chat` 的响应与 CN 的 `trae-api-cn.mchost.guru` **逐字一致** |
| `intl.trae_client_id` | `ono9krqynydwx5` | 差分实测：有效 ID 得到字段级错误 `Invalid param`；假 ID 立刻变成 `Invalid client.` |
| `intl.solo_client_id` | `en1oxy7wnw8j9n` | 同上 |

四项都仍可覆盖 —— 万一上游换域名，改配置即可，不必等新版本。
想只跑单一版本域时，显式设 `{"intl":{"enabled":false}}` 即可（显式配置优先于默认值）。

> **这段的判据比结论重要。** 我最初以为 `api.trae.ai` 就是国际版 API，域名看着完全像。
> 但实测它对 Trae 的 API 路径返回 **nginx 404 HTML**，而真 API 返回
> `{"ResponseMetadata":{"Error":{"Code":"10101",...}}}` ——
> **`api.trae.ai` 根本不是 Trae 的 API 主机。**
> 当时因为够不到国际站，只能如实写"必须你自己填"；现在从国际站拿到了主机清单、
> 逐个验过判据，才敢给默认值。
>
> 记判据而不只记结论，是因为**下次换域名时判据还能用**。

登录页主机**不需要单独配** —— 从 `ug_host` 派生（`api.X` → `www.X`，
这条规则在国内版上可验证）。`www.trae.ai/authorization` 实测可达（HTTP 200）。

### 境外节点上该用哪个版本

| 你的出口 | 国内版 `trae-cn` | 国际版 `trae-intl` |
|---|---|---|
| 国内 IP | ✅ `www.trae.cn` 可达 | 视网络而定 |
| **境外 IP** | ❌ **按区域封锁** —— `www.trae.cn` 由 volc-dcdn 返回 403「抱歉，您当前区域不支持访问」 | ✅ `www.trae.ai` 可达 |

⚠️ **这个失败模式极具误导性**：网关读凭据、续期、调模型全都正常（那些走 `api.trae.cn`，
**不受**区域封锁影响），**只有登录页打不开**。

所以本项目加了**启动前预检**：点「OAuth 一键登录」时先探登录页，区域被封就立刻返回
可照做的错误（含"在代理里为 `trae.cn` 加 DIRECT 规则"或"改用国际版"的指引），
而不是让你对着浏览器的「无法访问此页面」**干等 10 分钟超时**。

判定策略是**保守优先**：只对"明确的区域封锁特征"和"连不上"报错；
其它状态码（404/500/429…）一律放行 —— **误报比漏报更糟**，
那会让你去改一个本来没问题的代理配置。


### 导入时自动识别版本域

对齐参考实现"按域后缀判 realm"的做法：**以凭据自己带的 `host` 为准**。

导入一份国际版客户端的 `storage.json` 时，即使调用方把它当国内版提交，
网关也会依据凭据里的 host（`*.trae.ai`）自动登记成国际版产品 ——
否则后续所有请求都会打到国内上游。

### 跨版本域兜底

请求的域**完全没有渠道**时（没配国际版、或没导入该域账号），退到配对的另一个域：

```
trae-intl 无渠道 → 兜底到 trae（记 WARN）
trae 无渠道      → 兜底到 trae-intl（记 WARN）
```

两条边界：

- **只在"该域一个渠道都没有"时兜底**。若该域有渠道但账号都在冷却，兜底会把请求
  悄悄打到另一套账号体系上（计费与额度都不同），而用户以为用的还是原来的域。
- **绝不跨 trae↔solo 兜底**。两者的模型清单与 `function` 都不同，
  把 `trae` 的请求落到 `solo` 的池上会得到完全不同的模型。

### 自查

`/status` 的 `editions` 字段直接告诉你有没生效：

```jsonc
"editions": {
  "registered": { "cn": ["trae-cn","solo-cn"], "intl": ["trae-intl","solo-intl"] },
  "active_channels": { "trae": 1, "solo": 1 },   // 有渠道=有可用账号
  "intl_enabled": true
}
```

"国际版配了但没生效"是最难自查的问题之一（配置写错时只是**不注册**，
没有任何报错），所以显式暴露出来。

---

## 安全响应头与登录防爆破

### 全站安全响应头

**所有**响应都带（加在最外层中间件，不是逐个 handler 里设 —— 逐个设必然会漏）：

| 头 | 值 | 防什么 |
|---|---|---|
| `X-Content-Type-Options` | `nosniff` | 禁止浏览器按内容嗅探类型。没有它，一个被当作 `text/plain` 返回的 JSON 可能被当成 HTML 执行（存储型 XSS 的常见入口） |
| `X-Frame-Options` | `SAMEORIGIN` | 禁止别的站点 iframe 嵌入控制台。没有它可被点击劫持 —— 对"一键解除冷却""删除账号"这类按钮很危险 |
| `Referrer-Policy` | `no-referrer` | 控制台 URL 可能带 `session_id`，不该随外链泄漏 |
| `Content-Security-Policy` | `default-src 'self'; …; frame-ancestors 'self'` | 禁外部脚本/样式（控制台是自包含的），允许 `data:` 图片（粘贴截图与模型图标） |

> 我原先**只有导出端点**设了 `nosniff`，没有全局 —— 这正是"逐个 handler 里设"的必然结果。

### 登录防暴力破解

按**来源 IP** 限流：5 分钟内失败 5 次 → 锁定 5 分钟。

```
第 1~4 次输错 → HTTP 401  密码不正确
第 5 次输错   → HTTP 429  尝试次数过多，请在 5 分钟后再试
被锁后即使密码正确 → HTTP 429（防止"再试一次就中了"）
```

用 **429 而不是 401**：让用户能区分"密码错了"与"我被锁了"，
而不是反复重试一个永远不会成功的密码。

**为什么按 IP 而不是全局**：全局计数会被一个攻击者用来**锁死所有人**（拒绝服务）；
按 IP 只影响攻击者自己。

**⚠️ 代理头的处理（安全关键）**：

```jsonc
{ "trust_proxy_headers": false }   // 默认 false
```

`X-Forwarded-For` / `X-Real-IP` **只在显式开启时才采信**。若默认采信，
攻击者每次换一个假 IP 就能**完全绕过限流**（等于没有限流）。
关着的时候，反向代理后的所有请求会算作同一个 IP（限流偏严）——
宁可偏严，也不要"看起来有限流、实际等于没有"。

**⚠️ 已知局限**（写在代码注释里，不藏着）：

- 这是**进程内**状态，多实例部署时各算各的（要全局一致需要 Redis）
- 攻击者换 IP 即可绕过；它挡的是**低成本脚本爆破**，不是有组织的攻击 ——
  后者要靠强密码与网络层防护
- 密码长度要求 ≥6 位（按**字符**算，中文密码不会因字节数而"莫名够长"）；
  留空恢复单机免密

`/status` 的 `login_limit` 字段暴露限流状态（便于确认它真的在工作）。

---

## 控制台密码

```jsonc
{ "web_password": "你的密码" }   // 空 = 免密
```

设置后访问 `http://127.0.0.1:7870/` 会先要求输入密码。

**它保护什么、不保护什么**：

| | 由谁负责 |
|---|---|
| 控制台**页面本身** | `web_password`（挡掉随手打开 UI 的人） |
| **API 端点** | API Key / RBAC（见 `withAuth`） |

两者**正交**：设了密码后，用 curl 调 `/v1/chat/completions` 仍然只需要 API Key。
密码闸不改变任何 API 的鉴权行为。

安全细节：密码用 `subtle.ConstantTimeCompare` 比较（`==` 会因提前返回而泄漏时序，
攻击者能逐字节猜出密码）；会话是 32 字节随机 token 存进程内（12 小时 TTL，
重启即失效），放进 **HttpOnly** Cookie（禁止 JS 读取，降低 XSS 窃取风险）。

---

## 模型名校验

```jsonc
{ "trae": { "strict_models": true } }   // 默认 true
```

**默认严格**：模型名必须存在于本地目录，否则返回 400 并**列出可用模型**。

为什么默认严格：实测上游对**未知模型名**会**静默回退**到某个默认模型并返回
HTTP 200。于是用户把模型名打错一个字，拿到的是**另一个模型**的回答，
而且照样消耗额度 —— 这种静默错配比一个明确的 400 糟糕得多。

代价：上游新增模型而客户端还没刷新目录时请求会被拒。此时设 `strict_models: false`
即可透传（会记 WARN）。

---

## 并发排队

客户端发并行请求（多标签、批量脚本）超过账号在途上限时，网关会**排队等待**
而不是立刻 503。

```
并发 6 个请求（在途上限 3）→ 成功 6/6（耗时 17.7s，含排队）
```

**关键：只在"值得等"时才等。** `Pick` 返回 nil 有两种原因，必须区分：

| 原因 | 行为 |
|---|---|
| 账号健康、只是名额满了 | 几秒内就会释放 → **排队等**（上限 20s） |
| 账号都在冷却/熔断/停用 | 可能要等几十分钟 → **不等**，直接给出明确原因 |

不做这个区分的话：要么在"全池冷却 24 小时"时每个请求白等 20 秒（把小问题
放大成雪崩），要么在"只是并发突发"时直接给客户端 503（客户端会以为网关坏了）。

在途上限是"避免把单个账号打爆"的保护，**不是"拒绝服务"**。

---

## 两级降级

"降级"在本项目里是**两个独立的机制**，解决两类不同的问题。

### 1. 提示词降级闸（`internal/server/degrade.go`）

上游的内容策略会拦截某些 system 提示词（多为指纹误报）。一旦命中就返回 400；
若每次都"先撞 400 再用中性提示词重试"，那**每个请求都要白挨一次失败** ——
既慢又浪费额度。

所以加一道闸：**一旦确认被拦，就直接切到中性提示词，直到次日零点**。
降级期内的请求不再先撞 400。

两条关键语义：

- **降级期内重复触发不续期** —— 保持"最早触发点的零点重置"。
  否则连续误报会让降级期无限延长，变成"永远在用降级提示词"，
  而用户完全不知道为什么自己的 system 提示词失效了。
- **用 `time.FixedZone("CST", 8*3600)` 而不是 `LoadLocation("Asia/Shanghai")`** ——
  容器与精简发行版常缺 tzdata，`LoadLocation` 会失败并**静默回落到 UTC**，
  于是"次日零点"算错 8 小时，降级期在用户看来是**随机结束**的。

`/status` 暴露 `prompt_degrade` 状态，`POST /admin/degrade/reset` 手动解除。

### 2. 连败降权（`internal/pool` 的 `NoteFailures`）

这一条**补上了一个设计缺口**。

本项目的原则是"未分类错误不罚号"—— 因为罚号的前提是能把它归类到明确的
账号级问题。但那留下一个洞：

> **一个持续坏掉、但每次都报"未分类错误"的账号，永远不会被移出池**，
> 只会被反复选中、反复失败。

所以加一个**连败兜底**：单次不罚，但连续 N 次（默认 5）都是"说不清原因的失败"
就临时出池一段时间（默认 5 分钟）。既守住"不因一次抖动罚号"，又能在持续坏掉时
真正止损。

**只喂"不知道原因的失败"**（未分类错误 + 传输层失败）。
带权威分类的错误（401/403/429/5xx/请求不合法）各有"知道原因的惩罚"，
再喂连败是**重复计罚**。

三条实现细节：

- **降权与冷却/熔断并列取更长者，不叠加** —— `healthyLocked` 里是或门，
  任一未到期即不可选，生效的自然是三者中最远的那个。
- **降权期内再次达阈不延长、不翻倍** —— 否则用户的重试会把降权越堆越厚，
  最终账号被无限期排除。
- **降权不参与"兜底候选"** —— 兜底的语义是"软冷却可能已恢复，值得一试"，
  而降权是**明确的"出池休息"**。若允许它兜底，连败兜底就架空了。

状态跨重启保留（不持久化的话，重启会让坏账号立刻回池，连败兜底形同虚设），
`acct list` 与 `/status` 都会显示降权与连败计数。

---

## 命令行工具

除 `server` 外还有 6 个工具，都是**独立二进制**（可只拷需要的那个）：

| 工具 | 用途 | 依赖网关运行 |
|---|---|---|
| `acct` | 账号池管理（列表 / 解除冷却 / 启停 / 删除 / 快照） | 是 |
| `credit` | 额度查询（`-refresh` 强制实时） | 是 |
| `models` | 模型目录（可直接粘进客户端的 ID 列表） | 是 |
| `signin` | 触发签到（幂等） | 是 |
| `login` | OAuth 一键登录 | 是 |
| `stats` | **TUI 指标仪表盘** | 是 |

```bash
# 网关地址与 Key 可用环境变量一次配好
export TRAE2API_URL=http://127.0.0.1:7870
export TRAE2API_KEY=sk-trae-...

trae2api-acct list                    # 账号与状态
trae2api-acct clear trae-cn-916...    # 解除冷却
trae2api-models -name glm             # 找模型
trae2api-credit -refresh              # 实时额度
trae2api-signin                       # 签到
trae2api-login -product solo-cn       # 登录新账号
trae2api-stats                        # 交互式仪表盘（q 退出，r 刷新）
trae2api-stats -once                  # 只渲染一帧（便于写日志）
```

### 退出码约定

| 码 | 含义 | 脚本里的处置 |
|---|---|---|
| 0 | 成功 | — |
| 1 | 运行期失败（网络、上游拒绝） | 可以重试 |
| 2 | **用法错误**（参数缺失/非法） | 重试无用，要改命令 |

把 2 单独分出来是有意的：`if cmd; then` 只能区分成败，而"参数写错了"和
"运行失败了"需要不同的处置。

### 两条显示纪律

**1. CJK 宽度自己算。** `len()` 与 `utf8.RuneCountInString` 都按 1 列算中文，
用它对齐表格只要出现中文就整列错位。所以 `internal/cli` 自实现了宽度计算。

**2. 表格单元格里不用 emoji。** emoji 的终端显示宽度是**实现相关**的
（有的渲染 2 列、有的 1 列、有的因字体回退而不定），无法在运行时探测，
猜错就整张表错位。所以表格里只用确定宽度的中文（2 列）与 ASCII（1 列）；
emoji 只出现在非表格的散文式输出里。

同理**分隔线只用 ASCII**（`-` `|` `+`）：制表符（`U+2500` 系）的
East Asian Width 是 Ambiguous，中文终端常按 2 列渲染，会与按 1 列算的内容对不上。

### `stats` 仪表盘的三层防御渲染

1. **帧裁进窗口** —— 行内容按列宽截断（宽字符不切半）
2. **裁完仍放不下 → 退回滚动模式**（不清屏、不定位光标）——
   这样内容自然向上滚，用户能滚动回看，而不是看到被切一半的表
3. **尺寸变化 → 清屏重建基线** —— 否则旧内容残留在右侧/下方

还有两条：

- **清屏用 `ED(0)` 而非 `ED(2)`**：Windows conhost 收到 `2J` 会把屏幕内容
  **滚入 scrollback**，重绘几次就叠好几份表（用户缩放窗口后向上滚会看到）。
- **非 TTY 时只渲染一帧**：被重定向到文件时不能进交互循环，否则脚本永远挂住。

仪表盘把"无观测"渲染成 `—` 而不是 `0` —— 与 `/v1/stats` 的哨兵语义一致。

---

## 桌面 GUI

```bash
trae2api serve -gui                 # 开一个原生窗口显示控制台
trae2api serve -gui -gui-browser    # 强制用浏览器 App 模式（跳过 WebView2）
```

### 三级降级

| 级别 | 条件 | 表现 |
|---|---|---|
| 1 | 有 `WebView2Loader.dll` + WebView2 运行时 | **内嵌 WebView2 窗口**（真原生，控制台在同进程内渲染） |
| 2 | 没有 Loader，但有 Edge/Chrome | 浏览器 `--app=` 模式（无地址栏、无标签页，外观接近原生） |
| 3 | 都没有 | 日志打印 URL，让用户手动打开 |

### 关于「零第三方依赖」

主流的 Go WebView2 方案是 `github.com/jchv/go-webview2`，它依赖
`go-winloader` 与 `golang.org/x/sys` 三个模块。

但**它要解决的 `WebView2Loader.dll` 加载问题，在纯 stdlib 下同样能解决**
（`syscall.NewLazyDLL`），剩下的只是 COM vtable 调用。所以本项目**自己写了绑定**
（`internal/desktop/desktop_windows.go`，约 600 行 syscall），
把「零第三方依赖」这条硬约束继续守住。

代价是要自己处理 COM 的三个坑，见下。

### COM 互操作的三个坑（都是我实际踩到的）

1. **接口指针指向的是"指向 vtable 的指针"**。所以实现一个 COM 对象时，
   结构体的**第一个字段**必须是 vtable 指针，而传给 COM 的是 `&结构体.第一个字段`。

2. **⚠️ `Invoke` 传进来的接口指针是"借用"的，必须自己 `AddRef`**。
   这是我第一版的 bug：日志显示"窗口已就绪"、WebView2 的 profile 目录也建了
   （139 个文件 / 4.7MB，含 `ShaderCache`），但**浏览器进程随即退出、窗口空白**。
   原因就是没 AddRef，回调返回后环境被释放。
   **这个 bug 从日志完全看不出来** —— 每一步的 HRESULT 都是成功。
   靠"进程列表里没有我们的 msedgewebview2" + "没有子窗口" 才发现。

3. **`syscall.NewCallback` 包出来的回调必须保持可达**，否则 GC 回收后
   COM 侧调用会跳到已释放内存上（表现为随机崩溃）。见 `comKeepAlive`。

### `WebView2Loader.dll` 从哪来

**它不由 WebView2 运行时提供**（实测：运行时目录里的 `EmbeddedBrowserWebView.dll`
并不导出 `CreateCoreWebView2EnvironmentWithOptions`）。它是 WebView2 SDK 的一部分，
需要随应用分发。查找顺序：

1. 环境变量 `TRAE2API_WEBVIEW2_LOADER` 指定的路径
2. **与本 exe 同目录**（推荐：把 DLL 拷过来即可）
3. 当前工作目录
4. 系统搜索路径（PATH / 系统目录）

> ⚠️ 这个 DLL 是 Microsoft 的原生二进制，本项目**不打包**它（既不该也没有合适的分发方式）。
> 没有它时自动降级到浏览器 App 模式，功能不受影响 —— 只是窗口外观差一点。

### 验证情况

在 Windows 上实际启动验证过（`-gui`）：

```
✅ 日志：桌面窗口已就绪（内嵌 WebView2）
✅ 6 个 msedgewebview2 进程属于本实例（按 --user-data-dir 匹配）
✅ 到 7871 端口有 Established 连接（页面确实在加载）
✅ 主窗口子窗口链：
     Chrome_WidgetWin_0            1180x820
     Chrome_WidgetWin_1            1180x820
     Chrome_RenderWidgetHostHWND   1180x820   ← 渲染器
     Intermediate D3D Window       1180x820   ← GPU 合成
✅ 窗口类名 Trae2ApiDesktopWindow、标题「Trae2Api 控制台」、visible=True
```

> ⚠️ **无法验证的部分**：我**看不到屏幕**，所以"画面内容是否正确显示"是靠
> 上述间接证据推断的（渲染器子窗口 + GPU 合成窗口 + 真实 TCP 连接）。
> 像素级效果请你自己看一眼。

### `go vet` 的例外

`desktop` 包的 COM 互操作必然出现 `uintptr` ↔ `unsafe.Pointer` 转换
（原生代码经回调把裸指针以 `uintptr` 交给我们），vet 的 `unsafeptr` 分析器
对这类代码有已知误报。CI 里**不关闭整个分析器**，而是精确放行：

- 任何**其它** vet 报告 → 失败
- `unsafeptr` 报告出现在 `desktop` 包**之外** → 也失败

---

## Docker 部署

> ⚠️ **诚实声明：以下 Docker 配置未经构建验证。** 开发本项目的机器上没有安装 Docker，
> 因此 `docker build` / `docker compose up` **从未实际执行过**。
> 已经做的只有离线检查（见文末清单）。首次部署时请按清单验证，如有报错请对照修正。

### 前提：准备凭据目录

容器里没有 `%APPDATA%`，自动探测必然失败，因此**必须**显式挂载并配置路径。
把 Trae 客户端的 `globalStorage` 目录复制到项目下：

```
secrets/
├── trae-cn/          ← 来自 %APPDATA%\Trae CN\User\globalStorage\
│   ├── storage.json
│   └── state.vscdb
└── solo-cn/          ← 来自 %APPDATA%\TRAE SOLO CN\User\globalStorage\
    ├── storage.json
    └── state.vscdb
```

> 挂**整个目录**而不是只挂 `storage.json`：模型目录 `state.vscdb` 是它的兄弟文件，
> 网关从凭据路径的**同级**推导它（`traectl.StateDBPathBeside`）。

### 启动

```bash
docker compose up -d --build
docker compose logs -f --tail=100
curl http://127.0.0.1:17870/healthz
docker compose down
```

对外端口默认 `17870`（刻意避开 `8080`），可覆盖：`APP_PORT=18080 docker compose up -d`。

### 四个容易踩的点

1. **凭据目录必须可写**（compose 里**不能**加 `:ro`）。续期会轮换 refreshToken，
   网关必须把新 token 写回 `storage.json`，否则容器重启后就拿旧 refreshToken 去换、必然失败。

2. **⚠️ 绑定挂载会盖掉镜像里的属主（Linux 宿主上必踩）。**
   镜像里 `RUN mkdir -p /app /trae && chown 10001:10001` 建立的属主，在
   `- ./secrets/trae-cn:/trae/trae-cn` 这类**绑定挂载**下**全部失效** ——
   挂载后目录属主变成宿主目录的 UID（通常是 1000），而容器内进程以 10001 运行。
   于是续期写回时会 `EACCES`，而且**失败发生在几小时后第一次续期时**，
   报错看不出根因。

   ```bash
   # Linux 宿主：一次性修正属主
   sudo chown -R 10001:10001 ./secrets/trae-cn ./secrets/solo-cn
   ```

   > Windows / macOS 的 Docker Desktop 里绑定挂载权限是模拟的，通常不受影响。
   >
   > 为了不让这个问题以模糊的方式出现，**程序在启动时会主动探测凭据目录可写性**
   > （真的创建再删除一个临时文件，而不是只看权限位），不可写时直接打印出上面那条
   > `chown` 命令。`/app/data` 用的是**命名卷**，首次创建时会继承镜像里的属主，
   > 所以那一侧不需要对齐。

3. **`listen` 必须是 `0.0.0.0:7870`**。容器配置 `docker/config.docker.json` 已设好；
   若沿用默认的 `127.0.0.1`，容器内是通的，但**外部永远访问不到**。

4. **不要给 compose 加 `container_name`**。固定名字会关掉 Compose 的项目名前缀，
   同机从两个目录部署就会撞名。

### 离线已检查项（这些我验过）

| 检查 | 结果 |
|---|---|
| `Dockerfile` / `.dockerignore` / `docker-compose.yml` / `config.docker.json` 行尾 | ✅ 全部 LF（CR=0），`.gitattributes` 已钉住 |
| `.dockerignore` 是否排掉本地产物 | ✅ `*.exe`、`run/`、`data/`、`config.json`、`.git`、`*.log` |
| compose 缩进层级 | ✅ `build`/`image`/`ports`/`environment`/`volumes`/`restart`/`extra_hosts` 同级 |
| 是否设了 `container_name` | ✅ 未设（自动加项目前缀） |
| 端口映射方向 | ✅ `17870:7870`（对外:容器内），且容器内监听 7870 |
| 依赖是否引入 | ✅ `go.mod` 无任何 require |

### 未验证项（请在部署机上跑）

```bash
docker build --progress=plain -t trae2api:verify .
docker run --rm trae2api:verify id -u                                  # 期望：10001（非 0）
docker run --rm trae2api:verify sh -c 'touch /app/data/.w && rm /app/data/.w'
docker compose config                                                  # 校验语法与变量插值
docker compose up -d && docker compose logs --tail=50
curl -fsS http://127.0.0.1:17870/healthz
docker compose down
```

若基础镜像里已存在 10001 号用户（本项目的 `alpine:3.20` 里没有），
构建会报 `adduser: uid '10001' is in use` —— 改 `--build-arg APP_UID=<空闲号>` 即可。

---

## 已知限制

> 这张表是**如实**的状态，不是宣传。标 ❌ 的就是没做，标 ⚠️ 的就是有坑。

| 项 | 现状 |
|---|---|
| **图片输入** | ❌ **不可用**。上游 chat 接口不接受内联图片 —— 实测 11 种出站片段形态**全部**被业务码 4001 拒绝，仅纯文本可通。默认配置下收到图片会返回明确的 `400 vision_unavailable`，而不是让上游回一个语焉不详的 4001。详见上文「图片输入」一节。 |
| **同邮箱跨渠道隔离** | ❌ **做不到**。参考实现按"Google / Twitter 登录渠道"分流保存，但**Trae 的凭据里根本没有渠道字段**：`Credentials` 只有 `token / refreshToken / expiredAt / refreshExpiredAt / tokenReleaseAt / userId / host`；在本机真实 `state.vscdb` 上检索 `logintype` / `authtype` / `thirdparty` / `twitter` 全部 0 命中。现存唯一的"渠道"维度是 `Product.Key`（`trae-cn` / `solo-cn` / `trae-intl` / `solo-intl`），已并入 `AccountUID`。**不编一个字段名假装做了。** |
| **Redis 跨实例状态** | ❌ 未实现。状态是**进程内 + 落盘**，多实例部署时各算各的（冷却、额度、会话粘性都不共享）。 |
| **自动更新（web_update）** | ❌ 未实现。 |
| **跨域 failover** | ❌ 未实现。`trae` 与 `solo` 是两个**模型池**（不同 function、不同清单），语义上不是同一模型的备份，所以不做自动互备。国内版 ↔ 国际版之间**有**兜底（该域一个渠道都没有时），但那是"部署事实"级兜底，不是故障转移。 |
| **Docker** | ⚠️ 已提供 Dockerfile / compose / 容器配置，但**未经构建验证**（本机无 Docker）。已做的是离线可验的部分：Alpine 方言正确、UID 撞号容忍、数字 ID、构建期属主自检、`.dockerignore` 行尾 LF。**未验证的是真实构建与运行** —— 见「Docker 部署」。 |
| **`go test -race`** | ⚠️ 本机无 gcc，**未经本地验证**；CI 里跑。 |
| **桌面 GUI 像素** | ⚠️ 无屏幕访问，**未经视觉验证**；已验证的是窗口类名、标题、尺寸、子窗口链与 WebView2 进程数。 |
| **OAuth 浏览器授权腿** | ⚠️ 需要人工 SSO，**未自动验证**；已验证的是 PKCE 生成、本地回调服务、轮询恒返 200、取消路径。 |
| **每账号模型目录** | ⚠️ 模型目录按**渠道**取（多账号时用第一个账号的）。若不同账号的服务端清单不同，请求可能落到不含该模型的账号上 —— 不过**默认开启了严格模型校验**，未知模型名会直接 400 而不是静默回退。 |
| **`-journal`** | ⚠️ `internal/vscdb` 不解析 SQLite 热日志；WAL 校验失败时回退主库（与 SQLite 行为一致）。 |
| **非 Windows 的 dsh 防孤儿** | ⚠️ **无此加固**。Unix 的进程组需要有人**主动** `kill(-pgid)`，父进程被 SIGKILL 时同样没机会运行，语义与 Job Object **不等价**。与其写一个"看起来有、实际不生效"的实现（比没有更危险），不如明确说明。优雅退出（SIGTERM/SIGINT）时两个平台都会正常清理 dsh。 |
| **`sanitize.go`（提示词脱敏）** | ❌ 刻意未实现。参考实现那版是模型专用的，来源不可靠，照抄会引入错误行为。 |

### 已实现（对照参考实现的缺口清单）

| 项 | 现状 |
|---|---|
| 单一可执行文件 | ✅ 已收拢：`cmd/` 下只有 `trae2api/`，7 个子命令全在一个二进制里。 |
| 多用户 / RBAC | ✅ 用户表 + 专属 API Key + admin/user 角色 + owner 归属隔离。首个用户强制为管理员（防自锁）；不能降级最后一个管理员。 |
| 控制台鉴权 | ✅ **公开门户 `/` 与管理中枢 `/console` 分离**：门户只透出脱敏可用性（无 UID / 额度 / 用户数），管理中枢由 `web_password` 保护（常数时间比较 + HttpOnly 会话 + 按 IP 限流防爆破）。 |
| 凭证导入/导出 | ✅ 粘贴/上传 `.json`·`.txt`·`.zip`、兼容客户端 `storage.json` 原文、C 注释与全角引号清洗、逐条结果回显、ZIP 导出。**导入后异步校验**：续期 + 拉额度，区分「凭据问题」与「网络问题」（后者标 `transient`，避免用户误删好账号）。 |
| 签到 / 额度 | ✅ 每日定时签到（默认 9/21 点，幂等）+ 额度查询（`/v1/credits` 实时、`/status` 缓存）。 |
| 指标 / `/v1/stats` | ✅ global / by_model / by_account / by_realm / by_day 聚合，含 TTFB、耗时、token、缓存命中率与积分消耗。**加了保留窗口**（参考实现那版是无窗口 map，会无限增长）。 |
| Playground 演练场 | ✅ 选模型发请求（流式/非流式/思维链）+ **点选上传图片与代码文件** + **`Ctrl+V` 粘贴截图**。 |
| WAF IP 级拦截 | ✅ 60s 窗内**不同**账号达 2 个即激活，激活期终止轮转、账号冷却照记；`/status` 暴露闸状态，`POST /admin/waf/reset` 手动解除。 |
| System Prompt 改写 | ✅ passthrough / custom / append；400 时用极简提示词降级重试一次。 |
| OAuth 一键登录 | ✅ PKCE(S256) + 本地回调服务 + 浏览器授权 + AuthCode 换 token；轮询接口恒返 200；成功后自动落盘并注册进池。 |
| 文档/文件解析 | ✅ `file` 片段与 `@路径` 两种写法，文本类与图片，**根目录白名单 + 软链解析 + 大小限制**（默认关闭）。**PDF 已支持**（零依赖自实现：对象表 + FlateDecode + `Tj`/`TJ` + ToUnicode CMap）。 |
| 模型积分倍率展示 | ✅ 解析上游 `fee_model_level`，`/v1/models` 透出 `multiplier`，控制台有模型目录表。另补齐推理档位、图标、真实上下文窗口等字段。 |
| CLI 工具 / TUI | ✅ `acct` `credit` `models` `signin` `login` `stats`(TUI) —— **全部是同一个二进制的子命令**，统一退出码 0/1/2，CJK 宽度自实现。 |
| 桌面 GUI（WebView2） | ✅ **自写的纯 stdlib COM 绑定**（零 Go 依赖），内嵌 WebView2 原生窗口；三级降级（WebView2 → 浏览器 App 模式 → 打印 URL）；**关窗即优雅停机**（仅内嵌模式）。 |
| dsh 内嵌 | ✅ **Agent (dsh) 标签页**：控制台一键启停 DeepSeek Harness，iframe 内嵌其网页 UI；`/v1/models` 里出现 `dsh` 虚拟模型（域 `agent`）。**动态端口**（监听 `:0`）、**幂等启停**、**优雅终止**（TERM → 超时 KILL）、网关退出时回收；**Windows 上用 Job Object 防孤儿** —— 网关被 `Stop-Process -Force` 强杀（TerminateProcess，无清理机会）时 dsh 也会被 OS 连带杀掉（非 Windows 平台无此加固，见下）。**不做**的是"把对话转发给 dsh agent" —— 需要它的 HTTP API 契约，无法从本机确认，**不猜**。 |
| 登录防爆破 + 安全响应头 | ✅ 按来源 IP 限流（5 分钟 5 次 → 锁 5 分钟；**代理头默认不采信**）；全站 `nosniff` / `SAMEORIGIN` / `no-referrer` / CSP，加在**最外层**中间件。 |


---

## 凭证导入与导出

### 为什么需要"便携快照"

Trae 的凭据不是简单的 token + refreshToken —— 续期要用 **ECDSA P-256 设备私钥**
签名（DeviceProof），而这个私钥和 token 放在一起（`iCubeAuthInfo://icube-dc:<数字>`）。
**少了私钥，refreshToken 拿到也没用**（换不出新 token，token 一过期这份凭据就废了）。

所以本项目定义了一份自包含的「便携快照」：

```jsonc
{
  "version": 1,
  "product": "trae-cn",          // 或 solo-cn
  "label": "主号",
  "userId": "916348159159136",
  "token": "eyJhbGciOi…",         // 访问令牌
  "refreshToken": "…",            // 轮换语义：每次续期都会变
  "expiredAt": "2026-10-23T05:10:11.618Z",
  "refreshExpiredAt": "2027-04-06T…",
  "host": "https://api.trae.cn",
  "deviceId": "3366058269681403", // 续期 DeviceInfo.DeviceID 用它
  "machineId": "690ad7a9…",       // 续期 DeviceInfo.MachineID 用它
  "privateKeyPEM": "-----BEGIN PRIVATE KEY-----…",  // DeviceProof 签名用
  "publicKeyPEM": "-----BEGIN PUBLIC KEY-----…"
}
```

有了它，网关可以**完全脱离 Trae 客户端**运行：把快照拷到别的机器即可。
（实测验证：只凭一份导出的快照就能完成真实续期，见测试 `TestSnapshotLiveRefresh`。）

### 导入支持的形态

`POST /admin/accounts/import` 接受三种提交方式：JSON body `{"text":"…"}`、
`multipart/form-data` 上传文件、或直接 POST 原始文本。

内容形态自动识别：

| 形态 | 例子 |
|---|---|
| 单个快照 | `{"token":"…","refreshToken":"…","privateKeyPEM":"…"}` |
| 快照数组 | `[{…},{…}]` |
| 包装结构 | `{"accounts":[…]}` / `{"data":[…]}` / `{"tokens":[…]}` / `{"list":[…]}` |
| 客户端 storage.json 原文 | 含 `iCubeAuthInfo://icube.cloudide` 加密键（网关自动解密转换） |
| ZIP 压缩包 | 内存解压，取其中的 `.json`/`.txt`，跳过 `__MACOSX` 与 `._*` |
| 卡密行 | `token----refreshToken` / `token:refreshToken`（**会被明确拒绝**，见下） |

清洗容忍：UTF-8 BOM、CRLF、全角引号/逗号/冒号、`//` 与 `/* */` 注释、尾随逗号、
Markdown 代码块围栏。**刻意不处理字符串内部的 `//`**（会破坏 URL）。

### 两个刻意的设计取舍

1. **卡密行（只有 token+refreshToken）会被拒绝**，并说明原因：
   *"缺少 privateKeyPEM（没有它无法生成 DeviceProof，token 过期后无法续期）"*。
   收下一份"过几小时就失效"的凭据，比拒绝更糟。
2. **导入不做同步连通性校验**。校验要打上游，会让导入耗时取决于网络且消耗额度。
   有效性由首次真实请求暴露，或在控制台点「刷新额度」时验证。
   （对照：workbuddy2api 的导入是同步校验的，本项目刻意不这样做。）

### 端点

```bash
# 导入（需管理员）
curl -X POST http://127.0.0.1:7870/admin/accounts/import \
  -H "Authorization: Bearer <admin-key>" -H "Content-Type: application/json" \
  -d '{"text":"{\"product\":\"trae-cn\",\"token\":\"…\"}","product":"trae-cn"}'

# 上传文件（含 ZIP）
curl -X POST http://127.0.0.1:7870/admin/accounts/import \
  -H "Authorization: Bearer <admin-key>" -F "files=@accounts.zip" -F "product=trae-cn"

# 列出已导入的快照
curl http://127.0.0.1:7870/admin/accounts -H "Authorization: Bearer <admin-key>"

# 导出全部（ZIP）
curl -OJ http://127.0.0.1:7870/admin/accounts/export -H "Authorization: Bearer <admin-key>"

# 删除（同时从池中移除）
curl -X DELETE http://127.0.0.1:7870/admin/accounts/trae-cn-916348159159136.json \
  -H "Authorization: Bearer <admin-key>"
```

> ⚠️ **导出 ZIP 内含 refreshToken 与设备私钥，等同于账号的长期凭证。**
> 因此导出端点要求管理员权限，并设置 `Cache-Control: no-store`。
> （对照：workbuddy2api 的 `/ui/dsh/export` 不校验角色就返回 `api_key` —— 这个坑不抄。）

---

## 指标与 `/v1/stats`

```bash
curl http://127.0.0.1:7870/v1/stats -H "Authorization: Bearer <key>"
curl -X POST http://127.0.0.1:7870/v1/stats/reset -H "Authorization: Bearer <admin-key>"
```

返回 `global` / `by_model` / `by_account` / `by_realm` / `by_day` 五组聚合。

### 三条决定"统计可不可信"的纪律

这三条来自对 workbuddy2api 的复盘。它们的共同点是：**做错了不会报错，只会让数字悄悄骗人**。

**1. 「缺观测」与「显式 0」必须分离。**

上游不一定每次都给 usage（错误响应、流中断、上游省略 `token_usage` 事件）。
若把"没拿到"当成 0 记入均值，看板会显示"tokens 骤降"这种假警报。

所以返回结构里有 `usage_count`（**有观测**的请求数）与哨兵 `no_observation_sentinel: -1`：

```jsonc
{
  "requests": 10,
  "usage_count": 5,            // ← 只有 5 次拿到了 usage
  "avg_prompt_tokens": 100,    // ← 分母是 5 而不是 10（否则会被稀释成 50）
  "avg_ttfb_ms": -1,           // ← 哨兵：无观测（非流式请求没有首字节概念）
  "cache_hit_rate_percent": 80
}
```

控制台把哨兵渲染成「—」并在旁边标注*"「—」表示该项无观测（上游未返回），不是 0"*。

> 我在实现这个包时**自己就在这里犯了一次**：`AvgCredits` 的 Go 零值是 `0` 而不是哨兵，
> 于是"无观测"看起来像"观测到 0 积分"。被单元测试 `TestNoObservationSentinel` 抓出来。

**2. 缓存命中率的分母排除 `cacheWrite`。**

命中率是 `hit / (hit + miss)`。把"写入缓存"也算进分母，会让第一次请求永远显示未命中，长期低估。

**3. TTFB 均值的分母是 `ttfbCount`，不是 `requests`。**

非流式请求没有"首字节"概念。拿总请求数当分母会把 TTFB 均值稀释成无意义的数。
实现上 TTFB 只在第一个**真实增量**到达时记录 —— 刻意不用"网关发出 HTTP 200"作为起点，
那衡量的是网关自己的动作，而 TTFB 要衡量**上游**多快开始吐。

### 保留窗口（本项目对 workbuddy2api 的改进）

workbuddy2api 的 `byDay` 是一个**无窗口、无 TTL、无裁剪**的 map，长期运行会无限增长
（它自己的代码注释也标注了"每次全量序列化，会变慢"）。

本项目加了 `metrics.retain_days`（默认 14），并在**落盘与恢复两侧**都做惰性剔除 ——
只在一侧做的话，一份很久以前的快照会把早已过期的日分桶带回来，让窗口形同虚设。

注意：**全局聚合不受窗口影响**（它是累计值），窗口只裁 `by_day` 分桶。

### 持久化

`data/metrics.json`，每 2 分钟落盘一次（`dirty` 判断：无新请求时不产生磁盘 IO）。

与 `state.json` **分开存**：指标是**可丢的观测数据**，用户想重置统计时应当能单独删它，
而不影响账号池的冷却/熔断状态。

落盘用 `tmp+rename` 原子替换，但**刻意不做 fsync** ——
为"可丢的观测数据"付 fsync 的代价不值得；而账号凭据那种"丢了要重登"的数据才需要。

### ⚠️ 未知域前缀会被明确拒绝

上游对未知**模型名**会**静默回退**到某个默认模型并返回 200（实测：
传 `trae:no-such-model` 也返回 200，内容是默认模型的回答）。于是把域写错
（如 `trae2:glm-5.2`）会被当成模型名发出去 —— 用户以为调用了 X、实际拿到 Y，
还照样消耗额度。

真实 Trae 模型名里没有冒号（自定义渠道用 `provider//model` 形式，用的是斜杠），
所以本项目对"含冒号且前缀不是已知域"的模型名**直接返回 400** 并列出可用前缀：

```json
{"error":{"code":"unknown_realm",
  "message":"模型名 \"trae2:glm-5.2\" 的前缀 \"trae2\" 不是已知的域。可用前缀：trae / solo；也可直接写裸模型名（默认走 trae）"}}
```

---

## 项目结构

```
trae2api/
├── cmd/trae2api/          唯一程序入口（单文件多子命令分发）
├── internal/cmd/          各子命令的实现（被 cmd/trae2api 分发）`n├── docker/
│   └── config.docker.json 容器专用配置（listen 0.0.0.0 + 显式 storage_path）
├── Dockerfile             多阶段构建（golang:alpine → alpine，非 root 10001）
├── docker-compose.yml     编排（端口/卷/健康检查）
├── .dockerignore / .gitattributes
├── internal/
│   ├── realm/             域枚举 + 模型名前缀解析
│   ├── config/            配置加载与校验
│   ├── pool/              账号池：选号 / 分级冷却 / 熔断 / 并发租约 / 状态落盘
│   ├── session/           会话粘性 + 会话键提取
│   ├── usermgr/           多用户与角色（API Key / RBAC / owner 归属）
│   ├── traectl/           Trae 协议层
│   │   ├── crypto.go        tc 信封加解密
│   │   ├── creds.go         凭据存储读写
│   │   ├── refresh.go       ECDSA DeviceProof 续期
│   │   ├── chat.go          chat 客户端（SSE + 工具调用）
│   │   ├── credits.go       签到与额度
│   │   └── models.go        模型目录
│   ├── vscdb/             零依赖只读 SQLite 读取器
│   └── server/            HTTP 层 + 控制台 + 后台任务
│       ├── server.go        路由 / 鉴权（RBAC）/ /status
│       ├── chat.go          OpenAI ↔ Trae 转换 + 错误分级
│       ├── channel.go       渠道与账号构造
│       ├── admin.go         运维端点（签到/额度/冷却/启停）
│       ├── users.go         用户管理端点
│       ├── tasks.go         后台任务（额度刷新 + 每日签到）
│       └── console.go       内置控制台页面
└── run/                   运行目录（配置 / 二进制 / 端到端脚本）
```

## 测试

```bash
go test ./... -timeout 300s
go vet ./...
```

覆盖范围：

| 包 | 重点 |
|---|---|
| `traectl` | 真实凭据解密（两套产品）、ECDSA 签名验签与 DER 编码校验、canonical 串格式、SSE 全部陷阱（T1-T4 + finish_reason 改写）、工具参数序列化、签到幂等与额度解析、UA 回归防护、便携快照往返与跨格式解析清洗 |
| `vscdb` | 与 `node:sqlite` 真值 SHA-256 对拍、overflow page 边界、WAL 只读、损坏输入不 panic |
| `pool` | 选号按域/归属过滤、软冷却可兜底而硬冷却不可、**模型级冷却不参与兜底**、并发租约、熔断阈值、软冷却指数退避、状态落盘往返 |
| `session` | 绑定/解绑/TTL 续期、会话键提取（含嵌套 metadata）、开场白派生键的稳定性 |
| `usermgr` | 首个用户必须是管理员、不能删/禁用最后一个管理员、Key 重置使旧 Key 失效、落盘往返、Key 打码、列表顺序稳定 |
| `accountstore` | 同账号覆盖而非堆积、跨产品同 userId 不撞、坏文件只跳过、路径穿越防护、文件名净化、落盘内容可独立解析 |
| `metrics` | **缺观测与显式 0 分离**、哨兵语义、命中率分母排除 cacheWrite、TTFB 分母用 ttfbCount、保留窗口双向剔除、落盘往返 |
| `server` | 鉴权四种状态组合、角色授权（403）、owner 归属集合、错误→分级冷却映射、**未知域前缀拒绝**、**路由注册不冲突** |
| `config` | 默认值可用、缺失字段前向兼容、非法值报错、账号产品校验 |

部分测试在缺少 Trae 客户端时自动 `Skip`，因此在 CI 上也能通过。

### 单元测试与复盘抓出来的真 bug

这些都是**跑起来或对照复盘才暴露**的，不是读代码能看出来的 —— 记在这里以说明为什么值得写测试：

**第一轮（本会话较早）：**

1. **`usermgr` 自死锁**：`Create` 持有写锁后调用 `Save()`，而 `Save()` 去拿读锁。
   `sync.RWMutex` 不可重入 → 测试直接挂住。修法：拆出 `saveLocked()`。
2. **模型级冷却形同虚设**：被模型级冷却的账号仍会通过「兜底候选」被选中，
   因为兜底只排除了硬冷却。修法：兜底也必须排除模型级冷却。
3. **免密模式下管理端点全 403**：`withAuth` 在匿名放行时没注入身份，
   `isAdmin()` 恒为 false。修法：注入匿名管理员。
4. **用户列表顺序不稳定**：`CreatedAt` 只到秒，同一秒创建的用户并列时顺序取决于
   map 迭代。修法：加 ID 作为最终 tiebreaker。
5. **路由注册 panic**（启动时才炸）：Go 1.22+ ServeMux 拒绝 `GET /` 与无方法限定的
   `/admin/users` 共存。修法：按方法分别注册，并补 `TestHandlerRoutesRegister` 把它
   提前到测试阶段。

**第二轮（对照 workbuddy2api 复盘后修正）：**

6. **传输层失败被罚号**（对齐它的明确规则后修正）：DNS/连接/TLS/超时是**本地网络问题**，
   不是账号问题。原实现把它们记成账号失败并冷却 —— 一次断网会把整个健康池罚一遍。
   修法：新增 `traectl.TransportError` 类型，上层据此**跳过冷却处置**；未分类错误同样
   不罚号（要罚必须先能归类到明确的账号级问题）。
7. **选号权重是乘性公式**：`idle × success × credit`。某个因子为 0（余额耗尽、
   成功率样本为 0）会把整个权重清零，账号被**彻底饿死**。修法：改为**加性**
   `1.0 + creditFactor + idleFactor`，最差情况仍保留基准权重。
   同时把成功率从权重里**移除**（失败由熔断器与健康过滤负责，在权重里再罚一次是重复惩罚）。
8. **兜底选号用随机**：全部候选都在冷却时随机挑，会反复挑中同一个号，让它陷入
   "刚失败→立刻又被选中"的循环。修法：改用**单调序号 LRU**（`usedSeq`），保证轮着试。
9. **额度权重用绝对值而非占比**：1000/2000 与 1000/100000 的账号被等价看待。
   修法：记总量，按**剩余占比**给分。
10. **全角引号替换的启发式局限**（被一个写错的测试用例暴露）：替换是全局的，
    若全角引号出现在字符串**内容**里，清洗后 JSON 会不合法。这是刻意取舍
    （针对"中文输入法把定界符打成全角"这一高频场景），已在代码注释与测试里写明。

**第三轮（做指标与 Playground 时）：**

11. **我在指标包里犯了正要防的那个错**：`AvgCredits` 的 Go 零值是 `0`，
    而 `0` 是"真实观测到积分为 0"的合法值 —— 不显式初始化成哨兵，
    "无观测"就看起来像"观测到 0"。被 `TestNoObservationSentinel` 抓出来。
    这恰好证明了那条纪律（缺观测 ≠ 0）**很容易被违反，哪怕是在专门防它的人手里**。
12. **未知域前缀被静默透传**（E2E 测出来的）：`nosuchrealm:x` 返回了 HTTP 200。
    因为上游对未知模型名会静默回退到默认模型 —— 用户以为调了 X、实际拿到 Y，
    还照样付费。修法：含冒号且前缀不是已知域 → 400 并列出可用前缀。
13. **控制台字段名笔误**：JS 里写 `avg_comp_tokens`，而 JSON 字段是
    `avg_completion_tokens` → 控制台该列会显示 `undefined`。
    这类错误 TypeScript 能挡，纯 JS + 手写 HTML 挡不住 —— 靠 E2E 脚本读真实响应才发现。

**第四轮（WAF 闸 / Prompt 改写 / CI 时）：**

14. **`prompt.Rewriter.Apply` 在 nil receiver 上解引用**：`Apply` 直接访问 `r.mode`，
    而 nil 指针没有字段 → panic。`Enabled()` 我判了 nil，`Apply` 忘了。
    被 `TestPassthroughIsNoop`（它故意传 nil）抓出来。修法：`Apply` 也判 nil 并原样返回，
    这样调用方不必先判空 —— 少一处 `if x != nil` 就少一处漏判的机会。
15. **流式提前写 200 让降级重试不可能**：原来 `streamChat` 一进来就 `WriteHeader(200)`，
    上游随后返回 400 时状态码已提交，只能塞 SSE error 帧。改成**懒发首帧**后
    非 2xx 才能原样冒泡（详见「System Prompt 改写」一节）。
16. **`.dockerignore` 漏排 `dist/`**：我新加了交叉编译产物目录（7 个二进制约 58MB），
    但没同步更新排除规则 —— 每次 `docker build` 都会把这 58MB 传进构建上下文。
    这类"新增了产物目录但忘了排除"的疏漏不会报错，只让构建悄悄变慢。
17. **Docker 构建期自检写得自欺**：`test -w /app/data` 是在 `USER` 之前以 **root** 跑的，
    而 root 对任何目录都可写 —— 这条检查恒真，证明不了任何事。
    真正有意义的是 `stat -c '%u:%g'` 的**属主断言**。已改为只保留有意义的检查，
    并在注释里写明每条各能证明什么。
**第十二轮（对照 README 补齐）：**

41. **只做了国内版**（见第十一轮）之外，对照参考实现的 README 逐项核对，发现 14 处差距。
    本轮补了 9 处，其中**两处是安全问题**（不是"功能多少"）：
42. **完全没有登录防暴力破解**。`web_password` 可以被无限次尝试 —— 弱密码几分钟内
    就被爆破。补上按 IP 限流（5 分钟 5 次 → 锁 5 分钟），并**默认不采信**
    `X-Forwarded-For`（采信的话攻击者每次换个假 IP 就完全绕过限流）。
43. **没有全站安全响应头**。只有导出端点设了 `nosniff`，其他地方全裸 ——
    浏览器会按内容嗅探类型（JSON 可能被当 HTML 执行），页面可被任意站点 iframe 嵌入
    （点击劫持，对"解除冷却""删除账号"这类按钮很危险）。
    修法：加在**最外层中间件**。逐个 handler 里设必然会漏 —— 我自己就漏了。
44. **图片支持从来就没工作过**（本轮最重要的发现）。起因是给"Anthropic 多模态格式"
    写端到端测试，结果**三种格式全部 4001** —— 包括我从第一天就有的 OpenAI 形态。
    于是拿真实凭据直接对上游试了 **11 种**出站片段形态：
    **全部 4001，只有纯文本能通**。尺寸不是原因（8x8 与 256x256 一样）。
    结论：Trae 的 chat 接口**不接受内联 base64 图片**，客户端走的是
    「取上传凭证 → 上传对象存储 → 引用 URI」的链路（从 ai-completion 扩展里
    找到 `multimodal/get_resource_upload_token` 等方法名），我未能可靠还原该链路。
    **修法不是"修好它"，而是让它明确失败**：返回 `400 vision_unavailable` +
    原因 + 替代方案，而不是转上去让上游回一个笼统的 4001
    —— 后者会让用户完全看不出问题在图片上，而这个功能的失败模式是
    "看起来请求发出去了"。
45. **照抄客户端内部结构是危险的**。同一轮实测里发现：文本片段**必须**用 `text`
    字段，而客户端内部用的 `value` 字段**会被上游拒绝**。我一度按客户端内部结构
    把出站文本改成 `value`，实测把它挡了回来。教训：内部数据结构与线上协议之间
    还有一层序列化，**只有实测能确认线上形态**。
46. **关窗后网关继续跑**：用户双击 exe 打开窗口，心里的模型是"这是个桌面应用" ——
    关掉窗口就该退出。继续跑会留下**看不见的后台进程**占着端口，下次双击因端口
    占用失败而用户不知道为什么。改成内嵌模式关窗即优雅停机
    （浏览器 App 模式不停 —— 那可能只是去用标签页，停服务是误伤）。
47. **门户与管理面没分离**：原来 `/` 直接是完整控制台，任何人打开就能看到账号 UID、
    额度、冷却原因、用户列表 —— 即使设了密码，登录页也把"这里有个管理面"告诉了所有人；
    而且想给同事一个"服务是否正常 + 怎么接入"的页面时，只能把整个管理控制台给出去。
48. **导入不验证凭据**：导入 20 个账号后，只有等第一次真实请求失败才知道哪几个是坏的。
    补上后台并发校验，并**区分「凭据问题」与「网络问题」** —— 混在一起会让用户
    **误删好账号**（限流导致的失败看起来和失效一模一样）。并发度限 4：20 个并发很容易
    触发频控，而频控会污染校验结果。
49. **不能改密码、不能改角色**：用户只能改配置文件重启。补上运行时改密
    （核验原密码 + 作废所有会话 + 落盘到 `data/console.json` 而**不回写**
    `config.json` —— 回写会重排用户的字段顺序、改写格式）。
    改角色时**不能降级最后一个管理员** —— 否则管理面永久锁死，
    与"首个用户必须是管理员"是同一条规则的另一个入口。

**第十一轮（国际版 / 国内版）：**

39. **只做了国内版**：Trae 有国内（`*.trae.cn`）与国际（`*.trae.ai`）**两套独立账号体系**，
    我原先只实现了国内版。参考实现把这件事做成了**一等公民**（realm = cn/global，
    按凭据 domain 后缀判定），而我把它漏了。
40. **差点"猜"国际版端点**。我实测确认了几件事，避免了一个会长期潜伏的错：
    - `api.trae.ai/trae/api/v3/oauth/ExchangeToken` → **nginx 404 HTML**
    - `api.trae.cn` 与 `api.trae.com.cn` → **400 + JSON 错误信封**（真 API）
    即 **`api.trae.ai` 不是 Trae 的 API 主机**。国际版的真实端点在**国际版客户端**
    的 product.json 里，本机只装了国内版，无从推导。
    如果我按"域名模式看着像"就填了 `api.trae.ai` 当默认值，用户会看到
    **账号健康、每个请求都失败、错误信息指不出根因** —— 这类 bug 最难查。
    最终做法：**不提供默认值**，启动时打印一条指出"从哪取值"的 WARN，
    并把四个配置项列清楚。

**第十轮（自查与修缺）：**

> 这一轮的起因是用户反馈"还是有很多问题"。我没有再堆功能，而是**自己去找问题** ——
> 结果找到的比预想的多，而且第一个就是我自己造的。

35. **实现了功能但忘了接线**（最严重的一个）：我给 `traelctl.Client` 加了
    `IdleTimeout` 字段、实现了流中空闲监控、还写了完整的单元测试 ——
    但**构造 Client 时忘了赋值**。于是整套监控是**死代码**，而所有测试都是绿的
    （它们直接构造 Client，不走真实构造路径）。
    更隐蔽的是：当时的 `cmd/server/main.go`（现已收拢进 `internal/cmd/server/`）里确实有一行 `IdleTimeout: 120 * time.Second`，
    但那是 **`http.Server` 的 keep-alive 配置**，与它毫无关系 —— 我一度以为自己接上了。
    修法：抽出 `newTraeClient` 作为**唯一构造点**，并加
    `TestNewTraeClientWiring`（走真实构造路径）+ `TestNoStrayClientConstruction`
    （源码扫描，禁止其他地方直接构造 `traelctl.Client`）。
    **教训：字段加了不算实现，接线了才算。而"看起来接上了"最危险。**
36. **声明了却从没被读过的安全配置**：`web_password` 在 config 里存在、注释写着
    "控制台密码；空 = 免密"，但**代码里从来没读过它**。用户设了密码以为控制台受保护，
    实际毫无作用 —— 一个"看起来是安全控制"的死配置，比没有这个配置**危险得多**。
    修法：真正实现密码闸（常数时间比较 + HttpOnly 会话 Cookie）。
    **教训：审计"声明但未使用"的配置项，尤其是安全相关的。**
37. **并发超过在途上限就 503**：客户端发 4 个并行请求（很常见）时，前 3 个占满名额，
    第 4 个直接收到 503 —— 它会理解成"网关坏了"。而并发突发本该**排队**。
    修法：有界排队，且只在"账号健康、只是名额满"时才等（全池冷却时等也没用）。
38. **未知模型名静默回退**：`trae:no-such-model-xyz` 返回 HTTP 200，
    内容是**另一个模型**的回答，而且照样计费。我原先只打了 WARN 就放过。
    修法：默认严格校验（400 + 列出可用清单），可用 `strict_models: false` 关掉。

**第九轮（两级降级时）：**

33. **把独立的状态嵌进了别人的条件块**（同一个错误犯了三次）：给降权加字段时，
    我连续三次把新代码放进了 `if !a.breakerUntil.IsZero() { ... }` 里面 ——
    包括 `DegradeRemain` 的计算、`DegradeUntil` 的落盘。
    症状很隐蔽：**只有熔断时才显示/保存降权**，而降权恰恰是熔断未触发时才会发生的，
    所以生产上永远看不到。被单元测试逐条抓出来。
    教训：往既有代码里插新逻辑时，**先看清插入点的括号层级**，别按文本相似度找锚点。
34. **兜底分支漏了新状态**：`Pick` 的兜底候选原本只排除硬冷却与模型级冷却。
    我加了连败降权后忘了同步 —— 于是被降权的账号仍会通过兜底被选中，
    **连败兜底被架空**。测试抓出来的。这类"新状态要同步到所有判断点"的疏漏，
    靠代码审查很难发现，靠测试很直接。

**第八轮（可靠性七件套时）：**

29. **给结构体加 `sync.Once` 引发拷贝锁**：为了让 HTTP 客户端只构造一次，
    我给 `traectl.Client` 加了 `sync.Once`。但 `Channel.ClientFor` 的写法是
    **拷贝**一份再改 `Store` —— `go vet` 立刻报 `assignment copies lock value`。
    这不只是 vet 洁癖：拷贝 `sync.Once` 是**真的数据竞争**，且每个副本会各自
    建一个 Transport。而所有账号打的是同一个上游主机，连接池**本就该共享**。
    修法：把 HTTP 客户端移到包级，与 `Client` 实例解耦。
    **教训：往被拷贝的结构体里加带状态的字段前，先确认它没有被拷贝。**
30. **上游要求 assistant 的 content 非空白**（实测发现，文档里没有）：
    `null` / `""` / 缺失 → 400（业务码 4001）；`" "` → 200。
    而我的工具配对清理**恰好会制造出这种空壳** —— 自愈反而制造了失败。
    修法：清理后加一步规整，把空壳 content 补成一个空格。
31. **`isBlankContent` 的语义写错导致规整不幂等**：我最初把"纯空白字符串"也判为
    空白，但 `" "` 正是规整的**修复值** —— 于是每次都被判定为"有改动"、反复施加。
    被单元测试抓出来。修法：把"空白"的语义收窄为「**上游会拒收的那些形态**」，
    而不是"视觉上没内容"。**同一个概念在不同语境下语义不同，名字要跟着变。**
32. **`4001` 被误分类成"模型级冷却"**：`param is invalid` 是**客户端请求**的问题，
    同一账号/模型服务其他客户端完全正常。冷却它会把健康模型对所有人封几分钟。
    这个误伤是我**实测时自己撞上**的：连续探测被上一次 400 的冷却挡成了 503，
    排查时才意识到分类错了。

**第七轮（CLI 工具 / TUI 时）：**

27. **接口契约的跨进程失配**：服务端 `/v1/stats` 的 `pool.servable` 返回 **bool**，
    而当时的 `cmd/stats`（现已收拢进 `internal/cmd/stats/`）里声明成 `int` → JSON 解析失败，**整个仪表盘打不开**。
    这类错误 Go 的类型系统在跨进程边界上完全帮不上忙。
    修法：补 `servable_count`（数量）并让两边类型一致，同时加
    `TestStatsResponseShape` **逐字段断言 JSON 类型**（不只是断言字段存在）。
28. **emoji 宽度不可知**：我原本在表格里用 ✅/—/⛔，但 emoji 的终端显示宽度是
    实现相关的（有的 2 列、有的 1 列、有的因字体回退不定），无法运行时探测，
    猜错就整张表错位。修法：**表格单元格里只用确定宽度的中文与 ASCII**，
    emoji 只留在非表格的散文式输出里。这条是从"写测试时发现 `RuneWidth('✅')`
    到底该是几"反推出来的 —— 答案是"我不该需要知道"。

**第六轮（桌面 GUI 时）：**

25. **COM `Invoke` 的接口指针必须自己 AddRef**（最隐蔽的一个）。我漏了这一步，
    结果：日志显示"窗口已就绪"、**每一步 HRESULT 都是成功**、WebView2 的 profile
    目录也建了（139 个文件 / 4.7MB，含 `ShaderCache`）—— 但**浏览器进程随即退出、
    窗口空白**。因为借用指针在回调返回后被释放，环境跟着销毁。
    **从日志完全看不出来**，是靠"进程列表里没有我们的 msedgewebview2"
    +"枚举窗口没有子窗口" 才定位到。修法：`comCall(env, 1)` / `comCall(ctrl, 1)`。
26. **`go vet` 会连带分析依赖**，所以按包名过滤（`go vet $(go list ./... | grep -v desktop)`）
   没用 —— 任何 import desktop 的包都会把那些报告带出来。改成"全量 vet +
    精确放行 desktop 包里的 unsafeptr 类报告"，其余一律失败。

**第五轮（倍率 / 附件 / OAuth 时）：**

19. **同层两个同 JSON tag 的字段 → Go 静默丢弃两者**。我在 `rawModel` 里放了
    一个 `Multimodal bool \`json:"multimodal"\`` 和一个 `MultimodalCfg struct{...} \`json:"multimodal"\``
    （想同时兼容"布尔"与"对象"两种形态）。`encoding/json` 判定同层同名冲突后
    **把两个字段都丢掉** —— 不报错、不警告，表现为"所有模型突然都不支持多模态了"。
    修法：自定义 `multimodalField` 类型，一个字段吃两种形态。
    **这个坑从现象几乎无法反推**，只能靠类型层面避免。
20. **改了切片却没写回 map**。`expandFileRefs` 里我修改了 `msgs`（`[]map[string]json.RawMessage`），
    但没执行 `obj["messages"] = ...`，于是 `json.Marshal(obj)` 序列化的还是原始内容 ——
    表现为"统计说有 N 个文件，但请求体一个字没变"。被单元测试抓出来。
21. **`json.Marshal` 默认转义 HTML 字符**：`<` `>` `&` 会变成 `\u003c` 之类。
    对文件内容（HTML/XML/Markdown）来说这既撑大体积又让人无法在日志里读。
    修法：`Encoder` + `SetEscapeHTML(false)`。
22. **跳过原因被丢弃**：`expandAtRefs` 在"一个都没替换成功"时直接返回，
    把 `Skipped` 也一起丢了 —— 于是用户写 `@x.png` 没有任何反应，也不知道为什么。
    修法：无论是否替换成功都把 stats 带出去。
23. **配置不容忍 UTF-8 BOM**。Windows 记事本、PowerShell 5.1 的
    `Set-Content -Encoding UTF8` 都会写 BOM，而 Go 报的是
    `invalid character '\ufeff' looking for beginning of value` ——
    完全看不出是编辑器加的字节。修法：`stripBOM`（一行 TrimPrefix 换掉一整类求助）。
24. **我引入了一个语义重复的字段**：为了兼容两种 `multimodal` 形态，我新加了
    `MultimodalEnable` 却**忘了给旧的 `Multimodal` 赋值**，导致后者恒 false。
    语义重复的字段就该合并 —— 合并成一个 `Multimodal` 后这类错就不可能发生。

18. **绑定挂载盖掉镜像属主（compose 的真问题）**：`./secrets/trae-cn:/trae/trae-cn`
    会让镜像里 `chown 10001` 的效果失效，Linux 宿主上续期写回时 `EACCES`，
    且失败拖到几小时后才出现。修法：启动时主动探测可写性并打印 `chown` 命令。

---

## 合规声明

本项目仅供**个人学习与研究**，用于访问你自己已订阅的 Trae 额度。
请遵守 Trae 的服务条款。所有请求仅在你的本机与上游官方服务器之间直连，
不经过任何第三方中继。

## 许可

MIT
