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
	"sync"
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
	// sem 实例级并发槽：nil = 不限制（max_concurrency<=0）。
	// 免费逆向源（DeepSeek/Kimi 网页版等）并发一高就容易触发风控，限流是保命手段。
	sem chan struct{}
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
	// QueueWait 并发槽被占满时最多排队多久（0 = 30s）。超时返回 429 让客户端自退避。
	QueueWait time.Duration

	// registry 按虚拟账号 UID 存每个实例的模型表/费率。装配期写，运行期只读。
	registry map[string]*instance

	// 上游 /models 元数据缓存（运行期读写，故单独加锁）。
	metaMu sync.Mutex
	meta   map[string]*catalogCache
}

// catalogCache 是一个实例的上游目录缓存。
type catalogCache struct {
	at   time.Time
	byID map[string]provider.ModelInfo
}

// catalogTTL 元数据缓存时长。模型清单不会每分钟变，10 分钟足够且避免每次刷新都打上游。
const catalogTTL = 10 * time.Minute

// New 构造客户端。
func New() *Client {
	return &Client{registry: make(map[string]*instance), meta: make(map[string]*catalogCache)}
}

// RegisterInstance 装配期注册一个自定义实例（uid 与注入 Pool 的虚拟账号 UID 一致）。
// maxConcurrency <= 0 表示不限制并发。
func (c *Client) RegisterInstance(uid string, models []string, rate float64, maxConcurrency int) {
	if c.registry == nil {
		c.registry = make(map[string]*instance)
	}
	cp := append([]string(nil), models...)
	inst := &instance{name: uid, models: cp, rate: rate}
	if maxConcurrency > 0 {
		inst.sem = make(chan struct{}, maxConcurrency)
	}
	c.registry[uid] = inst
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
	// 实例级并发闸门（借鉴 foxhui 的多窗口隔离/队列思想）：免费逆向源并发一高就
	// 触发风控，所以默认允许用户给每个实例设上限。max_concurrency=0 时不限制。
	inst := c.instanceOf(a)
	release, ok := c.acquire(inst)
	if !ok {
		// 排队超时：返回 429 让客户端按 Retry-After 自退避，而不是把上游打爆。
		return nil, http.StatusTooManyRequests,
			[]byte(`{"error":{"message":"自定义渠道并发已满（max_concurrency），请稍后重试","type":"rate_limit"}}`), nil
	}
	req, err := http.NewRequest(http.MethodPost, base+EpChat, bytes.NewReader(forceStream(body)))
	if err != nil {
		release()
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, a)

	resp, err := c.streamClient().Do(req)
	if err != nil {
		release()
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		release()
		return nil, resp.StatusCode, raw, nil
	}
	// 空闲看门狗：连续 idleTimeout 无字节即判卡死（对齐 R35 / loomy）。
	// 并发槽必须**跟着流一起释放**（handler 在读完/断开时 Close），否则流式请求会永久占槽。
	return newReleaseReader(provider.NewIdleReader(resp.Body, c.idleTimeout()), release), resp.StatusCode, nil, nil
}

// FetchModels 实现 provider.Upstream：返回该实例配置的模型表。
//
// 模型集合以**面板配置为准**（用户声明要暴露哪些），但会尝试用上游 /models 的
// 元数据补全上下文长度/最大输出/能力标签——很多客户端靠 context_length 决定何时
// 截断，缺失就只能靠猜（foxhui issue #128：用户抱怨"看模型名不知道能跑多大"）。
// 拉取失败一律回落成裸模型名，绝不因为补元数据而让模型列表消失。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	inst := c.instanceOf(a)
	if inst == nil {
		return nil, nil
	}
	meta := c.catalogFor(a)
	out := make([]provider.ModelInfo, 0, len(inst.models))
	for _, id := range inst.models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if mi, ok := meta[id]; ok {
			mi.ID, mi.Name = id, id // 以配置里的名字为准，元数据只补字段
			out = append(out, mi)
			continue
		}
		out = append(out, provider.ModelInfo{ID: id, Name: id, ContextFromAPI: false})
	}
	return out, nil
}

// catalogFor 返回实例的上游模型元数据（按 id 索引），带 10 分钟缓存。
// 任何失败都返回空表（调用方回落裸名），不向上抛错——模型列表不该被元数据拖垮。
func (c *Client) catalogFor(a *auth.Auth) map[string]provider.ModelInfo {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	c.metaMu.Lock()
	if hit := c.meta[uid]; hit != nil && time.Since(hit.at) < catalogTTL {
		byID := hit.byID
		c.metaMu.Unlock()
		return byID
	}
	c.metaMu.Unlock()

	infos, err := c.fetchCatalog(a)
	byID := make(map[string]provider.ModelInfo, len(infos))
	if err == nil {
		for _, mi := range infos {
			byID[mi.ID] = mi
		}
	}
	if c.meta == nil {
		c.meta = make(map[string]*catalogCache)
	}
	c.metaMu.Lock()
	c.meta[uid] = &catalogCache{at: time.Now(), byID: byID}
	c.metaMu.Unlock()
	return byID
}

// fetchCatalog 拉一次上游 /models 并解析成 ModelInfo 列表（5s 超时，不拖慢刷新）。
func (c *Client) fetchCatalog(a *auth.Auth) ([]provider.ModelInfo, error) {
	base := c.baseURL(a)
	if base == "" {
		return nil, fmt.Errorf("customopenai: base_url 为空")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+EpModels, nil)
	if err != nil {
		return nil, err
	}
	applyAuth(req, a)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游 /models 返回 HTTP %d", resp.StatusCode)
	}
	return parseCatalog(raw)
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

// acquire 取一个实例并发槽。max_concurrency<=0（sem==nil）时不限制，直接放行。
// 槽满时最多排队 QueueWait（默认 30s），仍未取到则返回 ok=false。
func (c *Client) acquire(inst *instance) (release func(), ok bool) {
	if inst == nil || inst.sem == nil {
		return func() {}, true
	}
	select {
	case inst.sem <- struct{}{}:
		return func() { <-inst.sem }, true
	default:
	}
	wait := c.QueueWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case inst.sem <- struct{}{}:
		return func() { <-inst.sem }, true
	case <-t.C:
		return nil, false
	}
}

// releaseReader 在流关闭时释放并发槽（流式响应必须等到读完/断开才算一次请求结束）。
type releaseReader struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func newReleaseReader(rc io.ReadCloser, release func()) io.ReadCloser {
	if release == nil {
		release = func() {}
	}
	return &releaseReader{ReadCloser: rc, release: release}
}

func (r *releaseReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
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
	infos, perr := parseCatalog(raw)
	if perr != nil {
		return nil, perr
	}
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		out = append(out, mi.ID)
	}
	return dedupSort(out), nil
}

// parseCatalog 解析上游 /models 响应，尽量把上下文长度与能力标签也带出来。
//
// 兼容三种形状：标准 {"data":[...]}、少数实现 {"models":[...]}、裸数组 ["id",...]。
// 元数据字段各家叫法不同（duruoxian 用 limit.context / capabilities.reasoning.supported，
// 常见实现用 context_length / max_tokens），全部做别名兼容；解析不到就留零值，
// 由消费方按"未知"处理（不要凭空编数字）。
func parseCatalog(raw []byte) ([]provider.ModelInfo, error) {
	type modelEntry struct {
		ID    string `json:"id"`
		Limit struct {
			Context int64 `json:"context"`
			Output  int64 `json:"output"`
		} `json:"limit"`
		ContextLength int64 `json:"context_length"`
		MaxTokens     int64 `json:"max_tokens"`
		MaxOutput     int64 `json:"max_output_tokens"`
		Capabilities  struct {
			Reasoning json.RawMessage `json:"reasoning"`
			Tools     json.RawMessage `json:"tools"`
			Vision    json.RawMessage `json:"vision"`
			Input     []string        `json:"input"`
		} `json:"capabilities"`
	}
	var env struct {
		Data   []modelEntry `json:"data"`
		Models []modelEntry `json:"models"`
	}
	out := make([]provider.ModelInfo, 0, 16)
	if err := json.Unmarshal(raw, &env); err == nil {
		entries := env.Data
		if len(entries) == 0 {
			entries = env.Models
		}
		for _, e := range entries {
			id := strings.TrimSpace(e.ID)
			if id == "" {
				continue
			}
			mi := provider.ModelInfo{ID: id, Name: id}
			mi.ContextWindow = firstPositive(e.Limit.Context, e.ContextLength)
			mi.MaxTokens = firstPositive(e.Limit.Output, e.MaxOutput, e.MaxTokens)
			mi.ContextFromAPI = mi.ContextWindow > 0 || mi.MaxTokens > 0
			mi.SupportsReasoning = rawBool(e.Capabilities.Reasoning)
			mi.SupportsTools = rawBool(e.Capabilities.Tools)
			mi.SupportsImages = rawBool(e.Capabilities.Vision)
			for _, in := range e.Capabilities.Input {
				if strings.EqualFold(strings.TrimSpace(in), "image") {
					mi.SupportsImages = true
				}
			}
			out = append(out, mi)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil && len(ids) > 0 {
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, provider.ModelInfo{ID: id, Name: id})
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("对端 /models 返回无法解析的格式（需为 OpenAI 形状 {\"data\":[{\"id\":\"...\"}]}）")
}

// firstPositive 返回第一个 >0 的值（字段别名兼容：同名语义不同叫法时取先出现的）。
func firstPositive(vs ...int64) int64 {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

// rawBool 兼容两种写法：能力字段既可能是 bool，也可能是 {"supported":true} 对象
// （duruoxian 的 capabilities.reasoning 就是后者）。缺失/null 一律视为"未知"= false。
func rawBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var obj struct {
		Supported bool `json:"supported"`
		Enabled   bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Supported || obj.Enabled
	}
	return false
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
