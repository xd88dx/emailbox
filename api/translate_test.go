package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 翻译未配置时接口返回 1006,而不是把内部错误翻译成 500。
func TestTranslateEndpointDisabled(t *testing.T) {
	e := newTestServer(t)
	token, tenantID := register(t, e, "translater", "translater@example.com")
	accountID := createAccount(t, e, token, tenantID, `{"email":"t@qq.com","password":"authcode"}`)

	status, body := do(t, e, http.MethodPost,
		"/api/v1/tenants/"+tenantID+"/mail/accounts/"+accountID+"/messages/msg-1/translate/stream?folder=inbox&id_mode=uid",
		token, "")
	if status != http.StatusBadGateway {
		t.Fatalf("状态 = %d,期望 502:%s", status, body)
	}
	if !strings.Contains(body, `"code":1006`) {
		t.Fatalf("业务码不是 1006:%s", body)
	}
}

// API Key 是只读且零成本的脚本通道,翻译要调用外部付费服务,不允许它使用。
func TestAPIKeyCannotTranslate(t *testing.T) {
	e := newTestServer(t)
	token, tenantID := register(t, e, "keytrans", "keytrans@example.com")
	accountID := createAccount(t, e, token, tenantID, `{"email":"k@qq.com","password":"authcode"}`)
	key := resetKey(t, e, token, tenantID)

	status, body := doKey(t, e, http.MethodPost,
		"/api/v1/tenants/"+tenantID+"/mail/accounts/"+accountID+"/messages/msg-1/translate/stream?folder=inbox&id_mode=uid",
		key)
	if status != http.StatusForbidden {
		t.Fatalf("状态 = %d,期望 403:%s", status, body)
	}
	if !strings.Contains(body, "API Key 不支持翻译") {
		t.Fatalf("错误信息没有说清原因:%s", body)
	}
}
