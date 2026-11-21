package translate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 分块与熔断参数。
//
// 4k 字符是「一次请求的延迟与失败重试成本」之间的折中:绝大多数邮件落在一块里,
// 长邮件按段落切开、3 路并发,总延迟接近最慢的一块而不是各块之和。
const (
	chunkSize        = 4000
	maxChunks        = 8
	chunkConcurrency = 3
	cacheTTL         = 10 * time.Minute
	cacheMaxEntries  = 256
	breakerCooldown  = time.Minute
)

var (
	// ErrUnavailable 表示首选与兜底引擎都没成功。handler 据此映射业务码。
	ErrUnavailable = errors.New("翻译服务暂时不可用")
	// ErrEmptyText 是邮件没有可翻译的文本正文。
	ErrEmptyText = errors.New("这封邮件没有可翻译的正文")
)

// Result 是一次翻译的返回体。
type Result struct {
	Text      string `json:"text"`
	Engine    string `json:"engine"`
	Truncated bool   `json:"truncated"`
	Cached    bool   `json:"cached"`
}

// Engine 是一个翻译引擎。目标语言固定为简体中文,不做多语言参数化:
// 多一个 dim 就多一份缓存键与提示词分支,而当前产品只需要中文。
type Engine interface {
	Name() string
	// MaxSingle 是单次请求能接受的字符数,超过后由 Chain 分块。
	MaxSingle() int
	Translate(ctx context.Context, text string) (string, error)
}

type cacheEntry struct {
	result  Result
	expires time.Time
}

// Chain 按「首选 → 兜底」的顺序翻译,并维护短期熔断与结果缓存。
//
// 熔断是必要的:模型端点不可达时每次点击都等满超时,比直接走兜底还慢。
// 首选失败后的 60 秒内跳过它,期间只打兜底。
type Chain struct {
	primary  Engine
	fallback Engine

	mu        sync.Mutex
	openUntil time.Time
	cache     map[string]cacheEntry
}

func NewChain(primary, fallback Engine) *Chain {
	return &Chain{primary: primary, fallback: fallback, cache: make(map[string]cacheEntry)}
}

func (c *Chain) Enabled() bool { return c.primary != nil || c.fallback != nil }

// Event 是翻译流的一次输出。Final 表示这是最高质量引擎的结果,不会再被替换;
// Err 非空表示这次没有拿到译文。
type Event struct {
	Result Result
	Final  bool
	Err    error
}

// Stream 并行调用快引擎与高质量引擎,先到的先发。
//
// 快引擎(传统翻译)通常一两秒就返回,先让用户看到译文;高质量引擎(大模型)
// 返回后用它的结果替换。两者的先后不作假设:大模型先到就直接发最终结果并
// 取消快引擎;大模型失败时保留快引擎的结果——有译文总比一个错误提示好。
//
// 通道在流结束时关闭;ctx 取消(用户切走、关页面)会中止所有外部调用。
func (c *Chain) Stream(ctx context.Context, text string) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)
		c.stream(ctx, text, out)
	}()
	return out
}

func (c *Chain) stream(ctx context.Context, text string, out chan<- Event) {
	text = strings.TrimSpace(text)
	if text == "" {
		out <- Event{Err: ErrEmptyText}
		return
	}
	key := cacheKey(text)

	quality := c.primary
	if quality != nil && c.primaryCooling() {
		quality = nil
	}
	hit, hasHit := c.cached(key)
	if hasHit {
		hit.Cached = true
		if quality == nil || hit.Engine == quality.Name() {
			out <- Event{Result: hit, Final: true}
			return
		}
		// 缓存里只是快引擎的结果:先发出去,再试着升级。
		out <- Event{Result: hit, Final: false}
	}

	text, truncated := limitText(text)
	switch {
	case quality == nil && c.fallback == nil:
		if !hasHit {
			out <- Event{Err: ErrUnavailable}
		}
	case quality == nil:
		c.emit(ctx, out, c.fallback, key, text, truncated, true)
	case c.fallback == nil:
		c.emit(ctx, out, quality, key, text, truncated, true)
	default:
		c.streamBoth(ctx, out, key, text, truncated, quality, hasHit)
	}
}

// emit 只跑一个引擎。只有一个引擎可用时,它的结果就是最终结果。
func (c *Chain) emit(ctx context.Context, out chan<- Event, e Engine, key, text string,
	truncated bool, final bool) {
	translated, err := translateWith(ctx, e, text)
	if err != nil {
		if ctx.Err() == nil {
			if e == c.primary {
				c.openBreaker()
			}
			slog.Warn("翻译引擎失败", "engine", e.Name(), "error", err)
		}
		out <- Event{Err: errors.Join(ErrUnavailable, err)}
		return
	}
	if e == c.primary {
		c.clearCooldown()
	}
	res := Result{Text: translated, Engine: e.Name(), Truncated: truncated}
	c.remember(key, res)
	out <- Event{Result: res, Final: final}
}

// streamBoth 同时跑两个引擎。快引擎结果先发(Final=false),大模型结果到了
// 再发 Final=true;大模型先到则只发最终结果并取消快引擎。
func (c *Chain) streamBoth(ctx context.Context, out chan<- Event, key, text string,
	truncated bool, quality Engine, fastCached bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		res     Result
		err     error
		quality bool
	}
	results := make(chan result, 2)
	pending := 1
	go func() {
		translated, err := translateWith(ctx, quality, text)
		results <- result{
			res:     Result{Text: translated, Engine: quality.Name(), Truncated: truncated},
			err:     err,
			quality: true,
		}
	}()
	if !fastCached {
		pending++
		go func() {
			translated, err := translateWith(ctx, c.fallback, text)
			results <- result{res: Result{
				Text: translated, Engine: c.fallback.Name(), Truncated: truncated,
			}, err: err}
		}()
	}

	var errs []error
	delivered := fastCached
	for i := 0; i < pending; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				errs = append(errs, r.err)
				if ctx.Err() == nil {
					slog.Warn("翻译引擎失败", "engine", r.res.Engine,
						"quality", r.quality, "error", r.err)
					if r.quality {
						c.openBreaker()
					}
				}
				continue
			}
			if r.quality {
				c.clearCooldown()
				c.remember(key, r.res)
				out <- Event{Result: r.res, Final: true}
				return
			}
			delivered = true
			c.remember(key, r.res)
			out <- Event{Result: r.res, Final: false}
		case <-ctx.Done():
			return
		}
	}
	if !delivered {
		out <- Event{Err: errors.Join(ErrUnavailable, errors.Join(errs...))}
	}
}

// translateWith 按引擎能力分块并行翻译,再按原顺序拼回。
// 任何一块失败都让整次翻译失败——半中文半英文的结果比报错更糟。
func translateWith(ctx context.Context, e Engine, text string) (string, error) {
	if utf8.RuneCountInString(text) <= e.MaxSingle() {
		return e.Translate(ctx, text)
	}
	chunks := splitText(text, chunkSize)
	if len(chunks) == 1 {
		return e.Translate(ctx, chunks[0])
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]string, len(chunks))
	errs := make([]error, len(chunks))
	sem := make(chan struct{}, chunkConcurrency)
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		wg.Add(1)
		go func(i int, chunk string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			out[i], errs[i] = e.Translate(ctx, chunk)
			if errs[i] != nil {
				cancel()
			}
		}(i, chunk)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return "", err
		}
	}
	return strings.Join(out, "\n\n"), nil
}

// limitText 丢弃超出 maxChunks 的尾部。上限存在的意义是给一次点击设成本天花板,
// 而不是让一封十万字的邮件把翻译账单拉爆。
func limitText(text string) (string, bool) {
	limit := chunkSize * maxChunks
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	return string([]rune(text)[:limit]), true
}

// splitText 先按行把文本切成不超过 size 的块,再把短行合并。
func splitText(text string, size int) []string {
	lines := make([]string, 0, 32)
	for _, line := range strings.Split(text, "\n") {
		if utf8.RuneCountInString(line) <= size {
			lines = append(lines, line)
			continue
		}
		runes := []rune(line)
		for len(runes) > size {
			lines = append(lines, string(runes[:size]))
			runes = runes[size:]
		}
		lines = append(lines, string(runes))
	}

	out := make([]string, 0, 4)
	var cur strings.Builder
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			out = append(out, strings.TrimSpace(cur.String()))
		}
		cur.Reset()
	}
	for _, line := range lines {
		if cur.Len() > 0 && utf8.RuneCountInString(cur.String())+utf8.RuneCountInString(line)+1 > size {
			flush()
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	flush()
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

func cacheKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (c *Chain) cached(key string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(c.cache, key)
		}
		return Result{}, false
	}
	return entry.result, true
}

func (c *Chain) remember(key string, result Result) Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= cacheMaxEntries {
		now := time.Now()
		for k, entry := range c.cache {
			if now.After(entry.expires) {
				delete(c.cache, k)
			}
		}
		for k := range c.cache {
			if len(c.cache) < cacheMaxEntries {
				break
			}
			delete(c.cache, k)
		}
	}
	c.cache[key] = cacheEntry{result: result, expires: time.Now().Add(cacheTTL)}
	return result
}

func (c *Chain) primaryCooling() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.openUntil)
}

func (c *Chain) openBreaker() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openUntil = time.Now().Add(breakerCooldown)
}

func (c *Chain) clearCooldown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openUntil = time.Time{}
}
