package imapx

import (
	"context"
	"strings"

	"emailbox/pkg/mailer"

	"github.com/emersion/go-imap/v2"
)

// UnreadCount 实现 mailer.UnreadCounter。
//
// 用 STATUS 而不是 SELECT + SEARCH UNSEEN:STATUS 是一条只读元数据命令,
// 不会像 SELECT 那样改变会话的「当前邮箱」状态,也不需要取回任何邮件内容。
func (c *Client) UnreadCount(ctx context.Context, cred mailer.Credential) (int, error) {
	return withSession(ctx, c, cred, func(_ context.Context, s *session) (int, error) {
		data, err := s.imap.Status("INBOX", &imap.StatusOptions{NumUnseen: true}).Wait()
		if err != nil {
			return 0, errorf(c.cfg.Channel, mailer.ErrKindProviderError, err, "查询收件箱未读数失败")
		}
		// 服务商没有回 UNSEEN 时按 0 处理:缺这个数字不影响账号可用性判断。
		if data.NumUnseen == nil {
			return 0, nil
		}
		return int(*data.NumUnseen), nil
	})
}

// MarkAllRead 实现 mailer.AllReadMarker:遍历所有可选文件夹,把邮件标为 \Seen。
//
// 逐个文件夹处理而不是只查收件箱——用户要的是「选中邮箱里所有邮件」。
// 单个文件夹失败不中止其余文件夹,但最后必须报错:部分成功如果被当成成功,
// 用户会以为整个账号都清完了。
func (c *Client) MarkAllRead(ctx context.Context, cred mailer.Credential) error {
	_, err := withSession(ctx, c, cred, func(_ context.Context, s *session) (struct{}, error) {
		entries, err := s.imap.List("", "*", nil).Collect()
		if err != nil {
			return struct{}{}, errorf(c.cfg.Channel, mailer.ErrKindProviderError, err, "列出邮箱文件夹失败")
		}

		var failed []string
		var lastErr error
		for _, entry := range entries {
			// \Noselect 的条目只是层级占位,不是真实邮箱,SELECT 一定失败。
			if hasMailboxAttr(entry.Attrs, imap.MailboxAttrNoSelect) {
				continue
			}
			data, err := s.imap.Select(entry.Mailbox, nil).Wait()
			if err != nil {
				failed = append(failed, entry.Mailbox)
				lastErr = err
				continue
			}
			// 空文件夹对 1:* 的行为各家不一,直接跳过最省事。
			if data.NumMessages == 0 {
				continue
			}
			var seq imap.SeqSet
			seq.AddRange(1, 0) // 1:*
			if err := s.imap.Store(seq, &imap.StoreFlags{
				Op:     imap.StoreFlagsAdd,
				Silent: true,
				Flags:  []imap.Flag{imap.FlagSeen},
			}, nil).Close(); err != nil {
				failed = append(failed, entry.Mailbox)
				lastErr = err
			}
		}

		if len(failed) > 0 {
			e := errorf(c.cfg.Channel, mailer.ErrKindProviderError, lastErr,
				"有 %d 个文件夹标记已读失败", len(failed))
			e.Detail = strings.Join(failed, ", ")
			return struct{}{}, e
		}
		return struct{}{}, nil
	})
	return err
}

func hasMailboxAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if attr == want {
			return true
		}
	}
	return false
}
