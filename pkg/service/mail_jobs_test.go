package service_test

import (
	"context"
	"testing"

	"emailbox/configs"
	"emailbox/pkg/crypto"
	"emailbox/pkg/mailer"
	"emailbox/pkg/model"
	"emailbox/pkg/quota"
	"emailbox/pkg/service"
)

// fakeMailClient 只实现收信检查/批量已读用得到的那两个窄接口。
// 其余方法保持空实现:这两个任务不该顺带调用 List/Detail/Delete。
type fakeMailClient struct {
	unread    int
	marked    bool
	listCalls int
	err       error
}

func (f *fakeMailClient) Channel() string { return mailer.ChannelIMAP }
func (f *fakeMailClient) List(context.Context, mailer.Credential, mailer.ListOptions) ([]mailer.Message, error) {
	f.listCalls++
	return []mailer.Message{{ID: "1", Subject: "hi"}}, f.err
}
func (f *fakeMailClient) Detail(context.Context, mailer.Credential, mailer.Folder, string, string) (*mailer.Detail, error) {
	return nil, nil
}
func (f *fakeMailClient) Attachment(context.Context, mailer.Credential, mailer.Folder, string, string, string) (*mailer.Attachment, error) {
	return nil, nil
}
func (f *fakeMailClient) MarkRead(context.Context, mailer.Credential, []mailer.MessageRef) (mailer.BatchResult, error) {
	return mailer.BatchResult{}, nil
}
func (f *fakeMailClient) Delete(context.Context, mailer.Credential, []mailer.MessageRef) (mailer.BatchResult, error) {
	return mailer.BatchResult{}, nil
}
func (f *fakeMailClient) UnreadCount(context.Context, mailer.Credential) (int, error) {
	return f.unread, f.err
}
func (f *fakeMailClient) MarkAllRead(context.Context, mailer.Credential) error {
	f.marked = true
	return f.err
}

// 收件箱首页一分钟内复用缓存;手动刷新(force)绕过缓存;
// 标已读之后缓存必须失效,否则列表还显示旧的已读状态。
func TestMessageListCacheRespectsForceAndInvalidation(t *testing.T) {
	configs.AppConfig = &configs.Config{Session: configs.SessionConfig{ExpireHour: 24}}
	store := testStore(t)
	cipher, err := crypto.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	auth := service.NewAuthService(store)
	result, _, err := auth.Register(context.Background(), model.RegisterRequest{
		Username: "bob", Email: "bob@example.com", Password: "secret12",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := result.Tenants[0].ID
	accounts := service.NewAccountService(store, cipher, quota.NewService(store))
	created, err := accounts.Create(context.Background(), tenantID, model.CreateMailAccountRequest{
		Email: "cache@qq.com", Password: "authcode",
	})
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeMailClient{}
	messages := service.NewMessageService(store, cipher, quota.NewService(store), service.ChainOptions{}).
		WithChainFactory(func(*model.MailAccount) mailer.Client { return fake })
	ctx := context.Background()
	opt := mailer.ListOptions{Folder: mailer.FolderInbox, Top: 25}

	if _, err := messages.List(ctx, tenantID, created.ID, opt, false); err != nil {
		t.Fatal(err)
	}
	if _, err := messages.List(ctx, tenantID, created.ID, opt, false); err != nil {
		t.Fatal(err)
	}
	if fake.listCalls != 1 {
		t.Fatalf("一分钟内第二次打开邮箱仍打了上游: %d 次", fake.listCalls)
	}

	if _, err := messages.List(ctx, tenantID, created.ID, opt, true); err != nil {
		t.Fatal(err)
	}
	if fake.listCalls != 2 {
		t.Fatalf("手动刷新没有绕过缓存: %d 次", fake.listCalls)
	}

	if _, err := messages.MarkRead(ctx, tenantID, created.ID, []mailer.MessageRef{
		{ID: "1", IDMode: mailer.IDModeNone, Folder: mailer.FolderInbox},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := messages.List(ctx, tenantID, created.ID, opt, false); err != nil {
		t.Fatal(err)
	}
	if fake.listCalls != 3 {
		t.Fatalf("标已读后缓存没有失效: %d 次", fake.listCalls)
	}
}

// 未读检查写回 unread_count;批量已读成功后再把它清零。
func TestMailJobsWriteBackUnreadCount(t *testing.T) {
	configs.AppConfig = &configs.Config{Session: configs.SessionConfig{ExpireHour: 24}}
	store := testStore(t)
	cipher, err := crypto.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	auth := service.NewAuthService(store)
	result, _, err := auth.Register(context.Background(), model.RegisterRequest{
		Username: "alice", Email: "alice@example.com", Password: "secret12",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := result.Tenants[0].ID

	accounts := service.NewAccountService(store, cipher, quota.NewService(store))
	created, err := accounts.Create(context.Background(), tenantID, model.CreateMailAccountRequest{
		Email: "user@qq.com", Password: "authcode",
	})
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeMailClient{unread: 7}
	messages := service.NewMessageService(store, cipher, quota.NewService(store), service.ChainOptions{}).
		WithChainFactory(func(*model.MailAccount) mailer.Client { return fake })
	ctx := context.Background()
	j := model.Job{ID: "j1", TenantID: tenantID, Type: model.JobTypeUnreadCheck}
	item := model.JobItem{AccountID: created.ID, Email: created.Email}

	unread := service.NewUnreadService(store, messages, nil, nil)
	if res := unread.Run(ctx, j, item); res.Status != model.JobItemSuccess {
		t.Fatalf("未读检查结果 = %+v", res)
	}
	stored, err := store.GetMailAccount(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UnreadCount != 7 || stored.UnreadCheckedAt == nil {
		t.Fatalf("未读数写回 = %d / %v", stored.UnreadCount, stored.UnreadCheckedAt)
	}

	markRead := service.NewMarkReadService(store, messages, nil, nil)
	j.Type = model.JobTypeMarkRead
	if res := markRead.Run(ctx, j, item); res.Status != model.JobItemSuccess {
		t.Fatalf("批量已读结果 = %+v", res)
	}
	if !fake.marked {
		t.Error("MarkAllRead 没有被调用")
	}
	stored, err = store.GetMailAccount(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UnreadCount != 0 {
		t.Errorf("标已读后未读数 = %d,期望 0", stored.UnreadCount)
	}
}
