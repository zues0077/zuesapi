package customopenai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/provider"
)

func testClient(t *testing.T, base string) *Client {
	t.Helper()
	c := New()
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	c.StreamHTTP = &http.Client{}
	// 虚拟账号 UID = 实例名；注册模型表/费率。
	c.RegisterInstance("c1", []string{"model-a", "model-b"}, 0, 0)
	c.RegisterInstance("paid", []string{"model-p"}, 2.5, 0)
	return c
}

func acct(host, key, uid string) *auth.Auth {
	return &auth.Auth{Kind: ChannelName, ApiHost: host, AccessToken: key, UID: uid}
}

func TestBaseURLFromAccount(t *testing.T) {
	c := testClient(t, "")
	if got := c.baseURL(acct("http://127.0.0.1:1234/v1/", "k", "c1")); got != "http://127.0.0.1:1234/v1" {
		t.Fatalf("base_url 应去掉尾斜杠，实际 %q", got)
	}
	if got := c.baseURL(acct("", "k", "c1")); got != "" {
		t.Fatalf("空 base_url 应返回空，实际 %q", got)
	}
}

func TestChatStreamForwardsBodyAndBearer(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	rc, status, _, err := c.ChatStream(acct(srv.URL, "sk-test", "c1"), []byte(`{"model":"model-a","stream":true}`))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream 失败 status=%d err=%v", status, err)
	}
	defer rc.Close()
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization 应为 Bearer sk-test，实际 %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"model-a"`) {
		t.Fatalf("body 未原样转发: %q", gotBody)
	}
}

func TestChatStreamNoBaseURL(t *testing.T) {
	c := testClient(t, "")
	_, _, _, err := c.ChatStream(acct("", "k", "c1"), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("空 base_url 应报错，实际 %v", err)
	}
}

func TestFetchModelsAndPricingPerInstance(t *testing.T) {
	c := testClient(t, "")
	models, err := c.FetchModels(acct("", "", "c1"))
	if err != nil || len(models) != 2 || models[0].ID != "model-a" {
		t.Fatalf("FetchModels c1 错: %v %+v", err, models)
	}
	prices, err := c.FetchModelPricing(acct("", "", "c1"))
	if err != nil || len(prices) != 2 || prices[0].Rate != 0 || prices[0].Explicit == nil || !*prices[0].Explicit {
		t.Fatalf("c1 免费应 rate=0 explicit=true: %v %+v", err, prices)
	}
	pricesP, _ := c.FetchModelPricing(acct("", "", "paid"))
	if len(pricesP) != 1 || pricesP[0].Rate != 2.5 || !*pricesP[0].Explicit {
		t.Fatalf("paid 应 rate=2.5 explicit=true: %+v", pricesP)
	}
}

func TestNegativeRateIsUnknown(t *testing.T) {
	c := New()
	c.RegisterInstance("u", []string{"m"}, -1, 0)
	p, _ := c.FetchModelPricing(acct("", "", "u"))
	if p[0].Explicit == nil || *p[0].Explicit {
		t.Fatalf("负 rate 应 Explicit=false（未知，排最后），实际 %+v", p[0])
	}
	if p[0].Rate != 0 {
		t.Fatalf("负 rate 应归一为 0，实际 %v", p[0].Rate)
	}
}

func TestAggregateOpenAISSE(t *testing.T) {
	c := testClient(t, "")
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"
	out, err := c.Aggregate(strings.NewReader(sse), "customopenai/model-a")
	if err != nil {
		t.Fatalf("Aggregate 失败: %v", err)
	}
	if out == nil {
		t.Fatal("Aggregate 返回 nil")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{401, `{"error":"unauthorized"}`, provider.ErrSessionDead},
		{403, "forbidden", provider.ErrSessionDead},
		{429, `{"error":"rate limit"}`, provider.ErrSoftRate},
		{404, "not found", provider.ErrNotFound},
		{500, "oops", provider.ErrServer},
		{400, "context length exceeded", provider.ErrPromptTooLong},
		{400, "insufficient quota", provider.ErrHardCredit},
		{400, "bad request", provider.ErrBadParams},
		{200, "ok", provider.ErrNone},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d,%q)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// TestForceStream 校验发往通用 OpenAI 上游的 body 一定带 stream:true：
// 网关非流式路径靠 Aggregate 解析 SSE，上游若按 stream:false 回 JSON 会解析失败。
func TestForceStream(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"显式非流式", `{"model":"m","stream":false,"messages":[]}`},
		{"未带 stream", `{"model":"m","messages":[]}`},
		{"已流式", `{"model":"m","stream":true,"messages":[]}`},
	}
	for _, tc := range cases {
		out := forceStream([]byte(tc.in))
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("%s: 输出不是合法 JSON: %v", tc.name, err)
		}
		if v, _ := obj["stream"].(bool); !v {
			t.Errorf("%s: stream 未被强制为 true，got=%v", tc.name, obj["stream"])
		}
		if _, has := obj["stream_options"]; !has {
			t.Errorf("%s: 缺少 stream_options", tc.name)
		}
	}
	// 显式带 stream_options 时不得覆盖
	out := forceStream([]byte(`{"stream_options":{"include_usage":false}}`))
	if !strings.Contains(string(out), `"include_usage":false`) {
		t.Errorf("显式 stream_options 被覆盖: %s", out)
	}
	// 非 JSON 原样返回
	raw := []byte("not-json")
	if got := forceStream(raw); string(got) != "not-json" {
		t.Errorf("非 JSON 未原样返回: %q", got)
	}
}

// TestParseCatalogMetadata 上游 /models 的上下文长度与能力必须被解析出来
// （duruoxian 用 limit.context / limit.output / capabilities.reasoning.supported，
// 常见实现用 context_length / max_tokens；两种都要认，否则客户端只能靠猜）。
func TestParseCatalogMetadata(t *testing.T) {
	raw := []byte(`{"object":"list","data":[
      {"id":"deepseek-chat","limit":{"context":262144,"output":32768},
       "capabilities":{"input":["text"],"reasoning":{"supported":true}}},
      {"id":"GLM-5.3","context_length":131072,"max_tokens":8192,
       "capabilities":{"input":["text","image"],"tools":true}}
    ]}`)
	infos, err := parseCatalog(raw)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("期望 2 个模型，实际 %d", len(infos))
	}
	a, b := infos[0], infos[1]
	if a.ContextWindow != 262144 || a.MaxTokens != 32768 {
		t.Errorf("limit.context/output 未解析：ctx=%d out=%d", a.ContextWindow, a.MaxTokens)
	}
	if !a.SupportsReasoning || a.SupportsImages || a.SupportsTools {
		t.Errorf("deepseek-chat 能力解析错误：reasoning=%v images=%v tools=%v",
			a.SupportsReasoning, a.SupportsImages, a.SupportsTools)
	}
	if b.ContextWindow != 131072 || b.MaxTokens != 8192 {
		t.Errorf("context_length/max_tokens 未解析：ctx=%d out=%d", b.ContextWindow, b.MaxTokens)
	}
	if !b.SupportsImages || !b.SupportsTools {
		t.Errorf("GLM-5.3 能力解析错误：images=%v tools=%v", b.SupportsImages, b.SupportsTools)
	}
	if !a.ContextFromAPI {
		t.Error("ContextFromAPI 应为 true（元数据来自上游）")
	}
}

// TestFetchModelsEnrichesFromCatalog 模型集合以配置为准，但字段用上游元数据补全；
// 上游拉不到时回落成裸模型名，列表不能消失。
func TestFetchModelsEnrichesFromCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"id":"model-a","limit":{"context":64000,"output":2048}}]}`)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	c.HTTP = srv.Client()
	got, err := c.FetchModels(acct(srv.URL+"/v1", "k", "c1"))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应以配置的 2 个模型为准，实际 %d", len(got))
	}
	if got[0].ID != "model-a" || got[0].ContextWindow != 64000 || got[0].MaxTokens != 2048 {
		t.Errorf("model-a 未被元数据补全：%+v", got[0])
	}
	if got[1].ID != "model-b" || got[1].ContextWindow != 0 {
		t.Errorf("model-b 上游没有，应保持裸名：%+v", got[1])
	}
}

// TestConcurrencyLimitQueues 并发上限生效：满了之后排队，排队超时返回 429。
func TestConcurrencyLimitQueues(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先把响应头 flush 出去，让 ChatStream 立刻返回（并发槽由流持有），
		// 再卡住 body —— 否则 Do() 等不到头，测试自己就死锁了。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	defer close(block)

	c := New()
	c.HTTP = srv.Client()
	c.StreamHTTP = srv.Client()
	c.QueueWait = 120 * time.Millisecond
	c.RegisterInstance("one", []string{"m"}, 0, 1) // 并发上限 1

	a := acct(srv.URL+"/v1", "k", "one")
	// 第一个请求占住唯一的槽
	rc, _, _, err := c.ChatStream(a, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("第一个请求应成功占用并发槽：%v", err)
	}
	// 第二个请求应排队超时 → 429
	_, status, body, err := c.ChatStream(a, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("排队超时不应是传输错误：%v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("排队超时应返 429，实际 %d body=%s", status, body)
	}
	// 释放后应能再次进入
	_ = rc.Close()
	rc2, st2, _, err := c.ChatStream(a, []byte(`{"model":"m"}`))
	if err != nil || st2 != http.StatusOK {
		t.Fatalf("释放后应可再次进入：status=%d err=%v", st2, err)
	}
	_ = rc2.Close()
}
