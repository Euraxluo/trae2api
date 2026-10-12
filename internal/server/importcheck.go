package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"trae2api/internal/traectl"
)

// 本文件实现**导入后的异步校验**。
//
// ## 要解决的问题
//
// 导入只解析并落盘凭据 —— 它**不验证**这些凭据是否真的能用：
// refreshToken 可能已失效、账号可能被停用、token 可能已过期。
// 用户导入 20 个账号后，只有等第一次真实请求失败才知道哪几个是坏的。
//
// 所以导入后**在后台并发校验**：续期一次 token、拉一次额度，把结果实时回显。
//
// ## 为什么是异步而不是同步
//
// 同步校验会让导入接口的耗时取决于网络（20 个账号 × 每个 1~3 秒 = 一分钟），
// 而且中途断连就前功尽弃。异步的好处：
//   - 导入立即返回，用户马上看到"收下了几个"
//   - 校验在后台跑，进度可轮询
//   - 部分失败不影响其余
//
// ## 为什么并发度要限制
//
// 20 个账号同时打上游 = 20 个并发请求，很容易触发频控 —— 而频控会**污染**
// 校验结果（把"限流"误报成"凭据无效"）。所以并发度限制在 4，
// 并在结果里区分"凭据问题"与"网络/限流问题"。

// importJob 是一次导入后的校验任务。
type importJob struct {
	ID      string
	Started time.Time

	mu      sync.Mutex
	total   int
	done    int
	results []importCheckResult
	state   string // running / done
	// cancel 让调用方能取消（服务停机时）。
	cancel context.CancelFunc
}

// importCheckResult 是单个账号的校验结果。
type importCheckResult struct {
	UID   string `json:"uid"`
	Label string `json:"label"`
	// OK 凭据有效（续期成功）。
	OK bool `json:"ok"`
	// Credits 校验时拿到的剩余额度（-1 = 没拿到）。
	Credits int64 `json:"credits"`
	// ExpiredAt 续期后的 token 到期时间。
	ExpiredAt string `json:"expired_at,omitempty"`
	// Error 失败原因（已区分"凭据问题"与"网络问题"）。
	Error string `json:"error,omitempty"`
	// Transient 表示失败是**临时的**（网络/限流），重试可能成功 ——
	// 与"凭据确实无效"区分开，避免用户误删好账号。
	Transient bool `json:"transient,omitempty"`
}

// importValidator 管理进行中的校验任务。
type importValidator struct {
	mu   sync.Mutex
	jobs map[string]*importJob
}

func newImportValidator() *importValidator {
	return &importValidator{jobs: map[string]*importJob{}}
}

// start 创建并启动一次校验任务。
func (v *importValidator) start(accounts []*poolAccountRef) *importJob {
	ctx, cancel := context.WithCancel(context.Background())
	job := &importJob{
		ID:      randHex(8),
		Started: time.Now(),
		total:   len(accounts),
		state:   "running",
		cancel:  cancel,
	}
	v.mu.Lock()
	v.reapLocked()
	v.jobs[job.ID] = job
	v.mu.Unlock()

	go func() {
		defer cancel()
		v.run(ctx, job, accounts)
	}()
	return job
}

// reapLocked 清理超过 30 分钟的已完成任务。
func (v *importValidator) reapLocked() {
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, j := range v.jobs {
		j.mu.Lock()
		finished := j.state == "done" && j.Started.Before(cutoff)
		j.mu.Unlock()
		if finished {
			delete(v.jobs, id)
		}
	}
}

func (v *importValidator) get(id string) *importJob {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.jobs[id]
}

// poolAccountRef 是校验需要的最小信息（避免把整个 pool.Account 传进来）。
type poolAccountRef struct {
	UID   string
	Label string
}

// run 并发校验（并发度受限，见文件头说明）。
func (v *importValidator) run(ctx context.Context, job *importJob, accounts []*poolAccountRef) {
	const concurrency = 4
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, a := range accounts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a *poolAccountRef) {
			defer wg.Done()
			defer func() { <-sem }()
			res := v.checkOne(ctx, a)
			job.mu.Lock()
			job.results = append(job.results, res)
			job.done++
			job.mu.Unlock()
		}(a)
	}
	wg.Wait()

	job.mu.Lock()
	job.state = "done"
	job.mu.Unlock()
}

// checkOne 校验单个账号。具体动作由 Server 注入（见 checkImportAccount）。
func (v *importValidator) checkOne(ctx context.Context, a *poolAccountRef) importCheckResult {
	if importChecker == nil {
		return importCheckResult{UID: a.UID, Label: a.Label, OK: true}
	}
	return importChecker(ctx, a)
}

// importChecker 由 Server 在启动时注入（避免本文件依赖 Server 的字段）。
var importChecker func(ctx context.Context, a *poolAccountRef) importCheckResult

// wireImportChecker 注入真实的校验动作（在 Server.New 里调用一次）。
//
// 为什么用注入而不是让本文件直接依赖 Server：校验任务的生命周期与 Server
// 的字段无关，注入让这个文件可以独立测试（塞一个假的 checker 即可）。
func (s *Server) wireImportChecker() {
	importChecker = s.checkImportedAccount
}

// checkImportedAccount 校验一个刚导入的账号：续期一次 token + 拉一次额度。
//
// ## 为什么要区分"凭据问题"与"网络问题"
//
// 两者对用户的含义完全不同：
//   - **凭据问题**（refreshToken 失效）→ 这个账号废了，该删掉重新登录
//   - **网络问题**（超时/限流）→ 账号是好的，只是这次没验成，重试即可
//
// 混在一起会让用户**误删好账号**。所以 Transient 标记必须准确：
// 只有 RefreshError（上游明确拒绝凭据）才算"凭据问题"。
func (s *Server) checkImportedAccount(ctx context.Context, a *poolAccountRef) importCheckResult {
	res := importCheckResult{UID: a.UID, Label: a.Label, Credits: -1}

	acct := s.pool.Get(a.UID)
	if acct == nil || acct.Store == nil {
		res.Error = "账号不在池中（可能已被删除）"
		res.Transient = true
		return res
	}

	// 1) 续期：这是**唯一**能证明凭据还有效的动作
	if err := acct.Store.Refresh(ctx); err != nil {
		var re *traectl.RefreshError
		if errors.As(err, &re) {
			// 上游明确拒绝凭据 → 真问题
			res.Error = "凭据已失效（需重新登录客户端）：" + re.Error()
			return res
		}
		// 传输层失败 / 未知 → 临时问题，别让用户删号
		res.Error = "校验时网络异常（账号本身可能没问题，可稍后重试）：" + err.Error()
		res.Transient = true
		return res
	}
	cred, _, _ := acct.Store.Snapshot()
	res.OK = true
	res.ExpiredAt = cred.ExpiredAt

	// 2) 拉一次额度（失败不影响"凭据有效"的结论）
	if remain, ok := s.refreshAccountCredits(ctx, acct); ok {
		res.Credits = int64(remain)
	}
	return res
}

// status 返回任务状态（供轮询）。
func (j *importJob) status() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	ok, failed, transient := 0, 0, 0
	for _, r := range j.results {
		if r.OK {
			ok++
		} else if r.Transient {
			transient++
		} else {
			failed++
		}
	}
	m := map[string]any{
		"job_id":      j.ID,
		"state":       j.state,
		"total":       j.total,
		"done":        j.done,
		"ok":          ok,
		"failed":      failed,
		"transient":   transient,
		"elapsed_sec": int(time.Since(j.Started).Seconds()),
		"results":     j.results,
	}
	return m
}
