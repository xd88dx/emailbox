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
	unread          int
	marked          bool
	listCalls       int
	listUnreadCalls int
	err             error
}

func (f *fakeMailClient) Channel() string { return mailer.ChannelIMAP }
func (f *fakeMailClient) List(context.Context, mailer.Credential, mailer.ListOptions) ([]mailer.Message, error) {
	f.listCalls++
	return []mailer.Message{{ID: "1", Subject: "hi"}}, f.err
}

// ListWithUnread 让 fake 具备「顺带取未读」的能力。仍然走 List，
// 现有用例统计上游调用次数的断言才继续成立。
func (f *fakeMailClient) ListWithUnread(
	ctx context.Context, cred mailer.Credential, opt mailer.ListOptions,
) (mailer.ListResult, error) {
	f.listUnreadCalls++
	messages, err := f.List(ctx, cred, opt)
	count := f.unread
	return mailer.ListResult{Messages: messages, Unread: &count}, err
}

func (f *fakeMailClient) Detail(context.Context, mailer.Credential, mailer.Folder, string, string) (*mailer.Detail, error) {
	return nil, nil
}
func (f *fakeMailClient) Attachment(context.Context, mailer.Credential, mailer.Folder, string, string, string) (*mailer.Attachment, error) {
	return nil, nil
}
func (f *fakeMailClient) MarkRead(_ context.Context, _ mailer.Credential, items []mailer.MessageRef) (mailer.BatchResult, error) {
	result := mailer.BatchResult{Items: make([]mailer.ItemResult, 0, len(items))}
	for _, item := range items {
		result.Items = append(result.Items, mailer.ItemResult{Ref: item, OK: true})
	}
	result.Succeeded = len(items)
	return result, f.err
}
func (f *fakeMailClient) Delete(_ context.Context, _ mailer.Credential, items []mailer.MessageRef) (mailer.BatchResult, error) {
	result := mailer.BatchResult{Items: make([]mailer.ItemResult, 0, len(items))}
	for _, item := range items {
		result.Items = append(result.Items, mailer.ItemResult{Ref: item, OK: true})
	}
	result.Succeeded = len(items)
	return result, f.err
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

// 手动刷新邮件列表时顺带写回收件箱未读数;自动打开、翻页、非收件箱都不查。
func TestMessageListRefreshWritesBackUnread(t *testing.T) {
	configs.AppConfig = &configs.Config{Session: configs.SessionConfig{ExpireHour: 24}}
	store := testStore(t)
	cipher, err := crypto.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	auth := service.NewAuthService(store)
	result, _, err := auth.Register(context.Background(), model.RegisterRequest{
		Username: "erin", Email: "erin@example.com", Password: "secret12",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := result.Tenants[0].ID
	accounts := service.NewAccountService(store, cipher, quota.NewService(store))
	created, err := accounts.Create(context.Background(), tenantID, model.CreateMailAccountRequest{
		Email: "refresh@qq.com", Password: "authcode",
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMailClient{unread: 4}
	messages := service.NewMessageService(store, cipher, quota.NewService(store), service.ChainOptions{}).
		WithChainFactory(func(*model.MailAccount) mailer.Client { return fake })
	ctx := context.Background()
	if err := store.UpdateMailAccountUnread(ctx, tenantID, created.ID, 9); err != nil {
		t.Fatal(err)
	}

	// 自动打开不该为一次缓存更新多打一次上游。
	if _, err := messages.List(ctx, tenantID, created.ID,
		mailer.ListOptions{Folder: mailer.FolderInbox, Top: 25}, false); err != nil {
		t.Fatal(err)
	}
	if fake.listUnreadCalls != 0 {
		t.Fatalf("自动打开时查了未读: %d 次", fake.listUnreadCalls)
	}

	// 手动刷新收件箱:查一次并写回。
	if _, err := messages.List(ctx, tenantID, created.ID,
		mailer.ListOptions{Folder: mailer.FolderInbox, Top: 25}, true); err != nil {
		t.Fatal(err)
	}
	if fake.listUnreadCalls != 1 {
		t.Fatalf("手动刷新应查一次未读,实际 %d 次", fake.listUnreadCalls)
	}
	stored, err := store.GetMailAccount(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UnreadCount != 4 {
		t.Fatalf("刷新后未读数 = %d,期望 4", stored.UnreadCount)
	}

	// 非收件箱与翻页都不查。
	for _, opt := range []mailer.ListOptions{
		{Folder: mailer.FolderJunk, Top: 25},
		{Folder: mailer.FolderInbox, Top: 25, Skip: 25},
	} {
		if _, err := messages.List(ctx, tenantID, created.ID, opt, true); err != nil {
			t.Fatal(err)
		}
	}
	if fake.listUnreadCalls != 1 {
		t.Fatalf("非收件箱/翻页不该查未读,实际 %d 次", fake.listUnreadCalls)
	}
}

// 标已读只扣「选中时确实未读、且在收件箱」的邮件,并且不会把未读数扣成负数。
func TestMarkReadDecrementsUnreadCount(t *testing.T) {
	configs.AppConfig = &configs.Config{Session: configs.SessionConfig{ExpireHour: 24}}
	store := testStore(t)
	cipher, err := crypto.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	auth := service.NewAuthService(store)
	result, _, err := auth.Register(context.Background(), model.RegisterRequest{
		Username: "carol", Email: "carol@example.com", Password: "secret12",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := result.Tenants[0].ID
	accounts := service.NewAccountService(store, cipher, quota.NewService(store))
	created, err := accounts.Create(context.Background(), tenantID, model.CreateMailAccountRequest{
		Email: "read@qq.com", Password: "authcode",
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMailClient{}
	messages := service.NewMessageService(store, cipher, quota.NewService(store), service.ChainOptions{}).
		WithChainFactory(func(*model.MailAccount) mailer.Client { return fake })
	ctx := context.Background()
	if err := store.UpdateMailAccountUnread(ctx, tenantID, created.ID, 5); err != nil {
		t.Fatal(err)
	}

	if _, err := messages.MarkRead(ctx, tenantID, created.ID, []mailer.MessageRef{
		{ID: "1", Folder: mailer.FolderInbox, WasUnread: true},
		{ID: "2", Folder: mailer.FolderInbox, WasUnread: true},
		{ID: "3", Folder: mailer.FolderInbox, WasUnread: false},
		{ID: "4", Folder: mailer.FolderJunk, WasUnread: true},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetMailAccount(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UnreadCount != 3 {
		t.Fatalf("标已读后未读数 = %d,期望 3", stored.UnreadCount)
	}

	if err := store.UpdateMailAccountUnread(ctx, tenantID, created.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := messages.MarkRead(ctx, tenantID, created.ID, []mailer.MessageRef{
		{ID: "5", Folder: mailer.FolderInbox, WasUnread: true},
		{ID: "6", Folder: mailer.FolderInbox, WasUnread: true},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetMailAccount(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UnreadCount != 0 {
		t.Errorf("未读数应夹到 0,实际 %d", stored.UnreadCount)
	}
}

// 删除未读邮件同样要从缓存里扣掉,但只算收件箱。
func TestDeleteDecrementsUnreadCount(t *testing.T) {
	configs.AppConfig = &configs.Config{Session: configs.SessionConfig{ExpireHour: 24}}
	store := testStore(t)
	cipher, err := crypto.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	auth := service.NewAuthService(store)
	result, _, err := auth.Register(context.Background(), model.RegisterRequest{
		Username: "dave", Email: "dave@example.com", Password: "secret12",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := result.Tenants[0].ID
	accounts := service.NewAccountService(store, cipher, quota.NewService(store))
	created, err := accounts.Create(context.Background(), tenantID, model.CreateMailAccountRequest{
		Email: "delete@qq.com", Password: "authcode",
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMailClient{}
	messages := service.NewMessageService(store, cipher, quota.NewService(store), service.ChainOptions{}).
		WithChainFactory(func(*model.MailAccount) mailer.Client { return fake })
	ctx := context.Background()
	if err := store.UpdateMailAccountUnread(ctx, tenantID, created.ID, 4); err != nil {
		t.Fatal(err)
	}

	if _, err := messages.Delete(ctx, tenantID, created.ID, []mailer.MessageRef{
		{ID: "1", Folder: mailer.FolderInbox, WasUnread: true},
		{ID: "2", Folder: mailer.FolderInbox, WasUnread: true},
		{ID: "3", Folder: mailer.FolderInbox, WasUnread: false},
		{ID: "4", Folder: mailer.FolderJunk, WasUnread: true},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetMailAccount(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UnreadCount != 2 {
		t.Fatalf("删除后未读数 = %d,期望 2", stored.UnreadCount)
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
