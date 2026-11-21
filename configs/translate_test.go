package configs

import "testing"

func TestValidateTranslation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     TranslationConfig
		wantErr bool
		enabled bool
	}{
		{name: "全部留空即关闭", cfg: TranslationConfig{}},
		{
			name:    "LLM 只配一半要报错",
			cfg:     TranslationConfig{LLMBaseURL: "https://api.example.com/v1", LLMModel: "m"},
			wantErr: true,
		},
		{
			name:    "LLM 完整",
			cfg:     TranslationConfig{LLMBaseURL: "https://api.example.com/v1", LLMAPIKey: "k", LLMModel: "m", LLMTimeoutSeconds: 20},
			enabled: true,
		},
		{
			name:    "兜底只配 URL 没指定协议要报错",
			cfg:     TranslationConfig{FallbackURL: "http://libretranslate:5000"},
			wantErr: true,
		},
		{
			name:    "DeepL 缺少密钥要报错",
			cfg:     TranslationConfig{FallbackProvider: TranslateProviderDeepL, FallbackURL: "https://api-free.deepl.com"},
			wantErr: true,
		},
		{
			name:    "LibreTranslate 自建不需要密钥",
			cfg:     TranslationConfig{FallbackProvider: TranslateProviderLibre, FallbackURL: "http://libretranslate:5000"},
			enabled: true,
		},
		{
			name: "DeepL 支持多 key 轮询",
			cfg: TranslationConfig{
				FallbackProvider: TranslateProviderDeepL,
				FallbackURL:      "https://api-free.deepl.com",
				FallbackAPIKeys:  []string{"k1", "k2"},
			},
			enabled: true,
		},
		{
			name: "LibreTranslate 配多个 key 要报错",
			cfg: TranslationConfig{
				FallbackProvider: TranslateProviderLibre,
				FallbackURL:      "http://libretranslate:5000",
				FallbackAPIKeys:  []string{"k1", "k2"},
			},
			wantErr: true,
		},
		{
			name:    "非 http 协议要报错",
			cfg:     TranslationConfig{FallbackProvider: TranslateProviderLibre, FallbackURL: "ftp://x"},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Translate: c.cfg}
			err := validateTranslation(cfg)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v,期望错误=%v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if got := c.cfg.Enabled(); got != c.enabled {
				t.Errorf("Enabled() = %v,期望 %v", got, c.enabled)
			}
		})
	}
}
