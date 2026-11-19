// alias_min_tokens_test.go 自动别名 max_tokens 下限兜底测试。
//
// 锁定：只对登记的自动别名（deva-main/deva-free）兜底，普通模型/其他别名零影响；
// 三协议（chat/messages/responses）口径一致；与上限封顶共存时先抬下限再封顶。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// bodyCapHandler 记录内层每次收到的完整请求 body，返回固定 Chat 响应。
type bodyCapHandler struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (h *bodyCapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	h.mu.Lock()
	h.bodies = append(h.bodies, req)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],"usage":{}}`))
}

func (h *bodyCapHandler) lastBody(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) == 0 {
		t.Fatal("内层未收到任何请求")
	}
	return h.bodies[len(h.bodies)-1]
}

// aliasTestMux 构造带自动别名兜底配置的网关 mux。
// autoAliases 显式登记 deva-main/deva-free；cap 默认 32000（与线上一致）。
func aliasTestMux(inner http.Handler, minTokens int, cap int) *http.ServeMux {
	var auto map[string]bool
	if minTokens > 0 {
		auto = map[string]bool{"deva-main": true, "deva-free": true}
	}
	g := New(Config{
		Inner:          inner,
		APIKey:         "secret",
		MaxTokensCap:   cap,
		AliasMinTokens: minTokens,
		AutoAliases:    auto,
		Router: Router{
			Default:      "workbuddy",
			Map:          map[string]string{},
			CustomModels: map[string][]string{
				"deva-main": {"workbuddy/glm-5.3-flash"},
				"deva-free": {"workbuddy/hy4flash-free"},
				"my-alias":  {"workbuddy/other"},
			},
			Channels: []string{"workbuddy", "qoder"},
		},
	})
	mux := http.NewServeMux()
	g.Routes(mux)
	mux.Handle("/", inner)
	return mux
}

func aliasPost(mux *http.ServeMux, path, body string) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("x-api-key", "secret")
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		panic("unexpected status " + rec.Body.String())
	}
}

// ---------- Chat 协议 ----------

func TestChatAutoAliasFloorRaisesSmall(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 32000)
	aliasPost(mux, "/v1/chat/completions", `{"model":"deva-free","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 256 {
		t.Errorf("deva-free max_tokens = %d, want 256", got)
	}
}

func TestChatAutoAliasFloorKeepsLarge(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 32000)
	aliasPost(mux, "/v1/chat/completions", `{"model":"deva-main","max_tokens":512,"messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 512 {
		t.Errorf("deva-main max_tokens = %d, want 512 (should not lower)", got)
	}
}

func TestChatPlainModelNotTouched(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 32000)
	aliasPost(mux, "/v1/chat/completions", `{"model":"workbuddy/glm-5.3-flash","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 32 {
		t.Errorf("普通模型 max_tokens = %d, want 32 (must not raise)", got)
	}
}

func TestChatUserAliasNotTouched(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 32000)
	aliasPost(mux, "/v1/chat/completions", `{"model":"my-alias","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 32 {
		t.Errorf("用户自定义别名 max_tokens = %d, want 32 (must not raise)", got)
	}
}

func TestChatFloorDisabled(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, 0, 32000) // 0 = 不兜底
	aliasPost(mux, "/v1/chat/completions", `{"model":"deva-free","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 32 {
		t.Errorf("关闭兜底后 max_tokens = %d, want 32", got)
	}
}

// ---------- 三协议一致性 ----------

func TestMessagesAutoAliasFloorRaisesSmall(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 32000)
	aliasPost(mux, "/v1/messages", `{"model":"deva-main","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 256 {
		t.Errorf("messages 自动别名 max_tokens = %d, want 256", got)
	}
}

func TestResponsesAutoAliasFloorRaisesSmall(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 32000)
	aliasPost(mux, "/v1/responses", `{"model":"deva-free","max_output_tokens":32,"input":"hi"}`)
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 256 {
		t.Errorf("responses 自动别名 max_tokens = %d, want 256", got)
	}
}

// ---------- 与上限封顶共存 ----------

func TestFloorThenCap(t *testing.T) {
	inner := &bodyCapHandler{}
	mux := aliasTestMux(inner, AliasMinTokensFloor, 100) // cap 故意小于下限
	aliasPost(mux, "/v1/messages", `{"model":"deva-main","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	// messages 走 anthropicToChat：先抬下限到 256，再封顶到 100 → 100
	if got, _ := asInt(inner.lastBody(t)["max_tokens"]); got != 100 {
		t.Errorf("先抬下限再封顶后 max_tokens = %d, want 100", got)
	}
}
