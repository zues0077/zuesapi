// v3_primary_test.go 回归：v2 目录 404 下线后，v3 必须能作为主源独立撑起
// 模型清单与费率（2026-11-17 起 v2 /v2/enterprises/personal/models 恒 404）。
package workbuddyai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy-api/internal/auth"
)

// 精简 v3 响应：models 全量面 + cli agent 定序。
// 含一个 brokenModel（deepseek-v4-pro）验证下游过滤；含一个非-cli 的图像模型验证定序排除。
const v3PrimaryFixture = `{"code":0,"data":{
  "agents":[{"name":"cli","models":["hy3","gpt-6-luna","deepseek-v4-pro","kimi-k3.1-pro","gpt-6-astra"]}],
  "models":[
    {"id":"hy3","name":"Hy3","credits":"x0.00","maxInputTokens":192000,"supportsToolCall":true},
    {"id":"gpt-6-luna","name":"GPT-6-Luna","credits":"x0.07","maxInputTokens":1000000,"supportsToolCall":true,"supportsImages":true},
    {"id":"deepseek-v4-pro","name":"DS Pro","credits":"x0.20","maxInputTokens":1000000},
    {"id":"kimi-k3.1-pro","name":"Kimi-K3.1-Pro","credits":"x0.97","maxInputTokens":1000000},
    {"id":"gpt-6-astra","name":"GPT-6-Astra","credits":"x6.67","maxInputTokens":1000000},
    {"id":"hy-image-3.5","name":"Image","credits":"","maxInputTokens":0}
  ]
}}`

// resetCatalogCache 每个用例前清空包级缓存，避免 10 分钟 TTL 串扰。
// 直接整体替换（含全新 mutex）；不加锁——加的是旧 mutex 的锁，Unlock 时已被替换。
func resetCatalogCache() {
	cache = catalogCache{}
}

func v404PrimaryServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, EpCatalogV3):
			_, _ = w.Write([]byte(v3PrimaryFixture))
		default: // EpCatalog(v2) 及其余一律 404，模拟上游改版下线
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
		}
	}))
}

// TestV2Dead_V3PrimaryModels v2 404 时模型清单来自 v3，按 cli 顺序、剔除 brokenModels。
func TestV2Dead_V3PrimaryModels(t *testing.T) {
	resetCatalogCache()
	srv := v404PrimaryServer(t)
	defer srv.Close()

	c := NewWithBase(srv.URL)
	got, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModels via v3 primary: %v", err)
	}
	ids := make([]string, 0, len(got))
	idset := map[string]bool{}
	for _, m := range got {
		// extraModels 会补一批目录外模型；这里只关心 v3 主源的 4 款是否按序在列。
		ids = append(ids, m.ID)
		idset[m.ID] = true
	}
	// v3 cli 顺序中 deepseek-v4-pro 是 brokenModel，必须被剔除
	if idset["deepseek-v4-pro"] {
		t.Fatal("brokenModel deepseek-v4-pro 未剔除")
	}
	// 非 cli 的图像模型不该从主源混进来（extraModels 也不含它）
	if idset["hy-image-3.5"] {
		t.Fatal("非 cli 图像模型不该出现在模型清单")
	}
	// 其余 4 款都应在
	for _, want := range []string{"hy3", "gpt-6-luna", "kimi-k3.1-pro", "gpt-6-astra"} {
		if !idset[want] {
			t.Fatalf("缺 v3 主源模型 %q", want)
		}
	}
	// cli 相对顺序保持（hy3 在 gpt-6-luna 之前）
	pos := map[string]int{}
	for i, id := range ids {
		pos[id] = i
	}
	if pos["hy3"] > pos["gpt-6-luna"] || pos["gpt-6-luna"] > pos["kimi-k3.1-pro"] {
		t.Fatalf("cli 定序错乱: %v", ids)
	}
}

// TestV2Dead_V3PrimaryPricing v2 404 时费率来自 v3 credits（含 x 前缀 / " credits" 后缀）。
func TestV2Dead_V3PrimaryPricing(t *testing.T) {
	resetCatalogCache()
	srv := v404PrimaryServer(t)
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rows, err := c.FetchModelPricing(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModelPricing via v3 primary: %v", err)
	}
	want := map[string]float64{
		"hy3": 0, "gpt-6-luna": 0.07, "kimi-k3.1-pro": 0.97, "gpt-6-astra": 6.67,
	}
	seen := map[string]float64{}
	for _, row := range rows {
		seen[row.Model] = row.Rate
	}
	for model, rate := range want {
		got, ok := seen[model]
		if !ok {
			t.Fatalf("缺费率行 %q", model)
		}
		if got != rate {
			t.Fatalf("%s rate=%v want %v", model, got, rate)
		}
	}
}

// TestV2Dead_CachedNoRepeatV2 v3 主源写入缓存后，10 分钟内不再重复打 v2。
func TestV2Dead_CachedNoRepeatV2(t *testing.T) {
	resetCatalogCache()
	v2Hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, EpCatalogV3) {
			_, _ = w.Write([]byte(v3PrimaryFixture))
			return
		}
		v2Hits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := &auth.Auth{AccessToken: "at"}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FetchModelPricing(a); err != nil {
		t.Fatal(err)
	}
	if v2Hits != 1 {
		t.Fatalf("v2 请求次数=%d，want 1（缓存命中后不应再打）", v2Hits)
	}
	cache.mu.RLock()
	fresh := time.Since(cache.fetched) < catalogTTL
	cache.mu.RUnlock()
	if !fresh {
		t.Fatal("v3 主源结果未写入缓存")
	}
}
