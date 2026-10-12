package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"trae2api/internal/traectl"
)

// 本文件实现控制台账号池页的**批量动作**端点（冻结契约 A）：
//
//	POST /admin/accounts/revive   清五样（冷却/熔断/连败/降权/模型级冷却）
//	POST /admin/accounts/control  批量启用/停用
//	POST /admin/accounts/delete   批量删除快照文件
//
// ## 为什么是"批量优先"而不是让前端循环调单账号端点
//
// 单账号端点（/{uid}/clear-cooldown、/{uid}/enable）已经存在，前端完全可以
// for 循环调用。但这个页面有「全部复活」按钮 —— 池里几十个账号时那是几十次
// HTTP + 几十次 pool.Save()（每次都要重写整个 state.json）。批量端点让这件事
// 变成一次请求、一次落盘。
//
// ## 为什么 delete 收 uid 但返回 errors[]
//
// 契约把它定义为**部分成功**语义：删掉能删的，失败的逐条回报。
// 屏幕上勾了 5 个账号，其中 2 个只存在于 config.trae.accounts（没有快照文件），
// 那么"3 成功 2 失败 + 原因"远好于整批 500。"账号不存在"**不算失败的原因**：
// 目标状态（这个账号没了）已经达成，当成成功并计入 uids 更符合批量删除的直觉
// —— 详见 handleAccountsDelete 里的说明。

// accountsBatchRequest 是三个批量端点的公共请求体。
//
// Uids 为空且 All=false 时一律 400 而不是"删了 0 个返回 200"：
// 后者会让前端把"请求没带上参数"渲染成"操作成功"。
type accountsBatchRequest struct {
	Uids []string `json:"uids"`
	All  bool     `json:"all"`
	// Action 只被 control 使用：enable | disable。
	Action string `json:"action"`
}

// uids 返回本次请求实际作用的账号清单。
//
// all=true 时忽略 uids 并以池内顺序全量返回（排序保证响应确定性：
// 控制台会把这些 uid 逐个渲染，顺序抖动会让表格闪）。
func (s *Server) batchTargets(req accountsBatchRequest) ([]string, error) {
	if req.All {
		out := make([]string, 0, 16)
		for _, st := range s.pool.List() {
			out = append(out, st.UID)
		}
		sort.Strings(out)
		return out, nil
	}
	out := make([]string, 0, len(req.Uids))
	seen := make(map[string]bool, len(req.Uids))
	for _, uid := range req.Uids {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, uid)
	}
	sort.Strings(out)
	return out, nil
}

// decodeBatchBody 解析 JSON 请求体。
//
// 空 body 视为全零值（而不是报错）：`all:false` + 空 uids 会被后续的
// 空目标检查拦下并给出可读的 400，比 JSON 语法错误更好懂。
func decodeBatchBody(r *http.Request, dst *accountsBatchRequest) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

// handleAccountsRevive POST /admin/accounts/revive —— 批量复活。
//
// 契约：body {"uids":[...],"all":false} → 200 {"revived":N,"uids":[...]}
//
// "复活"= 让这个账号重新**有资格被选中**，所以要清的口径与 pool 的选号判据
// （healthyLocked / HealthyIgnoringInflight）逐条对齐，一个都不能漏：
//
//  1. 账号级冷却  coolingUntil（软冷却与硬冷却都清）
//  2. 熔断        breakerUntil
//  3. 失败计数    fails（熔断的计数来源，不清则下一次失败立刻重新熔断）
//  4. 连败降权    degradeUntil + consecutiveFails
//  5. 模型级冷却  modelCooling（最常见的漏项：只清账号级冷却的话，
//     /status 里 cooling=false 但 model_cooling 仍在，
//     实际请求该模型依旧失败）
//
// 只清冷却就返回"复活成功"是最典型的假成功：账号看起来活了，一发请求又挂。
//
// 实现落在 pool.Revive（一次加锁、原子清完）。为什么不在 handler 里拼
// ClearCooldown + ClearModelCooldown：degradeUntil 与 consecutiveFails 是
// Account 的非导出字段，handler 够不着；而且分多次调用会留下"半复活"的中间态。
func (s *Server) handleAccountsRevive(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req accountsBatchRequest
	if err := decodeBatchBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
		return
	}
	targets, _ := s.batchTargets(req)
	if len(targets) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"没有指定要复活的账号（uids 为空且 all=false）")
		return
	}

	revived := make([]string, 0, len(targets))
	missing := make([]string, 0)
	changedAny := false
	// 逐项计数：让它出现在响应里，验收方与用户都能看到"到底清掉了什么"，
	// 而不是只看到一个黑箱数字。
	cleared := map[string]int{
		"cooldown": 0, "breaker": 0, "fails": 0,
		"degrade": 0, "model_cooldown": 0, "unclassified_fails": 0,
	}
	for _, uid := range targets {
		ch, ok := s.pool.Revive(uid)
		if !ok {
			missing = append(missing, uid)
			continue
		}
		revived = append(revived, uid)
		if ch.Cooldown {
			cleared["cooldown"]++
		}
		if ch.Breaker {
			cleared["breaker"]++
		}
		if ch.Fails {
			cleared["fails"]++
		}
		if ch.Degrade {
			cleared["degrade"]++
		}
		if ch.ModelCooldown {
			cleared["model_cooldown"]++
		}
		if ch.UnclassifiedFails > 0 {
			cleared["unclassified_fails"]++
		}
		if ch.Changed() {
			changedAny = true
		}
	}
	// 落盘：复活是"用户手动纠正状态"，必须活过重启。
	// 否则重启后 state.json 把冷却/降权又恢复回来，用户会以为按钮没生效。
	if changedAny {
		if err := s.pool.Save(); err != nil {
			s.logf("WARN: 复活账号后落盘失败: %v", err)
		}
	}
	s.logf("INFO: 批量复活 %d 个账号（未在池内 %d 个）", len(revived), len(missing))
	writeJSON(w, http.StatusOK, map[string]any{
		"revived": len(revived),
		"uids":    revived,
		"cleared": cleared,
		// missing 不在冻结契约里，属于**附加**信息：契约只规定必须有哪些键，
		// 多给一个 informational 字段不破坏绑定它的前端（前端只读 revived）。
		// 不给的话，"勾了 5 个只复活了 3 个"会变成一个无法解释的数字。
		"missing": missing,
	})
}

// handleAccountsControl POST /admin/accounts/control —— 批量启用/停用。
//
// 契约：body {"uids":[...],"all":false,"action":"enable"|"disable"}
//
//	→ 200 {"changed":N,"action":"...","uids":[...]}
//
// changed 的语义是"**在池内、本次调用生效**的账号数"（= len(uids)），
// 与既有的单账号端点一致（handleSetDisabled 幂等地返回 {uid,disabled}）。
// 刻意不区分"原本就是这个状态"：契约里的 uids 数组已经让前端能把
// 实际影响的账号逐条渲染出来，再叠一层"真变了几个"只会让"批量停用 5 个"
// 显示成"changed: 2"而让人以为操作失败。
//
// 这**不**清冷却/熔断：控制台另有「复活」按钮承担这件事。
// 把两件事混在一起会让"停用"意外地给账号洗白，语义就不可预测了。
func (s *Server) handleAccountsControl(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req accountsBatchRequest
	if err := decodeBatchBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action != "enable" && action != "disable" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`action 必须是 "enable" 或 "disable"（收到 `+strconvQuote(req.Action)+`）`)
		return
	}
	targets, _ := s.batchTargets(req)
	if len(targets) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"没有指定要操作的账号（uids 为空且 all=false）")
		return
	}

	disabled := action == "disable"
	reason := ""
	if disabled {
		reason = "控制台批量禁用"
	}
	changed := make([]string, 0, len(targets))
	missing := make([]string, 0)
	for _, uid := range targets {
		if !s.pool.SetDisabled(uid, disabled, reason) {
			missing = append(missing, uid)
			continue
		}
		changed = append(changed, uid)
	}
	if len(changed) > 0 {
		if err := s.pool.Save(); err != nil {
			s.logf("WARN: 批量%s后落盘失败: %v", action, err)
		}
	}
	s.logf("INFO: 批量%s %d 个账号（未在池内 %d 个）", action, len(changed), len(missing))
	writeJSON(w, http.StatusOK, map[string]any{
		"changed": len(changed),
		"action":  action,
		"uids":    changed,
		"missing": missing,
	})
}

// handleAccountsDelete POST /admin/accounts/delete —— 批量删除快照。
//
// 契约：body {"uids":[...]} → 200 {"deleted":N,"uids":[...],"errors":[]}
//
// 部分成功语义：删掉能删的，失败的逐条回报进 errors。
//
// ## 一个 uid 对应哪份快照
//
// 池内 uid 形如 "trae-cn-916348159159136"（AccountUID = product.Key + "-" + userId），
// 而快照文件名是 "trae-cn-916348159159136.json"（product-userId.json，
// 见 accountstore.snapshotFileName）。两者**不能**用字符串拼接互推 ——
// userId 或 product 里出现连字符时，`trae-cn-a-b` 既可能来自 (trae-cn, a-b)
// 也可能来自 (trae, cn-a-b)。所以这里走**权威来源**：拿 uid 与每条快照
// 自己算出的 AccountUID(product, snap.UserID) 做比较，找得到才删。
//
// 顺带的好处：与 `DELETE /admin/accounts/{name}`（唯一既有的删除实现）共用同一条
// 反查逻辑，不会出现"批量删和单个删认的账号不一样"这种鬼故事。
//
// ## "账号不存在"怎么处理
//
// 分两种，刻意区别对待：
//
//	(a) 池内没有、也没有快照 → **计入 deleted**（幂等）。批量删除的目标是
//	    "让这些账号消失"，它已经消失了；报 error 只会让用户对着一个
//	    看起来失败、实际无需处理的条目发呆。因此响应里另有 missing 字段
//	    说明"其中 N 个本来就不存在"，信息不丢失。
//	(b) 池内有、但快照删除**真的失败**（权限/占用/IO）→ 记 errors[]，
//	    并且**不动**池内注册（快照还在就必须保留它，否则这个账号会在
//	    重启后"复活"成一个孤立的池内条目）。
//
// 只从快照目录删除，不碰 config.trae.accounts 指向的客户端 storage.json
// （与既有 DELETE /admin/accounts/{name} 的既有语义一致）。
func (s *Server) handleAccountsDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req accountsBatchRequest
	if err := decodeBatchBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
		return
	}
	targets, _ := s.batchTargets(req)
	if len(targets) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"没有指定要删除的账号（uids 为空且 all=false）")
		return
	}

	// uid → 快照文件名（权威反查，见上方说明）
	if s.accounts == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unavailable",
			"未配置快照存储目录，无法删除账号")
		return
	}
	fileOf := s.snapshotFilesByUID()
	inPool := make(map[string]bool)
	for _, st := range s.pool.List() {
		inPool[st.UID] = true
	}

	deleted := make([]string, 0, len(targets))
	missing := make([]string, 0)
	errs := make([]map[string]string, 0)
	removedFromPool := false
	for _, uid := range targets {
		name, hasFile := fileOf[uid]
		switch {
		case !hasFile && !inPool[uid]:
			// (a) 哪儿都没有：目标状态已达成，幂等成功
			missing = append(missing, uid)
			deleted = append(deleted, uid)
		case !hasFile && inPool[uid]:
			// 池内注册但没有快照文件（来自 config.trae.accounts 的客户端凭据）。
			// 删快照的端点删不掉它 —— 如实报错，而不是假装删了。
			errs = append(errs, map[string]string{
				"uid": uid, "error": "该账号没有快照文件（可能来自 config.trae.accounts 的客户端凭据，需在配置里移除）",
			})
		default:
			if err := s.accounts.Delete(name); err != nil {
				errs = append(errs, map[string]string{"uid": uid, "file": name, "error": err.Error()})
				continue
			}
			if inPool[uid] {
				s.pool.Remove(uid)
				s.removeChannelAccount(uid)
				removedFromPool = true
			}
			s.logf("INFO: 批量删除账号 %s（快照 %s）", uid, name)
			deleted = append(deleted, uid)
		}
	}
	if removedFromPool {
		if err := s.pool.Save(); err != nil {
			s.logf("WARN: 批量删除账号后落盘失败: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": len(deleted),
		"uids":    deleted,
		// errors 恒为非 nil 的数组（契约写的是 []，null 会让前端 `r.errors.length` 崩）
		"errors":  errs,
		"missing": missing,
	})
}

// snapshotFilesByUID 反查 uid → 快照文件名。
//
// 与 handleAccountDelete 用的是同一套推导（AccountUID(product, snap.UserID)），
// 只是反过来建索引。逐条 List() 一次而不是每个 uid 调一次，
// 避免 N 个账号把快照目录读 N 遍。
func (s *Server) snapshotFilesByUID() map[string]string {
	out := map[string]string{}
	if s.accounts == nil {
		return out
	}
	entries, _ := s.accounts.List()
	for _, e := range entries {
		p, ok := traectl.ProductByKey(e.Snap.Product)
		if !ok {
			continue
		}
		out[AccountUID(p, e.Snap.UserID)] = e.Name
	}
	return out
}

// removeChannelAccount 把账号从各渠道的账号表里摘掉。
//
// 快照删了但渠道表里还留着指针的话，Pick 仍可能选到它（渠道表是选号的入口），
// 于是"删掉的账号又开始跑请求"。既有 handleAccountDelete 也做这件事。
//
// s.channels 本身在 New() 里建好后不再改（只有 map 内的 *Channel 可变），
// 所以这里像既有实现一样直接遍历，不需要 s.mu。
func (s *Server) removeChannelAccount(uid string) {
	for _, ch := range s.channels {
		ch.mu.Lock()
		delete(ch.accounts, uid)
		ch.mu.Unlock()
	}
}

// strconvQuote 用 %q 的语义给错误消息里的用户输入加引号。
//
// 直接用 `"` + s + `"` 的话，带换行的输入会把日志/响应撑成多行，读起来像两个错误。
func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "?"
	}
	return string(b)
}
