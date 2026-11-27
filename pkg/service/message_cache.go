package service

import (
	"fmt"
	"time"

	"emailbox/pkg/mailer"
)

// messageCacheTTL 是收件箱首页的缓存时长。
//
// 打开/切换邮箱是自动行为,同一分钟内重复进入只该看到同一份结果;
// 每开一次都打一次上游既慢又白扣取件额度。右上角的手动刷新可以绕过它。
const messageCacheTTL = time.Minute

// messageCacheMaxEntries 限制缓存规模。只缓存首页,单个条目就是 25 封邮件的
// 元数据;这个上限对内存足够宽裕,又能防住「几千个账号全被打开过」的场景。
const messageCacheMaxEntries = 512

type messageCacheEntry struct {
	result  MessageListResult
	expires time.Time
}

func messageCacheKey(tenantID, accountID string, opt messageListCacheOptions) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d", tenantID, accountID, opt.folder, opt.skip, opt.top)
}

// messageListCacheOptions 是缓存键里用得到的列表参数。
type messageListCacheOptions struct {
	folder string
	skip   int
	top    int
}

func (s *MessageService) cacheGet(key string) (*MessageListResult, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(s.cache, key)
		}
		return nil, false
	}
	result := entry.result
	return &result, true
}

func (s *MessageService) cachePut(key string, result *MessageListResult) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(s.cache) >= messageCacheMaxEntries {
		now := time.Now()
		for k, entry := range s.cache {
			if now.After(entry.expires) {
				delete(s.cache, k)
			}
		}
		// 清掉过期项仍然满,就丢一个旧条目。缓存本来只是省请求的手段,
		// 不值得为「丢哪个」引入 LRU 的复杂度。
		for k := range s.cache {
			if len(s.cache) < messageCacheMaxEntries {
				break
			}
			delete(s.cache, k)
		}
	}
	s.cache[key] = messageCacheEntry{result: *result, expires: time.Now().Add(messageCacheTTL)}
}

// invalidateMessageCache 丢掉某个账号的全部列表缓存。
// 标已读/删除之后列表内容已经变了,继续返回旧列表会让用户以为操作没生效。
func (s *MessageService) invalidateMessageCache(tenantID, accountID string) {
	prefix := tenantID + "|" + accountID + "|"
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for key := range s.cache {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(s.cache, key)
		}
	}
}

// 单文件夹首页的缓存键。folder=all 归并时顺带写回这两个键，
// 之后切到「收件箱」就能直接命中，不必为同一批邮件再打一次上游。
func (s *MessageService) folderCacheKey(
	tenantID, accountID string, folder mailer.Folder, top int,
) string {
	return messageCacheKey(tenantID, accountID, messageListCacheOptions{
		folder: string(folder), skip: 0, top: top,
	})
}

// cacheGetAllFolders 取回 folder=all 归并所需的两个单文件夹缓存。
//
// 两片都在才算命中：只拿到一片时照样要打上游，而打上游后另一片也会被写回，
// 所以「部分命中」没有可利用的价值。
func (s *MessageService) cacheGetAllFolders(
	tenantID, accountID string, top int,
) ([]mailer.Message, []mailer.Message, bool) {
	inbox, ok := s.cacheGet(s.folderCacheKey(tenantID, accountID, mailer.FolderInbox, top))
	if !ok {
		return nil, nil, false
	}
	junk, ok := s.cacheGet(s.folderCacheKey(tenantID, accountID, mailer.FolderJunk, top))
	if !ok {
		return nil, nil, false
	}
	return inbox.Items, junk.Items, true
}
