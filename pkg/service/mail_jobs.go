package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"emailbox/pkg/job"
	"emailbox/pkg/mailer"
	"emailbox/pkg/model"
	"emailbox/pkg/repo"

	"github.com/google/uuid"
)

// ErrMailJobRunning 表示同一租户已有收信检查或批量已读任务在跑。
//
// 两者都是「按账号打上游」的任务,同时跑会让同一批账号的请求量翻倍;
// 上游看到的是同一批凭据的两倍请求,风控只会更快落下来。
var ErrMailJobRunning = errors.New("已有收信检查或批量已读任务在运行,请等它结束后再提交")

// unreadRecheckCooldown 是同一个账号两次未读检查之间的最短间隔。
// 用户连点刷新时,第二次不再重复打上游——刚查过的结果就是最新的。
const unreadRecheckCooldown = 60 * time.Second

// mailJobScope 是这两个任务共用的选取参数。
type mailJobScope struct {
	Scope      string
	AccountIDs []string
	GroupIDs   []string
}

// selectMailJobAccounts 选取一次批量任务要处理的账号。
//
// 与 HealthService.selectAccounts 的差别只有一处:这里额外跳过被封禁的账号。
// 检测任务需要看到「这个账号被封了」这个结论,而未读检查/标已读对封禁账号
// 做任何事都是白白打一次上游。
func selectMailJobAccounts(
	ctx context.Context, store *repo.Store, tenantID, action string, s mailJobScope,
) ([]model.MailAccount, error) {
	var (
		accounts []model.MailAccount
		err      error
	)
	switch s.Scope {
	case CheckScopeSelected:
		if len(s.AccountIDs) == 0 {
			return nil, fmt.Errorf("请先选择要%s的账号", action)
		}
		if len(s.AccountIDs) > model.MaxBatchAccountIDs {
			return nil, fmt.Errorf("单次最多%s %d 个账号,请分批", action, model.MaxBatchAccountIDs)
		}
		accounts, err = store.ListMailAccountsByIDs(ctx, tenantID, s.AccountIDs)
	case CheckScopeGroup:
		accounts, err = accountsInGroups(ctx, store, tenantID, s.GroupIDs)
	case CheckScopeAll, "":
		accounts, err = collectAccounts(ctx, store, tenantID, model.AccountFilter{})
	default:
		return nil, fmt.Errorf("未知的选取范围 %q", s.Scope)
	}
	if err != nil {
		return nil, err
	}
	out := make([]model.MailAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.Status == model.AccountStatusDisabled || account.Status == model.AccountStatusBanned {
			continue
		}
		out = append(out, account)
	}
	return out, nil
}

// providerKey 是限流的隔离粒度。
//
// 已知服务商用 provider(qq/163/gmail/outlook...);自建 IMAP 的 provider 是 custom,
// 各家的服务器完全不同,按主机名分开限流才不会把别人家的压力算到自己头上。
func providerKey(account *model.MailAccount) string {
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	if provider != "" && provider != mailer.ProviderCustom {
		return provider
	}
	if host := strings.ToLower(strings.TrimSpace(account.IMAPHost)); host != "" {
		return "host:" + host
	}
	return "unknown"
}

// interleaveByProvider 把账号按服务商交错排列。
//
// 任务队列是 FIFO 的:如果队首几百个都是同一家,worker 会全部堵在那一家的限流上,
// 其他服务商的账号只能在后面干等。交错之后各家的槽位能同时转起来。
func interleaveByProvider(accounts []model.MailAccount) []model.MailAccount {
	groups := make(map[string][]model.MailAccount)
	order := make([]string, 0, 4)
	for _, account := range accounts {
		key := providerKey(&account)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], account)
	}
	out := make([]model.MailAccount, 0, len(accounts))
	for len(out) < len(accounts) {
		for _, key := range order {
			if len(groups[key]) == 0 {
				continue
			}
			out = append(out, groups[key][0])
			groups[key] = groups[key][1:]
		}
	}
	return out
}

// providerGate 是一个服务商的闸门:并发上限 + 每秒请求数上限。
type providerGate struct {
	sem  chan struct{}
	rate float64
	mu   sync.Mutex
	// tokens 是令牌桶余额,last 是上次补充时间。
	tokens float64
	last   time.Time
}

// MailJobThrottle 按服务商限制批量任务的并发与速率。
//
// 同一个实例在收信检查与批量已读之间共享:两者互斥,共享一份闸门就是全局限流。
type MailJobThrottle struct {
	mu          sync.Mutex
	gates       map[string]*providerGate
	concurrency int
	perSecond   float64
}

func NewMailJobThrottle(concurrency, perSecond int) *MailJobThrottle {
	if concurrency <= 0 {
		concurrency = 2
	}
	if perSecond <= 0 {
		perSecond = 2
	}
	return &MailJobThrottle{
		gates:       make(map[string]*providerGate),
		concurrency: concurrency,
		perSecond:   float64(perSecond),
	}
}

// acquire 取得某个服务商的一次出站许可,返回释放函数。
func (t *MailJobThrottle) acquire(ctx context.Context, key string) (func(), error) {
	if t == nil {
		return func() {}, nil
	}
	gate := t.gate(key)
	select {
	case gate.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := gate.waitToken(ctx); err != nil {
		<-gate.sem
		return nil, err
	}
	return func() { <-gate.sem }, nil
}

func (t *MailJobThrottle) gate(key string) *providerGate {
	t.mu.Lock()
	defer t.mu.Unlock()
	gate, ok := t.gates[key]
	if !ok {
		gate = &providerGate{
			sem:    make(chan struct{}, t.concurrency),
			rate:   t.perSecond,
			tokens: 1, // 允许每个服务商立刻发出第一个请求
			last:   time.Now(),
		}
		t.gates[key] = gate
	}
	return gate
}

func (g *providerGate) waitToken(ctx context.Context) error {
	for {
		g.mu.Lock()
		now := time.Now()
		g.tokens = math.Min(1, g.tokens+now.Sub(g.last).Seconds()*g.rate)
		g.last = now
		if g.tokens >= 1 {
			g.tokens--
			g.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - g.tokens) / g.rate * float64(time.Second))
		g.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// newMailJob 组装一个批量邮件任务的 job 与 items。
func newMailJob(jobType, tenantID, userID string, accounts []model.MailAccount, fields map[string]any) (model.Job, []model.JobItem) {
	params := []byte("{}")
	if encoded, err := json.Marshal(fields); err == nil {
		params = encoded
	}
	j := model.Job{
		ID: uuid.NewString(), TenantID: tenantID, Type: jobType,
		Trigger: model.JobTriggerManual, Status: model.JobStatusPending,
		CreatedBy: userID, TotalCount: len(accounts), Params: string(params),
	}
	items := make([]model.JobItem, 0, len(accounts))
	for i, account := range accounts {
		items = append(items, model.JobItem{
			ID: uuid.NewString(), JobID: j.ID, AccountID: account.ID,
			Email: account.Email, Position: i, Status: model.JobItemPending,
		})
	}
	return j, items
}

// mailJobResult 把一次账号操作的错误映射成任务明细结果。
//
// 已停用/已删除/被封禁都算「跳过」:它们不是这次任务要解决的问题,
// 混进失败计数里只会让用户以为任务出错了。
func mailJobResult(err error) job.Result {
	switch {
	case err == nil:
		return job.Result{Status: model.JobItemSuccess}
	case errors.Is(err, ErrAccountDisabled):
		return job.Result{Status: model.JobItemSkipped, Message: "账号已停用"}
	case errors.Is(err, ErrAccountBanned):
		return job.Result{Status: model.JobItemSkipped, Message: "账号已被封禁"}
	case errors.Is(err, repo.ErrNotFound):
		return job.Result{Status: model.JobItemSkipped, Message: "账号已删除"}
	}
	return job.Result{
		Status:    model.JobItemFailed,
		ErrorKind: string(mailer.KindOf(err)),
		Message:   truncateError(err.Error()),
	}
}
