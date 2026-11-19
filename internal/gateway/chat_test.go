// chat_test.go Chat 同构透传入口 + 候选头拼装的回归测试。
//
// 锁定两件事（都是实测踩过的坑）：
//   1. /v1/chat/completions 现在经过网关，普通模型请求/响应必须零回归；
//   2. 多候选别名的 X-WorkBuddy-Candidates 头必须是 "alias|实体1|实体2" 完整拼接，
//      只传第一实体会让内层判定普通模式、跨渠道切号永远不生效。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capHandler 记录每次内层收到的候选头，返回固定 Chat 响应。
type capHandler struct {
	mu      sync.Mutex
	headers []string
}

func (c *capHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.headers = append(c.headers, r.Header.Get("X-WorkBuddy-Candidates"))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
}

func newChatMux(inner http.Handler, router Router) *http.ServeMux {
	g := New(Config{Inner: inner, APIKey: "secret", Router: router})
	mux := http.NewServeMux()
	g.Routes(mux)
	mux.Handle("/", inner)
	return mux
}

func chatRouter() Router {
	return Router{
		Default:      "workbuddy",
		Map:          map[string]string{},
		CustomModels: map[string][]string{"deva-main": {"workbuddy/glm-5.3-flash", "qwenwork/flash", "raccoon/sn-glm-5-3-flash"}},
		Channels:     []string{"workbuddy", "qwenwork", "raccoon"},
	}
}

func chatPost(mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	mux.ServeHTTP(rec, req)
	return rec
}

// TestChatPlainModelZeroRegression 普通模型（已带前缀）body 与响应都原样透传。
func TestChatPlainModelZeroRegression(t *testing.T) {
	inner := &scriptedHandler{}
	mux := newChatMux(inner, chatRouter())
	rec := chatPost(mux, `{"model":"workbuddy/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Hello world") {
		t.Errorf("response not passed through verbatim: %s", rec.Body.String())
	}
	// scriptedHandler.calls 记录 "path|auth|model"，model 必须保持客户端原名
	if len(inner.calls) != 1 || !strings.Contains(inner.calls[0], "workbuddy/glm-5.3-flash") {
		t.Errorf("inner calls = %v, want model unchanged", inner.calls)
	}
}

// TestChatAliasPassesFullCandidateHeader 别名必须注入完整 "alias|c1|c2|c3" 头，
// 且 body.model 换成解析后的第一候选（内层普通模式也要能路由）。
func TestChatAliasPassesFullCandidateHeader(t *testing.T) {
	inner := &capHandler{}
	mux := newChatMux(inner, chatRouter())
	rec := chatPost(mux, `{"model":"deva-main","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(inner.headers) != 1 {
		t.Fatalf("inner call count = %d, want 1", len(inner.headers))
	}
	got := inner.headers[0]
	want := "deva-main|workbuddy/glm-5.3-flash|qwenwork/flash|raccoon/sn-glm-5-3-flash"
	if got != want {
		t.Errorf("candidate header = %q, want %q", got, want)
	}
}

// TestMessagesAliasPassesFullCandidateHeader messages 路径修复后也必须拿到完整头
// （之前只传第一实体、切号失效，这条防止回归）。
func TestMessagesAliasPassesFullCandidateHeader(t *testing.T) {
	inner := &capHandler{}
	mux := newChatMux(inner, chatRouter())
	rec := post(mux, "/v1/messages", `{"model":"deva-main","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := "deva-main|workbuddy/glm-5.3-flash|qwenwork/flash|raccoon/sn-glm-5-3-flash"
	if len(inner.headers) != 1 || inner.headers[0] != want {
		t.Errorf("candidate header = %v, want [%q]", inner.headers, want)
	}
}

// TestCandidateHeaderFiltersUnconnectedChannel 未接入渠道的候选必须被剔除，
// 否则内层 runtimeForModel 会报 invalid_model 并中断整个轮换。
func TestCandidateHeaderFiltersUnconnectedChannel(t *testing.T) {
	inner := &capHandler{}
	router := chatRouter()
	router.CustomModels = map[string][]string{"deva-main": {
		"workbuddy/glm-5.3-flash", "ghost/model", "qwenwork/flash",
	}}
	mux := newChatMux(inner, router)
	rec := chatPost(mux, `{"model":"deva-main","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	want := "deva-main|workbuddy/glm-5.3-flash|qwenwork/flash"
	if len(inner.headers) != 1 || inner.headers[0] != want {
		t.Errorf("candidate header = %v, want [%q]", inner.headers, want)
	}
}

// TestChatStreamPassthrough 流式 Chat 响应原样透传（含 [DONE]）。
func TestChatStreamPassthrough(t *testing.T) {
	inner := &scriptedHandler{}
	mux := newChatMux(inner, chatRouter())
	rec := chatPost(mux, `{"model":"deva-main","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "[DONE]") {
		t.Errorf("SSE not passed through: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestChatAliasUnknownModelError 未配置的裸名在 default_channel=auto 且无命中时应报错（形状为 OpenAI）。
func TestChatAliasUnknownModelError(t *testing.T) {
	inner := &capHandler{}
	router := chatRouter()
	router.Default = "" // 关掉兜底
	mux := newChatMux(inner, router)
	rec := chatPost(mux, `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("error shape wrong: %s", rec.Body.String())
	}
}
