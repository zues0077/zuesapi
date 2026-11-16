package raccoon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy-api/internal/auth"
)

// TestFetchModelPricingUsesEffectiveRate 回归（2026-11-17）：
// 面板费率必须取上游「实际生效倍率」billing_effective_multiplier，
// 而非基础倍率 billing_multiplier —— 否则限免/折扣模型与官方客户端对不上。
//
// 官方客户端展示的就是实际倍率：限免模型基础 0.5→实际 0（客户端显示「限免中」），
// 折扣模型基础 0.75→实际 0.5（客户端显示「限时折扣 0.5 倍」）。
// 用户反馈"刷新也没用、和客户端对不上"即源于此字段取错。
func TestFetchModelPricingUsesEffectiveRate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"categories":[{"type":"chat","models":[
			{"model_name":"sn-sensenova-6-8-flash","billing_multiplier":0.5,
			 "billing_effective_multiplier":0,"billing_status":"limited_free",
			 "billing_status_note":"限免到11月30日"},
			{"model_name":"sn-qwen4-max","billing_multiplier":0.75,
			 "billing_effective_multiplier":0.5,"billing_status":"discount",
			 "billing_status_note":"限时折扣至11月22日"},
			{"model_name":"sn-kimi-k3","billing_multiplier":1,
			 "billing_effective_multiplier":1,"billing_status":"normal"},
			{"model_name":"legacy-model","billing_multiplier":0.3}
		]}]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	a := &auth.Auth{AccessToken: "dummy", ApiHost: srv.URL}
	rows, err := c.FetchModelPricing(a)
	if err != nil {
		t.Fatalf("FetchModelPricing: %v", err)
	}
	byID := map[string]struct {
		rate float64
		note string
	}{}
	for _, row := range rows {
		byID[row.Model] = struct {
			rate float64
			note string
		}{row.Rate, row.Note}
	}

	cases := []struct {
		model    string
		wantRate float64
		wantNote string
	}{
		{"sn-sensenova-6-8-flash", 0, "限免到11月30日"}, // 限免：实际 0，不是基础 0.5
		{"sn-qwen4-max", 0.5, "限时折扣至11月22日"},     // 折扣：实际 0.5，不是基础 0.75
		{"sn-kimi-k3", 1, ""},                        // 无活动：1
		{"legacy-model", 0.3, ""},                    // 老上游无 effective 字段：回落基础 0.3
	}
	for _, tc := range cases {
		got, ok := byID[tc.model]
		if !ok {
			t.Fatalf("缺少模型 %q", tc.model)
		}
		if got.rate != tc.wantRate {
			t.Errorf("%s rate = %v, want %v", tc.model, got.rate, tc.wantRate)
		}
		if got.note != tc.wantNote {
			t.Errorf("%s note = %q, want %q", tc.model, got.note, tc.wantNote)
		}
	}
}
