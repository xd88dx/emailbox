package graph

import (
	"context"
	"fmt"
	"net/url"

	"emailbox/pkg/mailer"
)

// graphPage 是 Graph 的分页响应。列表类接口只取 id,省掉其余字段。
type graphPage struct {
	Value []struct {
		ID string `json:"id"`
	} `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// UnreadCount 实现 mailer.UnreadCounter。
//
// unreadItemCount 是 Graph 维护的聚合值,不需要把邮件列出来再数。
func (c *Client) UnreadCount(ctx context.Context, cred mailer.Credential) (int, error) {
	return withSession(ctx, c, cred, func(ctx context.Context, s *session) (int, error) {
		count, err := c.inboxUnread(ctx, s)
		if err != nil {
			return 0, err
		}
		if count == nil {
			return 0, nil
		}
		return *count, nil
	})
}

// inboxUnread 在已有会话里取收件箱未读数，供列表顺带查询复用。
func (c *Client) inboxUnread(ctx context.Context, s *session) (*int, error) {
	var out struct {
		UnreadItemCount *int `json:"unreadItemCount"`
	}
	if err := c.doJSON(ctx, s, "GET",
		"/me/mailFolders/inbox?$select=unreadItemCount", nil, &out); err != nil {
		return nil, err
	}
	return out.UnreadItemCount, nil
}

// MarkAllRead 实现 mailer.AllReadMarker:遍历全部 mailFolders,
// 逐文件夹取出未读邮件并批量 PATCH isRead=true。
//
// Graph 没有「整箱标已读」的接口,只能先定位未读。按文件夹走而不是
// 一次查全账号,是为了避免把巨大的结果集拉回来再分页。
func (c *Client) MarkAllRead(ctx context.Context, cred mailer.Credential) error {
	_, err := withSession(ctx, c, cred, func(ctx context.Context, s *session) (struct{}, error) {
		folderIDs, err := c.listFolderIDs(ctx, s)
		if err != nil {
			return struct{}{}, err
		}
		for _, folderID := range folderIDs {
			refs, err := c.listUnreadRefs(ctx, s, folderID)
			if err != nil {
				return struct{}{}, err
			}
			if len(refs) == 0 {
				continue
			}
			result, err := c.batchOpSession(ctx, s, refs, "PATCH", map[string]any{"isRead": true})
			if err != nil {
				return struct{}{}, err
			}
			if result.Failed > 0 {
				return struct{}{}, newError(mailer.ErrKindProviderError,
					fmt.Sprintf("有 %d 封邮件标记已读失败", result.Failed), 0, nil)
			}
		}
		return struct{}{}, nil
	})
	return err
}

func (c *Client) listFolderIDs(ctx context.Context, s *session) ([]string, error) {
	path := "/me/mailFolders?$select=id&$top=100"
	var ids []string
	for path != "" {
		var page graphPage
		if err := c.doJSON(ctx, s, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Value {
			ids = append(ids, item.ID)
		}
		path = nextPath(page.NextLink)
	}
	return ids, nil
}

func (c *Client) listUnreadRefs(ctx context.Context, s *session, folderID string) ([]mailer.MessageRef, error) {
	path := "/me/mailFolders/" + url.PathEscape(folderID) +
		"/messages?$filter=isRead%20eq%20false&$select=id&$top=500"
	var refs []mailer.MessageRef
	for path != "" {
		var page graphPage
		if err := c.doJSON(ctx, s, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Value {
			refs = append(refs, mailer.MessageRef{ID: item.ID, IDMode: mailer.IDModeNone})
		}
		path = nextPath(page.NextLink)
	}
	return refs, nil
}

// nextPath 把 @odata.nextLink 的绝对地址转成 doJSON 需要的相对路径。
// doJSON 会自己拼接 BaseURL,直接传绝对地址会拼出错误 URL。
func nextPath(link string) string {
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.Path
	}
	return u.Path + "?" + u.RawQuery
}
