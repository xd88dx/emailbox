package imapx

import (
	"sync"
	"time"
)

// SessionPool 复用已登录的 IMAP 会话。
//
// 列表慢的主因是每个 HTTP 请求都重走一次 TCP + TLS + LOGIN:在经代理、
// 跨地域的网络里这几步是秒级的固定开销,而 IMAP 连接本身可以执行任意多次命令,
// 直到服务端空闲断开。去掉这段固定开销,重复打开同一邮箱才会真的快起来。
//
// 池不是全局变量,由 service 层持有并随 Client 注入。键里带账号作用域,
// 不同账号绝不会共用同一条连接——那会让 A 账号的信出现在 B 账号的列表里。
type SessionPool struct {
	idleTTL     time.Duration
	maxSessions int
	maxFolders  int

	mu      sync.Mutex
	idle    map[string][]idleSession
	total   int
	folders map[string]folderEntry
	// closed 之后不再接收归还的会话,避免退出途中还有在途请求把连接放回池里。
	closed bool
}

type idleSession struct {
	session  *session
	lastUsed time.Time
}

type folderEntry struct {
	name    string
	expires time.Time
}

// PoolConfig 是会话池配置。零值使用默认值。
type PoolConfig struct {
	// IdleTTL 是空闲会话的保留时长。短于服务商的空闲断开时间即可;
	// 猜错也不致命:连接已死时下一条命令会失败,调用方会重连一次。
	IdleTTL time.Duration
	// MaxSessions 是同时保留的空闲会话数上限。
	MaxSessions int
}

const (
	defaultIdleTTL     = 5 * time.Minute
	defaultMaxSessions = 64
	folderCacheTTL     = time.Hour
	defaultMaxFolders  = 512
)

// NewSessionPool 构造会话池。
func NewSessionPool(cfg PoolConfig) *SessionPool {
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = defaultIdleTTL
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	return &SessionPool{
		idleTTL:     cfg.IdleTTL,
		maxSessions: cfg.MaxSessions,
		maxFolders:  defaultMaxFolders,
		idle:        make(map[string][]idleSession),
		folders:     make(map[string]folderEntry),
	}
}

// acquire 取一条空闲会话并标记为已占用。没有可用会话时返回 nil。
//
// 取最近使用的那条:越新越可能还没被服务端断开。超过 IdleTTL 的直接关掉,
// 避免把「大概已经死了」的连接交出去。
func (p *SessionPool) acquire(key string) *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.idle[key]
	for len(list) > 0 {
		entry := list[len(list)-1]
		list = list[:len(list)-1]
		p.total--
		if time.Since(entry.lastUsed) > p.idleTTL {
			entry.session.close()
			continue
		}
		p.idle[key] = list
		entry.session.reused = true
		return entry.session
	}
	delete(p.idle, key)
	return nil
}

// release 把用完的会话放回池里。超过上限时直接关闭。
func (p *SessionPool) release(key string, s *session) {
	p.mu.Lock()
	if p.closed || p.total >= p.maxSessions {
		p.mu.Unlock()
		s.close()
		return
	}
	p.total++
	p.idle[key] = append(p.idle[key], idleSession{session: s, lastUsed: time.Now()})
	p.mu.Unlock()
}

// discard 关闭一条出过错的会话,不再复用。
func (p *SessionPool) discard(s *session) { s.close() }

// Close 关闭全部空闲会话。服务退出时调用,让 go-imap 的读协程一并结束。
func (p *SessionPool) Close() {
	p.mu.Lock()
	p.closed = true
	all := make([]*session, 0, p.total)
	for _, list := range p.idle {
		for _, entry := range list {
			all = append(all, entry.session)
		}
	}
	p.idle = make(map[string][]idleSession)
	p.folders = make(map[string]folderEntry)
	p.total = 0
	p.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

// folderFor 返回上次解析成功的邮箱名。空串表示没有缓存。
//
// 选文件夹时逐个试候选名,每试一个都是一次完整往返;收件箱通常第一个就中,
// 垃圾箱这类不常命中的名字却可能连续试错好几次。
func (p *SessionPool) folderFor(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.folders[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(p.folders, key)
		}
		return ""
	}
	return entry.name
}

func (p *SessionPool) rememberFolder(key, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.folders) >= p.maxFolders {
		p.folders = make(map[string]folderEntry)
	}
	p.folders[key] = folderEntry{name: name, expires: time.Now().Add(folderCacheTTL)}
}

func (p *SessionPool) forgetFolder(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.folders, key)
}
