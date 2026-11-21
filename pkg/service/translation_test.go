package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"emailbox/configs"
	"emailbox/pkg/crypto"
	"emailbox/pkg/mailer"
	"emailbox/pkg/model"
	"emailbox/pkg/quota"
	"emailbox/pkg/repo"
	"emailbox/pkg/service"
	"emailbox/pkg/translate"
)

// detailClient 在 fakeMailClient 之上返回固定的 HTML 正文。
type detailClient struct {
	*fakeMailClient
	detail *mailer.Detail
}

func (d *detailClient) Detail(context.Context, mailer.Credential, mailer.Folder, string, string) (*mailer.Detail, error) {
	return d.detail, nil
}

func translationFixture(t *testing.T) (*repo.Store, string, string, *quota.Service) {
	t.Helper()
	configs.AppConfig = &configs.Config{Session: configs.SessionConfig{ExpireHour: 24}}
	store := testStore(t)
	cipher, err := crypto.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	auth := service.NewAuthService(store)
	registered, _, err := auth.Register(context.Background(), model.RegisterRequest{
		Username: "translator", Email: "translator@example.com", Password: "secret12",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := registered.Tenants[0].ID
	accounts := service.NewAccountService(store, cipher, quota.NewService(store))
	created, err := accounts.Create(context.Background(), tenantID, model.CreateMailAccountRequest{
		Email: "translate@qq.com", Password: "authcode",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, tenantID, created.ID, quota.NewService(store)
}

// 没有 LLM 时走配置的兜底引擎;正文取 HTML 的纯文本部分。
func TestTranslationUsesFallbackEngine(t *testing.T) {
	store, tenantID, accountID, quotaService := translationFixture(t)
	var calls atomic.Int32
	var gotQ, gotTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Q      string `json:"q"`
			Target string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		gotQ, gotTarget = body.Q, body.Target
		_ = json.NewEncoder(w).Encode(map[string]string{"translatedText": "你好"})
	}))
	defer srv.Close()

	cipher, _ := crypto.New("0123456789abcdef0123456789abcdef")
	client := &detailClient{
		fakeMailClient: &fakeMailClient{},
		detail: &mailer.Detail{
			Message:  mailer.Message{ID: "1", Folder: mailer.FolderInbox},
			Body:     `<html><body><p>Hello &amp; welcome</p></body></html>`,
			BodyType: "html",
		},
	}
	messages := service.NewMessageService(store, cipher, quotaService, service.ChainOptions{}).
		WithChainFactory(func(*model.MailAccount) mailer.Client { return client })
	trans := service.NewTranslationService(messages, quotaService, service.TranslationOptions{
		FallbackProvider: configs.TranslateProviderLibre,
		FallbackURL:      srv.URL,
	})

	result := firstTranslation(t, mustStream(t, trans, tenantID, accountID))
	if result.Text != "你好" || result.Engine != "libretranslate" {
		t.Fatalf("结果 = %+v", result)
	}
	if gotQ != "Hello & welcome" {
		t.Errorf("送给引擎的正文 = %q", gotQ)
	}
	if gotTarget != "zh" {
		t.Errorf("target = %q,期望 zh", gotTarget)
	}
	// 第二次同一封走缓存,不再打引擎。
	firstTranslation(t, mustStream(t, trans, tenantID, accountID))
	if calls.Load() != 1 {
		t.Fatalf("兜底引擎被调用 %d 次,期望 1(第二次应命中缓存)", calls.Load())
	}
	if used, err := quotaService.Usage(context.Background(), tenantID, model.MetricTranslate); err != nil || used != 2 {
		t.Fatalf("翻译用量 = %d, err = %v", used, err)
	}
}

// 未配置翻译时也必须先做账号归属校验:别人的 accountID 得到 404,而不是
// 一个与资源存在与否无关的「未启用」。
func TestTranslationChecksTenantBeforeFeatureFlag(t *testing.T) {
	store, tenantID, _, quotaService := translationFixture(t)
	cipher, _ := crypto.New("0123456789abcdef0123456789abcdef")
	messages := service.NewMessageService(store, cipher, quotaService, service.ChainOptions{})
	trans := service.NewTranslationService(messages, quotaService, service.TranslationOptions{})

	_, err := trans.Stream(context.Background(), tenantID, "00000000-0000-0000-0000-000000000000",
		mailer.FolderInbox, "1", "uid")
	if !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("err = %v,期望 ErrNotFound", err)
	}
}

func mustStream(t *testing.T, svc *service.TranslationService, tenantID, accountID string) <-chan translate.Event {
	t.Helper()
	events, err := svc.Stream(context.Background(), tenantID, accountID, mailer.FolderInbox, "1", "uid")
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func firstTranslation(t *testing.T, events <-chan translate.Event) translate.Result {
	t.Helper()
	var result translate.Result
	for ev := range events {
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
		if !ev.Final {
			t.Error("只配置一个引擎时,结果应当标记为最终结果")
		}
		result = ev.Result
	}
	if result.Text == "" {
		t.Fatal("没有收到译文")
	}
	return result
}
