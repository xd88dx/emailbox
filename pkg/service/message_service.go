package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"emailbox/pkg/crypto"
	"emailbox/pkg/mailer"
	"emailbox/pkg/mailer/imapx"
	"emailbox/pkg/model"
	"emailbox/pkg/quota"
	"emailbox/pkg/repo"
)

// 邮件相关的业务错误。handler 层据此映射 HTTP 状态码。
var (
	ErrInvalidFolder = errors.New("不支持的邮件夹")
	// ErrAccountBanned 是已知被封的账号。已经封了还继续打上游只会加重风控，
	// 所以在进协议层之前就挡下。
	ErrAccountBanned   = errors.New("账号已被服务商封禁")
	ErrAccountDisabled = errors.New("账号已被停用")
	// ErrCredentialUndecryptable 通常意味着 ENCRYPTION_KEY 换过了。
	// 必须与「密码错误」区分开：报成密码错误的话用户会去改密码，越改越乱。
	ErrCredentialUndecryptable = errors.New("凭据无法解密，请检查 ENCRYPTION_KEY 是否变更")
)

// maxTop 是单次拉取的上限。再大对用户没有意义，却会显著拉长远端调用时间，
// 也更容易撞上服务商限流。
const maxTop = 50

// MessageService 把「从库里取账号 → 解密 → 调协议层 → 写回结果」串起来。
//
// mailer 包不碰数据库，这一层就是它与业务之间唯一的接缝。
type MessageService struct {
	store        *repo.Store
	cipher       crypto.Cipher
	quota        *quota.Service
	chainOptions ChainOptions
	// chainFor 按账号构造回退链。做成字段是为了让测试注入假通道——
	// 真实实现要连微软，单测里跑不了。
	chainFor func(account *model.MailAccount) mailer.Client
	// 收件箱首页缓存:自动打开/切换邮箱时一分钟内不重复打上游。
	// 手动刷新(force)绕过它,否则用户按了刷新却看到同一份缓存。
	cacheMu sync.Mutex
	cache   map[string]messageCacheEntry
	// pool 跨请求复用已登录的 IMAP 会话。它按账号隔离,且只由本服务持有——
	// 做成包级变量会让不同租户的连接混在一起,那是最不该发生的一类串号。
	pool *imapx.SessionPool
}

func NewMessageService(
	store *repo.Store, cipher crypto.Cipher, q *quota.Service, opt ChainOptions,
) *MessageService {
	s := &MessageService{store: store, cipher: cipher, quota: q, chainOptions: opt,
		cache: make(map[string]messageCacheEntry), pool: imapx.NewSessionPool(imapx.PoolConfig{})}
	s.chainFor = defaultChainFactory(s, opt)
	return s
}

// Close 释放池里的空闲 IMAP 连接。服务退出时调用。
func (s *MessageService) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// WithChainFactory 换掉回退链的构造方式，只给测试用：
// 真实的链要连微软与各家 IMAP，单测里跑不了。
func (s *MessageService) WithChainFactory(f func(*model.MailAccount) mailer.Client) *MessageService {
	s.chainFor = f
	return s
}

// MessageListResult 是列表接口的响应体。
type MessageListResult struct {
	Items []mailer.Message `json:"items"`
	// Channel 是本次实际走通的通道，前端用它显示具体 IMAP 认证通道。
	Channel string `json:"channel"`
}

// List 拉取邮件列表。
//
// folder=all 在这里拆成收件箱 + 垃圾箱两次调用后归并——协议层不认识 all，
// 让它认识的话每个通道都要实现一遍归并逻辑。
func (s *MessageService) List(
	ctx context.Context, tenantID, accountID string, opt mailer.ListOptions, force bool,
) (*MessageListResult, error) {
	if opt.Top <= 0 || opt.Top > maxTop {
		opt.Top = min(max(opt.Top, 1), maxTop)
	}
	if opt.Skip < 0 {
		opt.Skip = 0
	}
	if !mailer.ValidFolder(opt.Folder) {
		return nil, ErrInvalidFolder
	}

	start := time.Now()
	cached := false
	// 一行日志回答「这次打开邮箱到底慢在哪」:cached 说明有没有真的打上游,
	// elapsed_ms 是用户感知的等待时间。协议层还会分别记建连与 FETCH 的耗时。
	defer func() {
		slog.Info("邮件列表请求完成",
			"account_id", accountID,
			"folder", opt.Folder,
			"skip", opt.Skip,
			"top", opt.Top,
			"force", force,
			"cached", cached,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
	}()

	account, cred, err := s.credential(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	// 只缓存首页:翻页是一次性动作,缓存它们的收益远不如再取一次。
	cacheable := opt.Skip == 0
	// 手动刷新时顺带查一次未读数：列表本来就要建会话，未读只是一条元数据命令。
	// 自动打开与切文件夹不加，避免把这次查询摊到每一次点击上。
	wantUnread := force && cacheable &&
		(opt.Folder == mailer.FolderInbox || opt.Folder == mailer.FolderAll)
	cacheKey := ""
	if cacheable {
		cacheKey = messageCacheKey(tenantID, accountID, messageListCacheOptions{
			folder: string(opt.Folder), skip: opt.Skip, top: opt.Top,
		})
		if !force {
			if hit, ok := s.cacheGet(cacheKey); ok {
				cached = true
				return hit, nil
			}
		}
	}
	// 配额在走远端之前扣：扣完才发请求，超额时一个远端调用都不产生。
	if err := s.quota.CheckAndConsume(ctx, tenantID, model.MetricMailFetch, 1); err != nil {
		return nil, err
	}

	client, record := s.clientFor(ctx, tenantID, account)
	if opt.Folder == mailer.FolderAll {
		result, unread, err := s.listAll(ctx, client, record, cred, opt, wantUnread)
		if err != nil {
			return nil, err
		}
		s.rememberUnread(ctx, tenantID, accountID, unread)
		if cacheable {
			s.cachePut(cacheKey, result)
		}
		return result, nil
	}

	messages, unread, err := listMessages(ctx, client, cred, opt, wantUnread)
	record(err)
	if err != nil {
		return nil, err
	}
	s.rememberUnread(ctx, tenantID, accountID, unread)
	result := &MessageListResult{
		Items:   nonNil(messages),
		Channel: channelOf(err, account),
	}
	if cacheable {
		s.cachePut(cacheKey, result)
	}
	return result, nil
}

// listAll 聚合收件箱与垃圾箱，按时间倒序取前 top 条。
//
// 两个邮件夹各取 skip+top 条再归并：只取 top 条的话，
// 当一个邮件夹的邮件明显更新时，翻到第二页会漏掉另一个邮件夹的邮件。
func (s *MessageService) listAll(
	ctx context.Context, client mailer.Client, record func(error),
	cred mailer.Credential, opt mailer.ListOptions, wantUnread bool,
) (*MessageListResult, *int, error) {
	folders := []mailer.Folder{mailer.FolderInbox, mailer.FolderJunk}
	// 两个邮件夹互不依赖,各自从会话池取一条连接并行拉。串行时一次
	// folder=all 要付两遍网络往返,这是「全部邮件」比收件箱慢一倍的原因。
	type folderResult struct {
		messages []mailer.Message
		unread   *int
		err      error
	}
	results := make([]folderResult, len(folders))
	var wg sync.WaitGroup
	for i, folder := range folders {
		wg.Add(1)
		go func(i int, folder mailer.Folder) {
			defer wg.Done()
			results[i].messages, results[i].unread, results[i].err = listMessages(
				ctx, client, cred, mailer.ListOptions{
					Folder: folder,
					Skip:   0,
					Top:    opt.Skip + opt.Top,
				}, wantUnread && folder == mailer.FolderInbox)
		}(i, folder)
	}
	wg.Wait()

	merged := make([]mailer.Message, 0, opt.Top*2)
	var firstErr error
	var unread *int
	for i, folder := range folders {
		part, count, err := results[i].messages, results[i].unread, results[i].err
		if err != nil {
			// 一个邮件夹取不到不该让整个请求失败：多数账号的垃圾箱是可有可无的，
			// 而收件箱拿到了就该展示出来。全都失败时才报错。
			if firstErr == nil {
				firstErr = err
			}
			slog.Debug("folder=all 的某个邮件夹拉取失败",
				"folder", folder, "email", mailer.MaskEmail(cred.Email), "error", err)
			continue
		}
		if count != nil {
			unread = count
		}
		merged = append(merged, part...)
	}
	if len(merged) == 0 && firstErr != nil {
		record(firstErr)
		return nil, nil, firstErr
	}
	record(nil)

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].ReceivedAt.After(merged[j].ReceivedAt)
	})
	if opt.Skip < len(merged) {
		merged = merged[opt.Skip:]
	} else {
		merged = nil
	}
	if len(merged) > opt.Top {
		merged = merged[:opt.Top]
	}
	return &MessageListResult{Items: nonNil(merged)}, unread, nil
}

// listMessages 拉一页列表；wantUnread 时走「顺带取未读」的通道能力。
//
// 不支持该能力的通道（测试桩、以后新增的实现）退回普通 List，
// 未读数留空，调用方沿用库里已有的值。
func listMessages(
	ctx context.Context, client mailer.Client, cred mailer.Credential,
	opt mailer.ListOptions, wantUnread bool,
) ([]mailer.Message, *int, error) {
	if wantUnread {
		if lister, ok := client.(mailer.UnreadLister); ok {
			result, err := lister.ListWithUnread(ctx, cred, opt)
			return result.Messages, result.Unread, err
		}
	}
	messages, err := client.List(ctx, cred, opt)
	return messages, nil, err
}

// rememberUnread 把列表顺带查到的收件箱未读数写回账号。
//
// 写回失败只记日志：列表已经拿到了，不能因为一个缓存值没记上就让用户看不到信。
func (s *MessageService) rememberUnread(
	ctx context.Context, tenantID, accountID string, unread *int,
) {
	if unread == nil {
		return
	}
	if err := s.store.UpdateMailAccountUnread(ctxDetached(ctx), tenantID, accountID, *unread); err != nil &&
		!errors.Is(err, repo.ErrNotFound) {
		slog.Warn("写回收件箱未读数失败", "account_id", accountID, "error", err)
	}
}

// Detail 取一封邮件的详情。
func (s *MessageService) Detail(
	ctx context.Context, tenantID, accountID string, folder mailer.Folder, id, idMode string,
) (*mailer.Detail, error) {
	if folder == mailer.FolderAll || !mailer.ValidFolder(folder) {
		return nil, ErrInvalidFolder
	}
	account, cred, err := s.credential(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	if err := s.quota.CheckAndConsume(ctx, tenantID, model.MetricMailFetch, 1); err != nil {
		return nil, err
	}
	client, record := s.clientFor(ctx, tenantID, account)
	detail, err := client.Detail(ctx, cred, folder, id, idMode)
	record(err)
	// 没有附件时协议层返回的是 nil slice，encoding/json 会把它写成 `attachments: null`，
	// 而前端的类型声明是非空数组——于是「打开一封没有附件的邮件」会在
	// AttachmentList 里 null.filter 崩掉整棵 React 树。绝大多数邮件都没有附件，
	// 所以这条路径几乎是必经的。在这里补齐，两个协议实现就都不必各自记得。
	if detail != nil && detail.Attachments == nil {
		detail.Attachments = []mailer.AttachmentMeta{}
	}
	return detail, err
}

// Attachment 下载单个附件。
func (s *MessageService) Attachment(
	ctx context.Context, tenantID, accountID string,
	folder mailer.Folder, msgID, idMode, attID string,
) (*mailer.Attachment, error) {
	if folder == mailer.FolderAll || !mailer.ValidFolder(folder) {
		return nil, ErrInvalidFolder
	}
	account, cred, err := s.credential(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	client, record := s.clientFor(ctx, tenantID, account)
	att, err := client.Attachment(ctx, cred, folder, msgID, idMode, attID)
	record(err)
	return att, err
}

// MarkRead 批量标已读。
func (s *MessageService) MarkRead(
	ctx context.Context, tenantID, accountID string, items []mailer.MessageRef,
) (mailer.BatchResult, error) {
	result, err := s.batch(ctx, tenantID, accountID, items,
		func(c mailer.Client, cred mailer.Credential, refs []mailer.MessageRef) (mailer.BatchResult, error) {
			return c.MarkRead(ctx, cred, refs)
		})
	if err != nil {
		return result, err
	}
	s.adjustUnreadAfterBatch(ctx, tenantID, accountID, result.Items)
	return result, nil
}

// adjustUnreadAfterBatch 扣掉批量操作里成功处理掉的收件箱未读邮件。
//
// 标已读和删除都会让一封信从「未读」里消失。邮件本身由客户端在选中时标注
// WasUnread,服务层只做近似修正;下一次收信检查仍以服务端结果为准。
func (s *MessageService) adjustUnreadAfterBatch(
	ctx context.Context, tenantID, accountID string, items []mailer.ItemResult,
) {
	unread := 0
	for _, item := range items {
		if item.OK && item.Ref.WasUnread && item.Ref.Folder == mailer.FolderInbox {
			unread++
		}
	}
	if unread == 0 {
		return
	}
	if err := s.store.DecrementMailAccountUnread(ctxDetached(ctx), tenantID, accountID, unread); err != nil &&
		!errors.Is(err, repo.ErrNotFound) {
		slog.Warn("扣减未读数失败", "account_id", accountID, "error", err)
	}
}

// Delete 批量删除。
func (s *MessageService) Delete(
	ctx context.Context, tenantID, accountID string, items []mailer.MessageRef,
) (mailer.BatchResult, error) {
	result, err := s.batch(ctx, tenantID, accountID, items,
		func(c mailer.Client, cred mailer.Credential, refs []mailer.MessageRef) (mailer.BatchResult, error) {
			return c.Delete(ctx, cred, refs)
		})
	if err != nil {
		return result, err
	}
	s.adjustUnreadAfterBatch(ctx, tenantID, accountID, result.Items)
	return result, nil
}

func (s *MessageService) batch(
	ctx context.Context, tenantID, accountID string, items []mailer.MessageRef,
	run func(mailer.Client, mailer.Credential, []mailer.MessageRef) (mailer.BatchResult, error),
) (mailer.BatchResult, error) {
	if len(items) == 0 {
		return mailer.BatchResult{}, nil
	}
	for _, item := range items {
		if item.Folder == mailer.FolderAll || !mailer.ValidFolder(item.Folder) {
			return mailer.BatchResult{}, ErrInvalidFolder
		}
	}
	account, cred, err := s.credential(ctx, tenantID, accountID)
	if err != nil {
		return mailer.BatchResult{}, err
	}
	client, record := s.clientFor(ctx, tenantID, account)
	result, err := run(client, cred, items)
	record(err)
	if err == nil {
		// 列表内容已经变了(标已读/删除),缓存必须失效。
		s.invalidateMessageCache(tenantID, accountID)
	}
	return result, err
}

// credential 取出账号并解密成协议层要的 Credential。
//
// 明文凭据只在这里到 mailer 之间的内存里存在，绝不进日志、不进响应体。
func (s *MessageService) credential(
	ctx context.Context, tenantID, accountID string,
) (*model.MailAccount, mailer.Credential, error) {
	account, err := s.store.GetMailAccount(ctx, tenantID, accountID)
	if err != nil {
		return nil, mailer.Credential{}, err
	}
	if account.Status == model.AccountStatusBanned {
		// 已知被封的账号不该再打上游：每试一次都在加重风控。
		return nil, mailer.Credential{}, ErrAccountBanned
	}
	if account.Status == model.AccountStatusDisabled {
		return nil, mailer.Credential{}, ErrAccountDisabled
	}

	cred := mailer.Credential{
		Email:       account.Email,
		Provider:    account.Provider,
		AccountType: mailer.AccountType(account.AccountType),
		ClientID:    account.ClientID,
		IMAPHost:    account.IMAPHost,
		IMAPPort:    account.IMAPPort,
		AuthChannel: account.AuthChannel,
	}
	if account.Provider == "gmail" {
		if s.chainOptions.GoogleOAuthClientSecret != "" && account.ClientID == s.chainOptions.GoogleOAuthClientID {
			cred.ClientSecret = s.chainOptions.GoogleOAuthClientSecret
		}
	} else if s.chainOptions.OAuthClientSecret != "" && account.ClientID == s.chainOptions.OAuthClientID {
		cred.ClientSecret = s.chainOptions.OAuthClientSecret
	}
	for _, field := range []struct {
		enc string
		dst *string
	}{
		{account.PasswordEnc, &cred.Password},
		{account.RefreshTokenEnc, &cred.RefreshToken},
		{account.IMAPPasswordEnc, &cred.IMAPPassword},
	} {
		if field.enc == "" {
			continue
		}
		plain, err := s.cipher.Decrypt(field.enc)
		if err != nil {
			// 解密失败通常意味着 ENCRYPTION_KEY 换过了。必须报错而不是当成空值，
			// 否则表现成「密码错误」，用户会去改密码，越改越乱。
			return nil, mailer.Credential{}, ErrCredentialUndecryptable
		}
		*field.dst = plain
	}

	proxy, err := s.resolveProxy(ctx, tenantID, account)
	if err != nil {
		return nil, mailer.Credential{}, err
	}
	cred.Proxy = proxy
	return account, cred, nil
}

// resolveProxy 按「账号 → 分组」取生效的代理配置。
func (s *MessageService) resolveProxy(
	ctx context.Context, tenantID string, account *model.MailAccount,
) (mailer.ProxyConfig, error) {
	accountProxy, err := s.decryptProxy(mailer.ProxyConfig{
		URL:       account.ProxyURL,
		Fallback1: account.FallbackProxyURL1,
		Fallback2: account.FallbackProxyURL2,
	})
	if err != nil {
		return mailer.ProxyConfig{}, err
	}
	// 账号自己配了就整组用它的，不必再查分组。
	if strings.TrimSpace(accountProxy.URL) != "" {
		return accountProxy, nil
	}

	// 分组取不到就按「这个账号没有分组代理」处理：账号本身的配置已经在上面
	// 判过了，为了一次代理查询失败就让整封信取不回来并不划算。
	group, err := s.store.GetMailGroup(ctx, tenantID, account.GroupID)
	if err != nil {
		return mailer.ResolveProxy(accountProxy, nil), nil
	}
	groupProxy, err := s.decryptProxy(mailer.ProxyConfig{URL: group.ProxyURL})
	if err != nil {
		return mailer.ProxyConfig{}, err
	}
	return mailer.ResolveProxy(accountProxy, []mailer.ProxyConfig{groupProxy}), nil
}

// decryptProxy 解密代理三列。代理串常带认证口令，所以是加密存的。
func (s *MessageService) decryptProxy(cfg mailer.ProxyConfig) (mailer.ProxyConfig, error) {
	out := cfg
	for _, field := range []*string{&out.URL, &out.Fallback1, &out.Fallback2} {
		if *field == "" {
			continue
		}
		plain, err := s.cipher.Decrypt(*field)
		if err != nil {
			return mailer.ProxyConfig{}, ErrCredentialUndecryptable
		}
		*field = plain
	}
	return out, nil
}

// clientFor 返回该账号的回退链，以及一个记录本次访问结果的回调。
func (s *MessageService) clientFor(
	ctx context.Context, tenantID string, account *model.MailAccount,
) (mailer.Client, func(error)) {
	client := s.chainFor(account)
	record := func(callErr error) {
		s.writeResult(ctx, tenantID, account, callErr, true)
	}
	return client, record
}

// recordResult 写回一次令牌刷新的成败。
func (s *MessageService) recordResult(
	ctx context.Context, tenantID string, account *model.MailAccount, callErr error,
) {
	s.writeResult(ctx, tenantID, account, callErr, false)
}

// writeResult 写回本次访问的成败，reachedMailbox 表示这次调用是否真正登录了邮箱。
//
// 令牌刷新只证明令牌换得出来：它的成功不能把账号记成可用（那正是这个字段要纠正的误判），
// 只有「确定失效」的结论才允许写进健康状态。
// 写回失败只记日志：拉信本身已经成功了，不能因为记账失败就把结果丢掉。
func (s *MessageService) writeResult(
	ctx context.Context, tenantID string, account *model.MailAccount, callErr error, reachedMailbox bool,
) {
	status, message, errorKind := string(model.RefreshSuccess), "", ""
	if callErr != nil {
		status = string(model.RefreshFailed)
		message = truncateError(callErr.Error())
		errorKind = string(mailer.KindOf(callErr))
	}
	health, healthKind, verdict := healthOf(callErr)
	if !reachedMailbox && health != model.HealthInvalid {
		verdict = false
	}
	var err error
	if verdict {
		err = s.store.UpdateMailAccountAccessResult(ctx, tenantID, account.ID,
			status, message, errorKind, string(health), healthKind)
	} else {
		err = s.store.UpdateMailAccountRefreshResult(ctx, tenantID, account.ID, status, message, errorKind)
	}
	if err != nil {
		slog.Warn("写回账号访问结果失败", "account_id", account.ID, "error", err)
	}

	// 账号被封时立刻改状态，后续请求在 credential 里就会被挡下，不再打上游。
	if callErr != nil && mailer.KindOf(callErr) == mailer.ErrKindBanned {
		if _, err := s.store.BatchUpdateMailAccountStatus(ctxDetached(ctx), tenantID,
			string(model.AccountStatusBanned), []string{account.ID}); err != nil {
			slog.Warn("标记封禁账号失败", "account_id", account.ID, "error", err)
		}
	}
}

// OnChannelSuccess 供回退链的 OnSuccess 回调使用：把成功通道写回账号。
func (s *MessageService) OnChannelSuccess(
	ctx context.Context, tenantID, accountID, previousChannel, channel string,
) {
	if channel == "" || channel == previousChannel {
		return
	}
	if err := s.store.UpdateMailAccountAuthChannel(ctx, tenantID, accountID, channel); err != nil {
		slog.Warn("写回成功通道失败", "account_id", accountID, "error", err)
	}
}

// OnTokenRotated 供协议层的轮换回调使用：加密后持久化新的 refresh_token。
func (s *MessageService) OnTokenRotated(ctx context.Context, tenantID, accountID, refreshToken string) {
	enc, err := s.cipher.Encrypt(refreshToken)
	if err != nil {
		slog.Error("加密轮换后的 refresh_token 失败", "account_id", accountID, "error", err)
		return
	}
	if err := s.store.UpdateMailAccountRefreshToken(ctx, tenantID, accountID, enc); err != nil {
		slog.Error("持久化轮换后的 refresh_token 失败", "account_id", accountID, "error", err)
	}
}

func channelOf(err error, account *model.MailAccount) string {
	if err != nil {
		return ""
	}
	return account.AuthChannel
}

func nonNil(items []mailer.Message) []mailer.Message {
	if items == nil {
		return []mailer.Message{}
	}
	return items
}

const maxRecordedErrorLen = 500

func truncateError(s string) string {
	if len(s) <= maxRecordedErrorLen {
		return s
	}
	return s[:maxRecordedErrorLen]
}

// ctxDetached 在原 context 已取消时换一个干净的，
// 用于「必须落库」的收尾写入（比如标记封禁）。
func ctxDetached(ctx context.Context) context.Context {
	if ctx.Err() == nil {
		return ctx
	}
	return context.WithoutCancel(ctx)
}
