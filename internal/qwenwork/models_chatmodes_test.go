// models_chatmodes_test.go P1 回归：FetchModels 以 chat-modes 为动态主源，
// 与费率同源，能列出上游新增模型；chat-modes 失败时回退 COSY model/list。
package qwenwork

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-api/internal/auth"
)

// 精简版真实 chat-modes 响应（结构逐字对齐 2026-11-17 线上抓取）。
// 注意：线上返回的是裸 scenes map，无 {code,data} 信封——这是隐藏 bug 的根源。
const chatModesFixture = `{"qwork":[
{"key":"flash","i18n":{"display_name":{"zh":"标准","en":"Standard"}},"enable":true,"price_factor":0.1,"is_reasoning":true,"is_vl":true,"max_input_tokens":1000000,"format":"openai"},
{"key":"pro","i18n":{"display_name":{"zh":"高级","en":"Pro"}},"enable":true,"price_factor":1,"is_reasoning":true,"is_vl":false,"max_input_tokens":1000000,"format":"openai"},
{"key":"qwen4-max","i18n":{"display_name":{"zh":"Qwen4-Max","en":"Qwen4-Max"}},"enable":true,"price_factor":1.6,"is_reasoning":true,"is_vl":true,"max_input_tokens":1000000,"format":"anthropic"},
{"key":"qwen3.8-max-preview","i18n":{"display_name":{"zh":"Qwen3.8-Max","en":"Qwen3.8-Max"}},"enable":true,"price_factor":1.8,"is_reasoning":true,"is_vl":true,"max_input_tokens":1000000,"format":"openai"},
{"key":"glm-5.3","i18n":{"display_name":{"zh":"GLM-5.3","en":"GLM-5.3"}},"enable":true,"price_factor":1.2,"is_reasoning":true,"is_vl":false,"max_input_tokens":1000000,"format":"openai"},
{"key":"kimi-k3.1-pro","i18n":{"display_name":{"zh":"Kimi-K3.1-Pro","en":"Kimi-K3.1-Pro"}},"enable":true,"price_factor":2.4,"is_reasoning":true,"is_vl":true,"max_input_tokens":1000000,"format":"openai"},
{"key":"minimax-m3.1-flash","i18n":{"display_name":{"zh":"MiniMax-M3.1-Flash","en":"MiniMax-M3.1-Flash"}},"enable":true,"price_factor":0.2,"is_reasoning":true,"is_vl":true,"max_input_tokens":1000000,"format":"anthropic"},
{"key":"disabled-one","i18n":{"display_name":{"zh":"已下架","en":"Off"}},"enable":false,"price_factor":9,"max_input_tokens":1000000,"format":"openai"}
]}`

// TestFetchModels_ChatModesPrimary 验证 chat-modes 为主源时列出全部启用模型（含新 key）。
func TestFetchModels_ChatModesPrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, EpChatModes) {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatModesFixture))
	}))
	defer srv.Close()

	c := New()
	c.Web = srv.URL
	got, err := c.FetchModels(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 7 { // 8 条里 1 条 enable=false 被过滤
		t.Fatalf("模型数=%d，want 7", len(got))
	}
	byID := map[string]bool{}
	for _, m := range got {
		byID[m.ID] = true
	}
	for _, key := range []string{"qwen4-max", "glm-5.3", "kimi-k3.1-pro", "minimax-m3.1-flash"} {
		if !byID[key] {
			t.Fatalf("缺新模型 %q", key)
		}
	}
	if byID["disabled-one"] {
		t.Fatal("已下架模型不该出现")
	}
	// 展示名取中文 + 能力位来自 is_vl/is_reasoning
	for _, m := range got {
		if m.ID == "flash" && m.Name != "标准" {
			t.Fatalf("flash 展示名=%q want 标准", m.Name)
		}
		if m.ID == "pro" && m.SupportsImages {
			t.Fatal("pro is_vl=false，不该声明图片能力")
		}
		if m.ID == "qwen4-max" && !m.SupportsImages {
			t.Fatal("qwen4-max is_vl=true，应声明图片能力")
		}
	}
}

// TestFetchModelPricing_ChatModesSameSource 验证费率与清单同源、倍率正确。
func TestFetchModelPricing_ChatModesSameSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatModesFixture))
	}))
	defer srv.Close()

	c := New()
	c.Web = srv.URL
	rows, err := c.FetchModelPricing(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModelPricing: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("费率条数=%d，want 7", len(rows))
	}
	want := map[string]float64{
		"flash": 0.1, "pro": 1, "qwen4-max": 1.6, "glm-5.3": 1.2,
		"kimi-k3.1-pro": 2.4, "minimax-m3.1-flash": 0.2,
	}
	for _, row := range rows {
		if v, ok := want[row.Model]; ok && row.Rate != v {
			t.Fatalf("%s 倍率=%v want %v", row.Model, row.Rate, v)
		}
	}
}

// TestFetchModels_FallbackToGateway 验证 chat-modes 失败时回退 COSY model/list（3 款老档）。
func TestFetchModels_FallbackToGateway(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer web.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"qwork":[{"key":"pro","display_name":"高级","enable":true,"price_factor":1,"max_input_tokens":180000},{"key":"flash","display_name":"标准","enable":true,"price_factor":0.1,"max_input_tokens":180000}]}`))
	}))
	defer gw.Close()

	c := New()
	c.Web = web.URL
	c.Gateway = gw.URL
	got, err := c.FetchModels(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModels fallback: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("回退模型数=%d，want 2", len(got))
	}
	if got[0].ID != "pro" && got[1].ID != "pro" {
		t.Fatal("回退清单缺 pro")
	}
}

// TestFetchChatModes_EnvelopeCompat 验证上游若加回 {code,data} 信封也能解析。
func TestFetchChatModes_EnvelopeCompat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","data":{"qwork":[{"key":"qwen4-max","i18n":{"display_name":{"zh":"Qwen4-Max"}},"enable":true,"price_factor":1.6,"max_input_tokens":1000000}]}}`))
	}))
	defer srv.Close()
	c := New()
	c.Web = srv.URL
	modes, err := c.fetchChatModes(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("envelope compat: %v", err)
	}
	if len(modes) != 1 || modes[0].Key != "qwen4-max" {
		t.Fatalf("信封解析失败: %+v", modes)
	}
}
