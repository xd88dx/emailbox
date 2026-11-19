package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"emailbox/pkg/job"
	"emailbox/pkg/mailer"
	"emailbox/pkg/model"
	"emailbox/pkg/repo"
)

// UnreadService 负责「刷新时检查各邮箱未读」。
//
// 它只读一遍收件箱的元数据:不下载正文,不扣 daily_mail_fetch 配额——
// 与账号检测同口径,回答的是「这个邮箱现在有没有新邮件」,不是把信搬回来。
// 上游压力由 MailJobThrottle 与 JOB_WORKERS/JOB_ACCOUNT_DELAY_MS 共同控制。
type UnreadService struct {
	store    *repo.Store
	messages *MessageService
	jobs     *job.Manager
	throttle *MailJobThrottle
}

func NewUnreadService(
	store *repo.Store, messages *MessageService, jobs *job.Manager, throttle *MailJobThrottle,
) *UnreadService {
	return &UnreadService{store: store, messages: messages, jobs: jobs, throttle: throttle}
}

// Type 实现 job.Runner。
func (s *UnreadService) Type() string { return model.JobTypeUnreadCheck }

// SubmitBatch 提交一次收信检查任务。
//
// scope=selected 时不做冷却过滤:用户明确选中了这些账号,就应该真的查一次;
// 全部/分组检查则跳过 60 秒内刚查过的账号,避免连点刷新把上游打两遍。
func (s *UnreadService) SubmitBatch(
	ctx context.Context, tenantID, userID, scope string, accountIDs, groupIDs []string,
) (*model.Job, error) {
	if err := ensureNoMailJob(ctx, s.store, tenantID); err != nil {
		return nil, err
	}
	accounts, err := selectMailJobAccounts(ctx, s.store, tenantID, "检查",
		mailJobScope{Scope: scope, AccountIDs: accountIDs, GroupIDs: groupIDs})
	if err != nil {
		return nil, err
	}
	if scope != CheckScopeSelected {
		accounts = filterRecentlyChecked(accounts, time.Now().Add(-unreadRecheckCooldown))
	}
	if len(accounts) == 0 {
		return nil, errors.New("没有可检查的账号(刚检查过、已停用或已被封禁)")
	}

	j, items := newMailJob(model.JobTypeUnreadCheck, tenantID, userID,
		interleaveByProvider(accounts), map[string]any{"scope": scope, "count": len(accounts)})
	if err := s.jobs.Submit(ctx, j, items); err != nil {
		return nil, err
	}
	return &j, nil
}

// Run 实现 job.Runner:检查一个账号的收件箱未读数并写回。
func (s *UnreadService) Run(ctx context.Context, j model.Job, item model.JobItem) job.Result {
	if item.AccountID == "" {
		return job.Result{Status: model.JobItemSkipped, Message: "账号已删除"}
	}
	account, cred, err := s.messages.credential(ctx, j.TenantID, item.AccountID)
	if err != nil {
		return mailJobResult(err)
	}
	release, err := s.throttle.acquire(ctx, providerKey(account))
	if err != nil {
		return mailJobResult(err)
	}
	defer release()

	client := s.messages.chainFor(account)
	counter, ok := client.(mailer.UnreadCounter)
	if !ok {
		return job.Result{Status: model.JobItemFailed, ErrorKind: string(mailer.ErrKindProviderError),
			Message: "当前通道不支持未读检查"}
	}
	count, err := counter.UnreadCount(ctx, cred)
	if err != nil {
		return mailJobResult(err)
	}
	// 写回用注入的 context:检查已经完成,不能因为用户点停止就丢掉结果。
	if err := s.store.UpdateMailAccountUnread(ctxDetached(ctx), j.TenantID, item.AccountID, count); err != nil {
		if !errors.Is(err, repo.ErrNotFound) {
			slog.Warn("写回未读数失败", "account_id", item.AccountID, "error", err)
			return job.Result{Status: model.JobItemFailed, Message: truncateError(err.Error())}
		}
	}
	return job.Result{Status: model.JobItemSuccess}
}

// filterRecentlyChecked 去掉 cutoff 之后已经检查过的账号。
func filterRecentlyChecked(accounts []model.MailAccount, cutoff time.Time) []model.MailAccount {
	out := make([]model.MailAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.UnreadCheckedAt != nil && account.UnreadCheckedAt.After(cutoff) {
			continue
		}
		out = append(out, account)
	}
	return out
}

// ensureNoMailJob 保证收信检查与批量已读不会同时跑。
func ensureNoMailJob(ctx context.Context, store *repo.Store, tenantID string) error {
	for _, jobType := range []string{model.JobTypeUnreadCheck, model.JobTypeMarkRead} {
		active, err := store.CountActiveJobsByType(ctx, tenantID, jobType)
		if err != nil {
			return err
		}
		if active > 0 {
			return ErrMailJobRunning
		}
	}
	return nil
}
