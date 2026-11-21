package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// LibreTranslate 兼容端点。自建时通常不需要密钥;公共实例可能要求 api_key。
type LibreTranslate struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewLibreTranslate(baseURL, apiKey string, timeout time.Duration) *LibreTranslate {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &LibreTranslate{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

func (l *LibreTranslate) Name() string   { return "libretranslate" }
func (l *LibreTranslate) MaxSingle() int { return 5000 }

func (l *LibreTranslate) Translate(ctx context.Context, text string) (string, error) {
	in := struct {
		Q      string `json:"q"`
		Source string `json:"source"`
		Target string `json:"target"`
		Format string `json:"format"`
		APIKey string `json:"api_key,omitempty"`
	}{Q: text, Source: "auto", Target: "zh", Format: "text", APIKey: l.apiKey}
	var out struct {
		TranslatedText string `json:"translatedText"`
	}
	if err := postJSON(ctx, l.client, l.baseURL+"/translate", nil, in, &out); err != nil {
		return "", fmt.Errorf("libretranslate: %w", err)
	}
	if strings.TrimSpace(out.TranslatedText) == "" {
		return "", fmt.Errorf("libretranslate: 返回了空译文")
	}
	return strings.TrimSpace(out.TranslatedText), nil
}

// DeepL Free 使用同一套 JSON 协议,只是端点是 api-free.deepl.com 且必须带 key。
// 支持多个 key 轮询:每个 key 有独立额度,列表把请求摊到几份额度上。
type DeepL struct {
	endpoint string
	keys     []string
	next     atomic.Uint64
	client   *http.Client
}

func NewDeepL(baseURL string, keys []string, timeout time.Duration) *DeepL {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	endpoint := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(endpoint, "/v2/translate") {
		endpoint += "/v2/translate"
	}
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			clean = append(clean, k)
		}
	}
	return &DeepL{endpoint: endpoint, keys: clean, client: &http.Client{Timeout: timeout}}
}

func (d *DeepL) Name() string   { return "deepl" }
func (d *DeepL) MaxSingle() int { return 5000 }

func (d *DeepL) Translate(ctx context.Context, text string) (string, error) {
	if len(d.keys) == 0 {
		return "", fmt.Errorf("deepl: 未配置 API Key")
	}
	// 从轮询位置开始试;某个 key 额度用尽(456)或失效(401/403)
	// 时换下一个,而不是让用户看到一次本可避免的失败。
	start := int(d.next.Add(1)-1) % len(d.keys)
	var lastErr error
	for i := 0; i < len(d.keys); i++ {
		out, err := d.translateWithKey(ctx, d.keys[(start+i)%len(d.keys)], text)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryWithNextKey(err) {
			break
		}
	}
	return "", fmt.Errorf("deepl: %w", lastErr)
}

func (d *DeepL) translateWithKey(ctx context.Context, apiKey, text string) (string, error) {
	// ZH-HANS 是简体中文;只用 ZH 时部分账号会按账号语言返回繁体。
	in := struct {
		Text       []string `json:"text"`
		TargetLang string   `json:"target_lang"`
	}{Text: []string{text}, TargetLang: "ZH-HANS"}
	var out struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	headers := map[string]string{"Authorization": "DeepL-Auth-Key " + apiKey}
	if err := postJSON(ctx, d.client, d.endpoint, headers, in, &out); err != nil {
		return "", err
	}
	if len(out.Translations) == 0 || strings.TrimSpace(out.Translations[0].Text) == "" {
		return "", fmt.Errorf("返回了空译文")
	}
	return strings.TrimSpace(out.Translations[0].Text), nil
}

// retryWithNextKey 只对「换一把 key 有意义」的错误换 key:
// 401/403 是鉴权或账号问题,456 是额度用尽,429 是限流。
func retryWithNextKey(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 456:
		return true
	}
	return false
}

// statusError 只保留状态码,不保留响应体:其中可能回显请求内容。
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func postJSON(ctx context.Context, client *http.Client, url string,
	headers map[string]string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 不读响应体:其中可能回显请求内容。
		return &statusError{code: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("响应无法解析: %w", err)
	}
	return nil
}
