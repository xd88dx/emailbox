package service

import (
	"fmt"
	"time"
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
