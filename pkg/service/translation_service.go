package service

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"emailbox/configs"
	"emailbox/pkg/mailer"
	"emailbox/pkg/mailer/imapx"
	"emailbox/pkg/model"
	"emailbox/pkg/quota"
	"emailbox/pkg/translate"
)

// TranslationOptions 是翻译服务的构造参数。空配置表示功能关闭。
type TranslationOptions struct {
	LLMBaseURL       string
	LLMAPIKey        string
	LLMModel         string
	LLMTimeout       time.Duration
	FallbackProvider string
	FallbackURL      string
	FallbackAPIKeys  []string
}

// TranslationService 取正文并调用翻译链。
//
// 正文由服务端自己取,不接受前端上传:否则这个端点会变成租户可用的通用
// 翻译代理,内容和成本都不受控。取邮件详情的远端调用不扣 daily_mail_fetch
// (用户是在读同一封已打开的信),只扣 daily_translate。
type TranslationService struct {
	messages *MessageService
	quota    *quota.Service
	chain    *translate.Chain
}

func NewTranslationService(messages *MessageService, q *quota.Service, opt TranslationOptions) *TranslationService {
	var primary translate.Engine
	if opt.LLMBaseURL != "" {
		primary = translate.NewLLM(opt.LLMBaseURL, opt.LLMAPIKey, opt.LLMModel, opt.LLMTimeout)
	}
	var fallback translate.Engine
	switch opt.FallbackProvider {
	case configs.TranslateProviderLibre:
		key := ""
		if len(opt.FallbackAPIKeys) > 0 {
			key = opt.FallbackAPIKeys[0]
		}
		fallback = translate.NewLibreTranslate(opt.FallbackURL, key, 0)
	case configs.TranslateProviderDeepL:
		fallback = translate.NewDeepL(opt.FallbackURL, opt.FallbackAPIKeys, 0)
	}
	return &TranslationService{messages: messages, quota: q, chain: translate.NewChain(primary, fallback)}
}

// Enabled 表示至少配置了一种引擎。
func (s *TranslationService) Enabled() bool {
	return s.chain != nil && s.chain.Enabled()
}

// Stream 取正文并返回翻译事件流。账号归属、功能开关、配额与取正文这些
// 预检失败时直接返回错误;之后两个引擎的调用都在流里,客户端断开即取消。
func (s *TranslationService) Stream(
	ctx context.Context, tenantID, accountID string,
	folder mailer.Folder, id, idMode string,
) (<-chan translate.Event, error) {
	start := time.Now()
	if folder == mailer.FolderAll || !mailer.ValidFolder(folder) {
		return nil, ErrInvalidFolder
	}
	// 先解析账号再做功能开关:别人租户的 accountID 必须稳定地得到 404,
	// 而不是因为「翻译未启用」先返回一个与资源存在与否无关的错误。
	account, cred, err := s.messages.credential(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	if !s.Enabled() {
		return nil, translate.ErrUnavailable
	}
	// 与取件同口径:先扣配额再打远端,超额时一个请求都不产生。
	if err := s.quota.CheckAndConsume(ctx, tenantID, model.MetricTranslate, 1); err != nil {
		return nil, err
	}

	client, record := s.messages.clientFor(ctx, tenantID, account)
	detail, err := client.Detail(ctx, cred, folder, id, idMode)
	record(err)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, translate.ErrEmptyText
	}

	text := detail.Body
	if detail.BodyType == "html" {
		text = imapx.HTMLToText(detail.Body)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, translate.ErrEmptyText
	}
	detailMs := time.Since(start).Milliseconds()
	chars := utf8.RuneCountInString(text)
	events := s.chain.Stream(ctx, text)

	// 包一层只为一件事:记下每个结果来自哪个引擎、是不是最终结果。
	// 不记正文;响应可能分两次到达,逐事件记录才能看出先后。
	out := make(chan translate.Event, 4)
	go func() {
		defer close(out)
		for ev := range events {
			if ev.Err == nil {
				slog.Info("翻译事件",
					"engine", ev.Result.Engine,
					"final", ev.Final,
					"chars", chars,
					"detail_ms", detailMs,
					"elapsed_ms", time.Since(start).Milliseconds())
			}
			out <- ev
		}
	}()
	return out, nil
}
