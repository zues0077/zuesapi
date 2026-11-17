package customopenai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/provider"
	"workbuddy-api/internal/upstream"
)

// instance 是一个自定义渠道实例的配置快照（按虚拟账号 UID 索引）。
// 装配期由 config.custom_channels 注册进来；运行期只读。
type instance struct {
	name   string
	models []string
	rate   float64
}

// Client 自定义 OpenAI 兼容上游客户端。
//
// 一个 Client 服务所有 customopenai 实例；每个实例的 base_url / api_key / 模型表
// 分别来自虚拟账号的 ApiHost / AccessToken 与 registry（按 UID 索引）。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 无总超时，专供 SSE（与 R35 全渠道一致：Client.Timeout 会在读 body 阶段掐断长流）。
	StreamHTTP  *http.Client
	IdleTimeout time.Duration

	// registry 按虚拟账号 UID 存每个实例的模型表/费率。装配期写，运行期只读。
	registry map[string]*instance
}

// New 构造客户端。
func New() *Client {
	return &Client{registry: make(map[string]*instance)}
}

// RegisterInstance 装配期注册一个自定义实例（uid 与注入 Pool 的虚拟账号 UID 一致）。
func (c *Client) RegisterInstance(uid string, models []string, rate float64) {
	if c.registry == nil {
		c.registry = make(map[string]*instance)
	}
	cp := append([]string(nil), models...)
	c.registry[uid] = &instance{name: uid, models: cp, rate: rate}
}

// baseURL 从账号读 base_url（每个实例可指向不同的上游）。
func (c *Client) baseURL(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(a.ApiHost), "/")
}

func (c *Client) idleTimeout() time.Duration {
	// NewIdleReader 对 d<=0 直接透传（不加看门狗）；装配期可注入 config 值。
	return c.IdleTimeout
}

// applyAuth 写 Bearer 鉴权。api_key 为空时不写 Authorization（允许无鉴权的本地服务）。
func applyAuth(req *http.Request, a *auth.Auth) {
	if a == nil {
		return
	}
	key := strings.TrimSpace(a.AccessTokenValue())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// RefreshToken 实现 provider.Upstream：自定义实例无刷新端点。
// api_key 由用户在面板更换（重启生效），故这里返回明确错误引导重配，而非发必然失败的请求。
func (c *Client) RefreshToken(a *auth.Auth) error {
	return &provider.Error{
		Kind: provider.ErrSessionDead, Status: 0,
		Msg: "customopenai: api_key 由面板配置，失效请在面板修改后重启",
	}
}

// forceStream 把发往上游的请求体改成 stream=true。
//
// 为什么必须改：网关的**流式/非流式由客户端决定**（handler 看 peek.Stream 选
// Upstream.Stream 或 Upstream.Aggregate），而 Aggregate 只认 SSE。内置渠道的上游
// 本身只吐 SSE，所以非流式也能聚合；标准 OpenAI 兼容上游若收到 stream:false 会
// 直接回 JSON，Aggregate 解析不到数据事件，报 upstream_parse（实测）。
// 因此这里统一强制上游流式，非流式聚合交给 upstream.Aggregate。
//
// 顺带补 stream_options.include_usage（未显式带时），让上游在末帧回 usage，
// 与 workbuddyai/upstream 的既有做法一致（D7）。解析失败则原样返回，交由上游报错。
func forceStream(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// ChatStream 实现 provider.Upstream：把客户端 body 原样转发到自定义上游。
// 不做本地模型校验——用户自定义上游的模型集合以面板配置为准，硬校验会误拒上游可用模型
// （R42 教训：本地静态表校验与动态能力冲突）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	base := c.baseURL(a)
	if base == "" {
		return nil, 0, nil, fmt.Errorf("customopenai: 实例未配置 base_url（ApiHost 为空）")
	}
	req, err := http.NewRequest(http.MethodPost, base+EpChat, bytes.NewReader(forceStream(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, a)

	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	// 空闲看门狗：连续 idleTimeout 无字节即判卡死（对齐 R35 / loomy）。
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// FetchModels 实现 provider.Upstream：返回该实例配置的模型表（无需访问上游）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	inst := c.instanceOf(a)
	if inst == nil {
		return nil, nil
	}
	out := make([]provider.ModelInfo, 0, len(inst.models))
	for _, id := range inst.models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, provider.ModelInfo{ID: id, Name: id, ContextFromAPI: false})
	}
	return out, nil
}

// FetchModelPricing 实现 provider.Upstream：费率来自面板配置。
// rate==0 → Explicit=true（免费，A 排序排最前）；rate<0 → Explicit=false（未知，排最后）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	inst := c.instanceOf(a)
	if inst == nil {
		return nil, nil
	}
	explicit := inst.rate >= 0
	rate := inst.rate
	if rate < 0 {
		rate = 0
	}
	out := make([]provider.ModelPricing, 0, len(inst.models))
	for _, id := range inst.models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, provider.ModelPricing{
			Model: id, Channel: ChannelName, Rate: rate, Explicit: &explicit,
		})
	}
	return out, nil
}

// UserResource 实现 provider.Upstream：自定义上游额度不由本工具管理，返回 0（面板显示 0）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 实现 provider.Upstream。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 实现 provider.Upstream：自定义上游无签到。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return fmt.Errorf("customopenai: 自定义上游无签到")
}

// Classify 实现 provider.Upstream：标准 HTTP 语义错误分类。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream：上游即 OpenAI SSE，直接交通用规范化层。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, r, func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 实现 provider.Upstream：非流式路径把上游 SSE 收敛为单个 chat.completion。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return upstream.Aggregate(r)
}

func (c *Client) instanceOf(a *auth.Auth) *instance {
	if a == nil {
		return nil
	}
	return c.registry[a.UID]
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.httpClient()
}

// Classify 标准 OpenAI 兼容上游的错误分类（HTTP 状态码 + body 关键词）。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.ErrSessionDead
	case status == http.StatusPaymentRequired:
		return provider.ErrHardCredit
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		switch {
		case strings.Contains(lower, "context length"), strings.Contains(lower, "too long"), strings.Contains(lower, "tokens exceed"):
			return provider.ErrPromptTooLong
		case strings.Contains(lower, "content policy"), strings.Contains(lower, "sensitive"):
			return provider.ErrContentBlocked
		case strings.Contains(lower, "insufficient"), strings.Contains(lower, "quota"):
			return provider.ErrHardCredit
		}
		return provider.ErrBadParams
	}
	return provider.ErrNone
}

// ProbeModels 用给定 base_url + api_key 拉对端的 /models，返回模型 id 列表。
//
// 为什么需要：各家模型 id 的命名没有统一标准（gpt-4o-mini / deepseek-chat /
// moonshot-v1-8k / 火山方舟的推理接入点 ID…），让用户在面板手敲必错，错了还表现成
// "上游 400 但不知道哪错了"。面板的「拉取模型」用它自动发现并勾选。
//
// 由后端代发而不是浏览器直连：一是 CORS 会把大部分上游挡死，二是 api_key 不必
// 在前端跨域请求里裸奔。
func ProbeModels(ctx context.Context, baseURL, apiKey string, timeout time.Duration) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("base_url 不能为空")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+EpModels, nil)
	if err != nil {
		return nil, err
	}
	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败：%w", base+EpModels, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("对端返回 HTTP %d：%s", resp.StatusCode, msg)
	}
	// 标准形状 {"data":[{"id":"..."}]}；少数实现用 {"models":[...]} 或裸数组，一并兼容。
	var env struct {
		Data   []struct{ ID string `json:"id"` } `json:"data"`
		Models []struct{ ID string `json:"id"` } `json:"models"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		out := make([]string, 0, len(env.Data)+len(env.Models))
		for _, m := range env.Data {
			if id := strings.TrimSpace(m.ID); id != "" {
				out = append(out, id)
			}
		}
		for _, m := range env.Models {
			if id := strings.TrimSpace(m.ID); id != "" {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return dedupSort(out), nil
		}
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil && len(ids) > 0 {
		return dedupSort(ids), nil
	}
	return nil, fmt.Errorf("对端 /models 返回无法解析的格式（需为 OpenAI 形状 {\"data\":[{\"id\":\"...\"}]}）")
}

// dedupSort 去重 + 稳定排序（面板勾选列表顺序可预期，便于用户找模型）。
func dedupSort(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
