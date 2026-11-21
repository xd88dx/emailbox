package translate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeEngine struct {
	name  string
	max   int
	err   error
	delay time.Duration
	calls atomic.Int32
	fn    func(string) string
}

func (f *fakeEngine) Name() string   { return f.name }
func (f *fakeEngine) MaxSingle() int { return f.max }
func (f *fakeEngine) Translate(ctx context.Context, text string) (string, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if f.fn != nil {
		return f.fn(text), nil
	}
	return "译:" + text, nil
}

// collect 把一次流的全部事件收成切片。
func collect(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// 大模型失败时,快引擎的结果照样发出去,且不跟着一个错误事件。
func TestStreamKeepsFastResultWhenQualityFails(t *testing.T) {
	primary := &fakeEngine{name: "llm", max: 6000, err: errors.New("network down")}
	fallback := &fakeEngine{name: "libretranslate", max: 5000}
	chain := NewChain(primary, fallback)

	events := collect(t, chain.Stream(context.Background(), "hello"))
	if len(events) != 1 || events[0].Err != nil {
		t.Fatalf("事件 = %+v", events)
	}
	if events[0].Result.Engine != "libretranslate" || events[0].Result.Text != "译:hello" {
		t.Fatalf("结果 = %+v", events[0].Result)
	}
	if events[0].Final {
		t.Error("仍有大模型可尝试时,快引擎结果不应标记为最终")
	}
	// 熔断期内第二次请求只走快引擎,不再调用大模型。
	second := collect(t, chain.Stream(context.Background(), "again"))
	if len(second) != 1 || second[0].Result.Engine != "libretranslate" || !second[0].Final {
		t.Fatalf("第二次事件 = %+v", second)
	}
	if primary.calls.Load() != 1 {
		t.Fatalf("熔断后仍调用了大模型 %d 次", primary.calls.Load())
	}
}

// 快引擎先返回就先发;大模型随后返回,再发一个 Final 事件用于替换。
func TestStreamFastFirstThenQualityUpgrade(t *testing.T) {
	primary := &fakeEngine{name: "llm", max: 6000, delay: 30 * time.Millisecond}
	fallback := &fakeEngine{name: "libretranslate", max: 5000}
	chain := NewChain(primary, fallback)

	events := collect(t, chain.Stream(context.Background(), "hello"))
	if len(events) != 2 {
		t.Fatalf("事件 = %+v", events)
	}
	if events[0].Result.Engine != "libretranslate" || events[0].Final {
		t.Fatalf("第一帧 = %+v", events[0])
	}
	if events[1].Result.Engine != "llm" || !events[1].Final {
		t.Fatalf("第二帧 = %+v", events[1])
	}
	// 升级后缓存里应是大模型的结果,下次直接给最终稿。
	again := collect(t, chain.Stream(context.Background(), "hello"))
	if len(again) != 1 || !again[0].Final || again[0].Result.Engine != "llm" || !again[0].Result.Cached {
		t.Fatalf("缓存事件 = %+v", again)
	}
}

// 大模型先到就只发一帧最终结果,并取消快引擎。
func TestStreamQualityFirstOnly(t *testing.T) {
	primary := &fakeEngine{name: "llm", max: 6000}
	fallback := &fakeEngine{name: "deepl", max: 5000, delay: 200 * time.Millisecond}
	chain := NewChain(primary, fallback)

	events := collect(t, chain.Stream(context.Background(), "hello"))
	if len(events) != 1 || !events[0].Final || events[0].Result.Engine != "llm" {
		t.Fatalf("事件 = %+v", events)
	}
	if fallback.calls.Load() != 1 {
		t.Fatalf("快引擎调用 %d 次,期望 1", fallback.calls.Load())
	}
}

// 两个引擎都失败时才报错。
func TestStreamBothFail(t *testing.T) {
	chain := NewChain(
		&fakeEngine{name: "llm", max: 6000, err: errors.New("llm down")},
		&fakeEngine{name: "deepl", max: 5000, err: errors.New("deepl down")},
	)
	events := collect(t, chain.Stream(context.Background(), "hello"))
	if len(events) != 1 || !errors.Is(events[0].Err, ErrUnavailable) {
		t.Fatalf("事件 = %+v", events)
	}
}

// 缓存里只有快引擎结果时,先发出去,再尝试升级。
func TestStreamCachedFastStillUpgrades(t *testing.T) {
	primary := &fakeEngine{name: "llm", max: 6000}
	fallback := &fakeEngine{name: "deepl", max: 5000}
	chain := NewChain(primary, fallback)
	chain.remember(cacheKey("hello"), Result{Text: "旧译文", Engine: "deepl"})

	events := collect(t, chain.Stream(context.Background(), "hello"))
	if len(events) != 2 {
		t.Fatalf("事件 = %+v", events)
	}
	if !events[0].Result.Cached || events[0].Final {
		t.Fatalf("第一帧 = %+v", events[0])
	}
	if events[1].Result.Engine != "llm" || !events[1].Final {
		t.Fatalf("第二帧 = %+v", events[1])
	}
	if fallback.calls.Load() != 0 {
		t.Fatalf("缓存已有时仍调用了快引擎 %d 次", fallback.calls.Load())
	}
}

func TestChainWithoutEnginesIsUnavailable(t *testing.T) {
	chain := NewChain(nil, nil)
	events := collect(t, chain.Stream(context.Background(), "hello"))
	if len(events) != 1 || !errors.Is(events[0].Err, ErrUnavailable) {
		t.Fatalf("事件 = %+v", events)
	}
}

func TestChainRejectsEmptyText(t *testing.T) {
	chain := NewChain(&fakeEngine{name: "llm", max: 6000}, nil)
	events := collect(t, chain.Stream(context.Background(), "  \n "))
	if len(events) != 1 || !errors.Is(events[0].Err, ErrEmptyText) {
		t.Fatalf("事件 = %+v", events)
	}
}

func TestChainCachesResult(t *testing.T) {
	e := &fakeEngine{name: "llm", max: 6000}
	chain := NewChain(e, nil)
	collect(t, chain.Stream(context.Background(), "cache me"))
	events := collect(t, chain.Stream(context.Background(), "cache me"))
	if len(events) != 1 || !events[0].Result.Cached {
		t.Fatalf("第二次事件 = %+v", events)
	}
	if e.calls.Load() != 1 {
		t.Fatalf("缓存命中后仍调用了引擎 %d 次", e.calls.Load())
	}
}

func TestChainSplitsLongTextAndKeepsOrder(t *testing.T) {
	e := &fakeEngine{name: "llm", max: 5000, fn: strings.ToUpper}
	chain := NewChain(e, nil)
	input := strings.Repeat("a", chunkSize*2+10)
	events := collect(t, chain.Stream(context.Background(), input))
	if len(events) != 1 || events[0].Err != nil {
		t.Fatalf("事件 = %+v", events)
	}
	if e.calls.Load() != 3 {
		t.Fatalf("分块调用 %d 次,期望 3", e.calls.Load())
	}
	if strings.ReplaceAll(events[0].Result.Text, "\n\n", "") != strings.ToUpper(input) {
		t.Error("分块结果的顺序或内容不对")
	}
}

func TestChainTruncatesBeyondLimit(t *testing.T) {
	e := &fakeEngine{name: "llm", max: 5000}
	chain := NewChain(e, nil)
	input := strings.Repeat("a", chunkSize*maxChunks+100)
	events := collect(t, chain.Stream(context.Background(), input))
	if len(events) != 1 || !events[0].Result.Truncated {
		t.Fatalf("事件 = %+v", events)
	}
	if e.calls.Load() != maxChunks {
		t.Fatalf("调用 %d 次,期望 %d", e.calls.Load(), maxChunks)
	}
}

// DeepL 多 key:按轮询位置分配,额度用尽(456)时在同一次调用里换下一个 key。
func TestDeepLRotatesKeys(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "DeepL-Auth-Key ")
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		if key == "dead" {
			w.WriteHeader(456)
			return
		}
		fmt.Fprint(w, `{"translations":[{"text":"你好"}]}`)
	}))
	defer server.Close()

	engine := NewDeepL(server.URL, []string{"dead", "live"}, 5*time.Second)
	got, err := engine.Translate(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "你好" {
		t.Fatalf("译文 = %q", got)
	}
	if _, err := engine.Translate(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, ",") != "dead,live,live" {
		t.Fatalf("key 使用顺序 = %v", seen)
	}
}
