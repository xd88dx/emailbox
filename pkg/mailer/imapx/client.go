package imapx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"emailbox/pkg/mailer"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const defaultTimeout = 60 * time.Second

// Config 是 IMAP 通道的配置。
type Config struct {
	// Channel 决定通道形态：imap（密码鉴权）/ imap_new / imap_old / imap_gmail（OAuth）。
	Channel string
	// TokenURL 留空时按 Channel 选生产端点；测试用它覆盖。
	TokenURL string
	Timeout  time.Duration
	// OnTokenRefresh 在服务商返回新的 refresh_token 时调用。
	OnTokenRefresh func(email, refreshToken string)
	// DialFunc 供测试注入进程内的 IMAP 服务器。留空时按代理配置真实拨号。
	DialFunc func(ctx context.Context, host string, port int, proxyURL string) (net.Conn, error)
	// TLSLegacy 打开静态 RSA 套件,用于只支持旧式 TLS 的服务商(如新浪 993)。
	TLSLegacy bool
	// Pool 复用已登录的会话。nil 表示每次请求重新建连(测试与 mailprobe 走这条)。
	Pool *SessionPool
	// PoolScope 让池按账号隔离会话,形如 tenantID|accountID。
	// 为空时即使配了 Pool 也不复用:没有作用域就可能让不同账号共用一条连接。
	PoolScope string
}

// Client 是 IMAP 通道的实现。
type Client struct {
	cfg Config
}

var _ mailer.Client = (*Client)(nil)

// New 构造 IMAP 通道。channel 取 mailer.ChannelIMAP / ChannelIMAPNew / ChannelIMAPOld。
func New(cfg Config) *Client {
	if cfg.Channel == "" {
		cfg.Channel = mailer.ChannelIMAP
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	return &Client{cfg: cfg}
}

// Channel 实现 mailer.Client。
func (c *Client) Channel() string { return c.cfg.Channel }

// session 是一条已经登录好的 IMAP 连接。
type session struct {
	imap  *imapclient.Client
	proxy string
	// uidCapable 表示该连接上可以用 UID 命令。几乎所有服务器都支持，
	// 但这个标记要如实带到 Message.IDMode 上——混用 UID 与序列号会取到错误的邮件。
	uidCapable bool
	// reused 表示这条会话来自池,本次没有重新握手。只用于计时日志。
	reused bool
}

func (s *session) close() {
	if s.imap == nil {
		return
	}
	// LOGOUT 失败无所谓，紧接着的 Close 会把连接收掉；
	// 连接已经断了的情况下报错反而是常态。
	//nolint:errcheck // 故意丢弃：连接就要关了，LOGOUT 的结果没有意义
	s.imap.Logout().Wait()
	_ = s.imap.Close()
}

func (s *session) idMode() string {
	if s.uidCapable {
		return mailer.IDModeUID
	}
	return mailer.IDModeSequence
}

// withSession 依次尝试代理候选，在每个候选上建连接、鉴权，然后执行 fn。
func withSession[T any](
	ctx context.Context, c *Client, cred mailer.Credential,
	fn func(context.Context, *session) (T, error),
) (T, error) {
	return withProxy(ctx, c, cred, func(proxyURL string) (T, error) {
		return runOnProxy(ctx, c, cred, proxyURL, fn)
	})
}

// runOnProxy 在一个代理候选上建连接并执行 fn。
// 写成自由函数是因为 Go 不允许方法带类型参数。
func runOnProxy[T any](
	ctx context.Context, c *Client, cred mailer.Credential, proxyURL string,
	fn func(context.Context, *session) (T, error),
) (T, error) {
	var zero T
	key := ""
	if c.cfg.Pool != nil && c.cfg.PoolScope != "" {
		key = c.poolKey(cred, proxyURL)
		// 池里还有会话就直接用;连接已死时 runPooled 返回 usable=false,
		// 这里新建一条再执行一次——只重试一次,避免坏账号把请求拖成多倍耗时。
		if out, usable, err := runPooled(ctx, c, key, fn); usable {
			return out, err
		}
	}
	s, err := c.connect(ctx, cred, proxyURL)
	if err != nil {
		return zero, err
	}
	out, err := fn(ctx, s)
	if key != "" && err == nil {
		c.cfg.Pool.release(key, s)
		return out, nil
	}
	// 出过错或没启用池:这条连接不再复用。
	s.close()
	return out, err
}

// connect 建立连接、发 ID、鉴权。
func (c *Client) connect(ctx context.Context, cred mailer.Credential, proxyURL string) (*session, error) {
	start := time.Now()
	host, port := imapServer(c.cfg.Channel, cred)
	if host == "" {
		return nil, newError(c.cfg.Channel, mailer.ErrKindProviderError,
			"账号没有 IMAP 主机，且无法从邮箱域名推断", nil)
	}

	conn, err := c.dial(ctx, host, port, proxyURL)
	if err != nil {
		return nil, err
	}
	dialMS := time.Since(start).Milliseconds()

	client := imapclient.New(conn, &imapclient.Options{WordDecoder: wordDecoder})
	s := &session{imap: client, proxy: proxyURL}

	identifyStart := time.Now()
	c.identify(client)
	identifyMS := time.Since(identifyStart).Milliseconds()
	authStart := time.Now()
	if err := c.authenticate(ctx, client, cred, proxyURL); err != nil {
		s.close()
		return nil, err
	}
	s.uidCapable = true
	// 建连耗时是「打开邮箱慢」最主要的固定成本,按阶段分开记:
	// dial 含 TCP + TLS + 代理握手,identify 只对要求 IMAP ID 的服务器存在,
	// auth 是 LOGIN/XOAUTH2。三项仍占大头时该做会话复用,而不是调大超时。
	slog.Info("IMAP 建连完成",
		"channel", c.cfg.Channel,
		"email", mailer.MaskEmail(cred.Email),
		"dial_ms", dialMS,
		"identify_ms", identifyMS,
		"auth_ms", time.Since(authStart).Milliseconds(),
		"total_ms", time.Since(start).Milliseconds(),
	)
	return s, nil
}

// runPooled 尝试复用池里的会话。
//
// usable=false 有两种含义:池里没有空闲会话,或者取到的会话已经失效。
// 两种都由调用方新建一条连接,因此这里不需要区分。
func runPooled[T any](
	ctx context.Context, c *Client, key string,
	fn func(context.Context, *session) (T, error),
) (T, bool, error) {
	var zero T
	s := c.cfg.Pool.acquire(key)
	if s == nil {
		return zero, false, nil
	}
	out, err := fn(ctx, s)
	if err == nil {
		c.cfg.Pool.release(key, s)
		return out, true, nil
	}
	// 出过错的会话状态不明,不能再放回池里。
	c.cfg.Pool.discard(s)
	if !sessionDead(err) {
		return zero, true, err
	}
	slog.Debug("IMAP 会话已失效,重新建连", "channel", c.cfg.Channel, "error", err)
	return zero, false, nil
}

// poolKey 按「账号作用域 + 通道 + 主机 + 出口代理」隔离会话。
//
// 代理必须进键:同一个账号换到另一条代理等于换了一个出口身份,
// 复用旧连接会让 {mail} 模板失去意义。
func (c *Client) poolKey(cred mailer.Credential, proxyURL string) string {
	host, port := imapServer(c.cfg.Channel, cred)
	return fmt.Sprintf("%s\x00%s\x00%s:%d\x00%s", c.cfg.PoolScope, c.cfg.Channel, host, port, proxyURL)
}

// sessionDead 判断错误是否意味着这条连接已经不能再用。
//
// 只认网络层的断裂:业务错误(邮件不存在、文件夹不可用、鉴权失败)重连也是同样结果,
// 重试只会多打一次上游。调用方取消同样不重试。
func sessionDead(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection closed", "client closed", "broken pipe",
		"use of closed network connection", "unexpected eof",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func (c *Client) dial(ctx context.Context, host string, port int, proxyURL string) (net.Conn, error) {
	if c.cfg.DialFunc != nil {
		return c.cfg.DialFunc(ctx, host, port, proxyURL)
	}
	dialer, err := mailer.NewDialer(proxyURL, c.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	return mailer.DialTLSWith(ctx, dialer, host, port, mailer.TLSOptions{LegacyCiphers: c.cfg.TLSLegacy})
}

// identify 发送 IMAP ID 命令。
//
// 163/126 强制要求客户端先发 ID，否则报 Unsafe Login。发失败不算致命错误：
// 不支持 ID 的服务器会回 BAD，而它们本来也不需要这条命令。
func (c *Client) identify(client *imapclient.Client) {
	if !client.Caps().Has(imap.CapID) {
		return
	}
	// 只记不拦：不支持 ID 的服务器会回 BAD，而它们本来也不需要这条命令；
	// 真正需要 ID 的服务器会在后面的 LOGIN 上报 Unsafe Login，那条错误更有指向性。
	if _, err := client.ID(&imap.IDData{
		Name:    "emailbox",
		Version: appVersion,
		Vendor:  "emailbox",
	}).Wait(); err != nil {
		slog.Debug("IMAP ID 命令失败，继续鉴权",
			"channel", c.cfg.Channel, "error", err)
	}
}

// appVersion 会出现在 IMAP ID 里，服务商用它做客户端识别。
const appVersion = "1.0"

func (c *Client) authenticate(
	ctx context.Context, client *imapclient.Client, cred mailer.Credential, proxyURL string,
) error {
	if c.cfg.Channel == mailer.ChannelIMAP {
		password := cred.IMAPPassword
		if strings.TrimSpace(password) == "" {
			password = cred.Password
		}
		if strings.TrimSpace(password) == "" {
			return newError(c.cfg.Channel, mailer.ErrKindAuthFailed, "账号没有 IMAP 密码或授权码", nil)
		}
		if err := client.Login(cred.Email, password).Wait(); err != nil {
			return classifyAuthError(c.cfg.Channel, err)
		}
		return nil
	}

	// OAuth 通道：先换 access token，再走 XOAUTH2。
	hc, err := mailer.NewHTTPClient(proxyURL, c.cfg.Timeout)
	if err != nil {
		return err
	}
	token, err := c.fetchAccessToken(ctx, hc, cred)
	if err != nil {
		return err
	}
	if err := client.Authenticate(&xoauth2Client{username: cred.Email, token: token}); err != nil {
		return classifyAuthError(c.cfg.Channel, err)
	}
	return nil
}

// classifyAuthError 把各家五花八门的鉴权错误翻译成可操作的中文文案。
func classifyAuthError(channel string, err error) error {
	kind, message := mailer.ClassifyIMAPAuthError(err.Error())
	return newError(channel, kind, message, err)
}
