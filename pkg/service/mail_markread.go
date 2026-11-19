package service

import (
	"context"
	"errors"
	"log/slog"

	"emailbox/pkg/job"
	"emailbox/pkg/mailer"
	"emailbox/pkg/model"
	"emailbox/pkg/repo"
)

// MarkReadService 把选中账号的所有邮件标为已读。
//
// 「所有邮件」按用户的定义覆盖全部文件夹(收件箱、垃圾箱、已删除、自定义文件夹),
// 因此每个账号要遍历一次文件夹列表;账号多、文件夹多时这是明显更重的操作,
// 只能放后台任务,不能挂在同步请求上。
type MarkReadService struct {
	store    *repo.Store
	messages *MessageService
	jobs     *job.Manager
	throttle *MailJobThrottle
}

func NewMarkReadService(
	store *repo.Store, messages *MessageService, jobs *job.Manager, throttle *MailJobThrottle,
) *MarkReadService {
	return &MarkReadService{store: store, messages: messages, jobs: jobs, throttle: throttle}
}

// Type 实现 job.Runner。
func (s *MarkReadService) Type() string { return model.JobTypeMarkRead }

// SubmitBatch 提交一次批量已读任务。只能显式选择账号——
// 「把整个租户所有邮箱全部标已读」不是能在列表页顺手触发的动作。
func (s *MarkReadService) SubmitBatch(
	ctx context.Context, tenantID, userID string, accountIDs []string,
) (*model.Job, error) {
	if err := ensureNoMailJob(ctx, s.store, tenantID); err != nil {
		return nil, err
	}
	accounts, err := selectMailJobAccounts(ctx, s.store, tenantID, "标记",
		mailJobScope{Scope: CheckScopeSelected, AccountIDs: accountIDs})
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errors.New("没有可标记的账号(已停用或已被封禁)")
	}

	j, items := newMailJob(model.JobTypeMarkRead, tenantID, userID,
		interleaveByProvider(accounts), map[string]any{"count": len(accounts)})
	if err := s.jobs.Submit(ctx, j, items); err != nil {
		return nil, err
	}
	return &j, nil
}

// Run 实现 job.Runner:把一个账号所有文件夹的邮件标为已读。
func (s *MarkReadService) Run(ctx context.Context, j model.Job, item model.JobItem) job.Result {
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
	marker, ok := client.(mailer.AllReadMarker)
	if !ok {
		return job.Result{Status: model.JobItemFailed, ErrorKind: string(mailer.ErrKindProviderError),
			Message: "当前通道不支持批量标已读"}
	}
	if err := marker.MarkAllRead(ctx, cred); err != nil {
		return mailJobResult(err)
	}

	// 标记成功后未读数必然归零。写回失败只记日志:邮件已经处理完了,
	// 不能因为一列缓存没更新就把任务记成失败。
	if err := s.store.UpdateMailAccountUnread(ctxDetached(ctx), j.TenantID, item.AccountID, 0); err != nil &&
		!errors.Is(err, repo.ErrNotFound) {
		slog.Warn("清零未读数失败", "account_id", item.AccountID, "error", err)
	}
	return job.Result{Status: model.JobItemSuccess}
}
