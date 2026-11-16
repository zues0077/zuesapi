package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/config"
	"workbuddy-api/internal/pool"
	"workbuddy-api/internal/provider"
)

// TestBrand 独立的后缀渠道，用于打满所有“渠道组”（空/有账号/有错）的分支。
const TestBrand provider.Kind = "testbrand"

// okUpstream 最简 fakeUpstream：默认成功拉取 3 条定价，不加错。
type okUpstream struct {
	errPricing bool
	errModels  bool
	nPricing   int
	calls      int
}

func (f *okUpstream) RefreshToken(a *auth.Auth) error { return nil }
func (f *okUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (f *okUpstream) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	if f.errModels { return nil, fmt.Errorf("boom models") }
	return nil, nil
}
func (f *okUpstream) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	f.calls++
	if f.errPricing { return nil, fmt.Errorf("boom pricing %d", f.calls) }
	out := make([]provider.ModelPricing, 0, f.nPricing)
	for i := 0; i < f.nPricing; i++ {
		out = append(out, provider.ModelPricing{Channel: string(TestBrand), Model: fmt.Sprintf("gpt-%d", i), Rate: float64(i), Explicit: func() *bool { b := true; return &b }()})
	}
	return out, nil
}
func (f *okUpstream) UserResource(a *auth.Auth) (int64, error) { return 100, nil }
func (f *okUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 100, nil, nil
}
func (f *okUpstream) DailyCheckin(a *auth.Auth) error           { return nil }
func (f *okUpstream) Classify(status int, body string) provider.ErrKind { return provider.ErrNone }
func (f *okUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return nil, nil
}
func (f *okUpstream) Aggregate(r io.Reader, model string) (map[string]any, error) { return nil, nil }

func newHealthApp(t *testing.T, up provider.Upstream) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	wbPool := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	wbPool.Add(&auth.Auth{Kind: string(provider.WorkBuddy), UID: "u-wb", AccessToken: "t", RefreshToken: "r", Domain: "www.workbuddy.ai"})
	brandPool := pool.New(filepath.Join(dir, "state-brand.json"))
	brandPool.Add(&auth.Auth{Kind: string(TestBrand), UID: "u-brand", AccessToken: "t", RefreshToken: "r", Domain: "www.testbrand.io"})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: up},
			TestBrand:          {Kind: TestBrand, Pool: brandPool, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// TestRefreshPricingHealth 验证费率拉取的一口健康度：
// 成功→live，无可用账号→no account available，拉错→pricing fetch failed，
// 微标三色（ok/stale/bad/gray）与 /api/fees 的 health 数据同源。
func TestRefreshPricingHealth(t *testing.T) {
	a := newHealthApp(t, &okUpstream{nPricing: 3, errPricing: false})
	a.StartPricingAutoRefresh(context.Background(), 30*time.Minute)
	time.Sleep(300 * time.Millisecond)

	h := a.pricingHealth
	// workbuddy: 成败; app: 无账号池
	for k, v := range h {
		if v.Source != "live" {
			t.Errorf("%s source=%q expected=live", k, v.Source)
		}
		if v.Stale {
			t.Errorf("%s stale 不应为 true", k)
		}
		if v.Error != "" {
			t.Errorf("%s err=%q expected=none", k, v.Error)
		}
		if v.ModelCount != 3 {
			t.Errorf("%s model_count=%d expected=3", k, v.ModelCount)
		}
	}

	// 故障：无可用账号的渠道 → no account available
	a2 := newHealthApp(t, &okUpstream{nPricing: 3})
	a2.runtimes[TestBrand].Pool = &pool.Pool{}
	a2.StartPricingAutoRefresh(context.Background(), 30*time.Minute)
	time.Sleep(300 * time.Millisecond)
	if h2 := a2.pricingHealth; h2[TestBrand].Error != "no account available" {
		t.Errorf("app err=%q", h2[TestBrand].Error)
	}
	if h2 := a2.pricingHealth; h2[TestBrand].Source != "" {
		t.Errorf("app source=%q expected empty", h2[TestBrand].Source)
	}

	// 价格拉取失败显形
	a3 := newHealthApp(t, &okUpstream{nPricing: 3})
	up := a3.runtimes[provider.WorkBuddy].Upstream.(*okUpstream)
	up.errPricing = true
	a3.StartPricingAutoRefresh(context.Background(), 30*time.Minute)
	time.Sleep(300 * time.Millisecond)
	if a3.pricingHealth[provider.WorkBuddy].Error == "" || !strings.Contains(a3.pricingHealth[provider.WorkBuddy].Error, "pricing fetch failed") {
		t.Errorf("wb err=%q", a3.pricingHealth[provider.WorkBuddy].Error)
	}
	if a3.pricingHealth[provider.WorkBuddy].Source != "" {
		t.Errorf("wb source=%q", a3.pricingHealth[provider.WorkBuddy].Source)
	}

	// FeesInfo 暴露 health 键（key 为渠道名，与路由 fallback 同源）
	a4 := newHealthApp(t, &okUpstream{nPricing: 3})
	a4.StartPricingAutoRefresh(context.Background(), 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	fi := a4.FeesInfo()
	health := fi["health"]
	hm, ok := health.(map[string]provider.ChannelHealth)
	if !ok || len(hm) != 2 {
		t.Fatalf("health 应覆盖 2 个渠道，实际 %v (%v)", health, ok)
	}
	if _, ok := hm[string(provider.WorkBuddy)]; !ok {
		t.Errorf("health 中缺少 workbuddy")
	}
	if _, ok := hm[string(TestBrand)]; !ok {
		t.Errorf("health 中缺少 testbrand")
	}
	if !a4.pricingFetched.After(time.Now().Add(-time.Hour)) {
		t.Errorf("pricingFetched 已过期 ?")
	}
}
