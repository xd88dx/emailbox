package imapx

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emailbox/pkg/mailer"
)

// dialCounter 返回一个会计数的 DialFunc,并把建立的连接交给测试保管。
func dialCounter(ts *testServer, dials *atomic.Int32) (func(context.Context, string, int, string) (net.Conn, error), func() []net.Conn) {
	var mu sync.Mutex
	var conns []net.Conn
	dial := func(ctx context.Context, _ string, _ int, _ string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", ts.addr)
		if err == nil {
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
		return conn, err
	}
	snapshot := func() []net.Conn {
		mu.Lock()
		defer mu.Unlock()
		return append([]net.Conn(nil), conns...)
	}
	return dial, snapshot
}

func pooledClient(ts *testServer, pool *SessionPool, scope string, dial func(context.Context, string, int, string) (net.Conn, error)) *Client {
	return New(Config{
		Channel:   mailer.ChannelIMAP,
		Timeout:   5 * time.Second,
		Pool:      pool,
		PoolScope: scope,
		DialFunc:  dial,
	})
}

func listOnce(t *testing.T, c *Client) {
	t.Helper()
	if _, err := c.List(context.Background(), testCred(),
		mailer.ListOptions{Folder: mailer.FolderInbox, Top: 5}); err != nil {
		t.Fatal(err)
	}
}

// 同一账号连续两次取列表只该握手一次。这是会话复用的核心收益:
// 省掉的正是 TCP + TLS + LOGIN 这段固定开销。
func TestSessionPoolReusesConnection(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"INBOX": {makeMessage("a", "1")}})
	var dials atomic.Int32
	dial, _ := dialCounter(ts, &dials)
	pool := NewSessionPool(PoolConfig{})
	t.Cleanup(pool.Close)
	c := pooledClient(ts, pool, "tenant|account-a", dial)

	listOnce(t, c)
	listOnce(t, c)

	if got := dials.Load(); got != 1 {
		t.Fatalf("两次取列表拨号 %d 次,期望 1 次", got)
	}
}

// 作用域不同绝不能共用会话,否则别人的邮件会出现在当前账号的列表里。
func TestSessionPoolIsolatesScopes(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"INBOX": {makeMessage("a", "1")}})
	var dials atomic.Int32
	dial, _ := dialCounter(ts, &dials)
	pool := NewSessionPool(PoolConfig{})
	t.Cleanup(pool.Close)

	listOnce(t, pooledClient(ts, pool, "tenant|account-a", dial))
	listOnce(t, pooledClient(ts, pool, "tenant|account-b", dial))

	if got := dials.Load(); got != 2 {
		t.Fatalf("两个账号拨号 %d 次,期望各建一条共 2 次", got)
	}
}

// 没有作用域时即便配了池也不复用——没有隔离依据就不该冒串号风险。
func TestSessionPoolRequiresScope(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"INBOX": {makeMessage("a", "1")}})
	var dials atomic.Int32
	dial, _ := dialCounter(ts, &dials)
	pool := NewSessionPool(PoolConfig{})
	t.Cleanup(pool.Close)
	c := pooledClient(ts, pool, "", dial)

	listOnce(t, c)
	listOnce(t, c)

	if got := dials.Load(); got != 2 {
		t.Fatalf("空作用域下拨号 %d 次,期望每次都新建(2 次)", got)
	}
}

// 服务端或代理把空闲连接断掉是常态。取到死会话时必须自己重连一次,
// 而不是把「连接已关闭」直接抛给用户。
func TestSessionPoolReconnectsAfterConnectionDropped(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"INBOX": {makeMessage("a", "1")}})
	var dials atomic.Int32
	dial, conns := dialCounter(ts, &dials)
	pool := NewSessionPool(PoolConfig{})
	t.Cleanup(pool.Close)
	c := pooledClient(ts, pool, "tenant|account-a", dial)

	listOnce(t, c)
	first := conns()
	if len(first) != 1 {
		t.Fatalf("第一次应只建立一条连接,实际 %d", len(first))
	}
	// 模拟服务端断开:池里那条会话已经不能再用。
	_ = first[0].Close()

	listOnce(t, c)

	if got := dials.Load(); got != 2 {
		t.Fatalf("会话断开后拨号 %d 次,期望重连一次共 2 次", got)
	}
}
