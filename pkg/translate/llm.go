package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM 调用 OpenAI 兼容的 chat/completions 端点。
//
// 不引入 SDK:这里只需要一次 POST 和一个字段,SDK 会带来一棵依赖树,
// 也让「用哪个模型/网关」重新受制于某个厂商的版本节奏。
type LLM struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewLLM(baseURL, apiKey, model string, timeout time.Duration) *LLM {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &LLM{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: timeout},
	}
}

func (l *LLM) Name() string { return "llm" }

// MaxSingle 留出提示词与转义的开销,不把模型上下文塞满。
func (l *LLM) MaxSingle() int { return 6000 }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

const llmSystemPrompt = "Translate the user's email text into Simplified Chinese. " +
	"Preserve line breaks, email addresses, URLs, code, numbers and names. " +
	"Output only the translation, with no explanations or quotes."

func (l *LLM) Translate(ctx context.Context, text string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model: l.model,
		Messages: []chatMessage{
			{Role: "system", Content: llmSystemPrompt},
			{Role: "user", Content: text},
		},
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.apiKey)

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 不回传响应体:有的网关会把请求内容或密钥片段写进错误里。
		return "", fmt.Errorf("llm: HTTP %d", resp.StatusCode)
	}
	var parsed chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed); err != nil {
		return "", fmt.Errorf("llm: 响应无法解析: %w", err)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("llm: 返回了空译文")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
