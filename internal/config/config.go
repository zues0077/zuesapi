// Package config 加载 JSON 配置 + 环境变量覆盖，并支持原子写回（GUI 面板修改）。
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Listen 监听地址：Host 为空表示全部接口。
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// IsLoopback 是否是「仅本机可访问」的监听地址：环回 IP / localhost / 空主机名
// （空主机名在 Go 里等同 :port，即全部网卡，但旧配置里 ":7863" 曾被当作本机语义，
// 这里按「显式写环回才算安全」处理——空主机名一律视为对外暴露）。
func (l Listen) IsLoopback() bool {
	h := strings.TrimSpace(strings.ToLower(l.Host))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Addr 返回 net.Listen 使用的地址串，如 "127.0.0.1:7863" / ":7863"。
func (l Listen) Addr() string {
	host := l.Host
	port := l.Port
	if port <= 0 {
		port = 7863
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// UnmarshalJSON 兼容旧版字符串形式（":7863" / "127.0.0.1:9999" / "9999"）。
func (l *Listen) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		l2, err := ParseListen(s)
		if err != nil {
			return err
		}
		*l = l2
		return nil
	}
	var o struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	l.Host, l.Port = o.Host, o.Port
	return nil
}

// ParseListen 解析 "host:port" / ":port" / "port" 三种形式。
func ParseListen(s string) (Listen, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Listen{}, fmt.Errorf("empty listen address")
	}
	host, portStr := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, portStr = s[:i], s[i+1:]
	} else if n, err := strconv.Atoi(host); err == nil {
		// 无冒号且整体是数字 → 仅端口（兼容旧配置 "7863"）
		return Listen{Port: n}, nil
	}
	port := 0
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n <= 0 || n > 65535 {
			return Listen{}, fmt.Errorf("bad listen port %q", portStr)
		}
		port = n
	}
	return Listen{Host: host, Port: port}, nil
}

// CustomChannel 用户自定义的 OpenAI 兼容上游实例（D 任务）。
// 每个实例在装配期变成一个虚拟账号（ApiHost=BaseURL, AccessToken=APIKey, UID=Name），
// 注入 customopenai 渠道的 Pool，天然参与 A 自动选渠 / E 多候选。
type CustomChannel struct {
	Name    string   `json:"name"`     // 实例唯一标识（= 虚拟账号 UID，= 模型前缀里的渠道组内 id）
	BaseURL string   `json:"base_url"` // OpenAI 兼容基址，含 /v1，不带尾斜杠（如 http://host:8800/v1）
	APIKey  string   `json:"api_key"`  // Bearer key；空表示上游无鉴权
	Models  []string `json:"models"`   // 该实例提供的模型 id（用于 /v1/models 与路由命中）
	Rate    float64  `json:"rate"`     // 费率倍率：0=免费优先；<0=未知（排最后）；>0=按值排序
	// Tag 账号列表里的展示标签：""/"custom" = 自定义；"reverse" = 逆向源。
	// 不参与任何逻辑，纯展示（用户要求：自定义账号在账号卡片上标出来，
	// duruoxian 这类网页反代源要换个标注）。
	Tag string `json:"tag,omitempty"`
	// MaxConcurrency 该实例的并发上限（0 = 不限制）。
	// 免费逆向源（网页版 DeepSeek/Kimi 等）并发一高极易触发风控，建议设 1~2。
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

// Config 顶层配置。
type Config struct {
	Listen Listen `json:"listen"`
	// PublicURL 是「外网 / 反代访问地址」，**只用于面板展示与复制，绝不参与 net.Listen**。
	//
	// 为什么单独立一个字段：监听表达的是「绑定哪张网卡」（NAS / 容器里通常填
	// 0.0.0.0），而经 Lucky / nginx 反代或 DDNS 暴露出去的那串 https://host:port
	// 表达的是「客户端从哪儿访问」。两者语义不同，把外网域名填进监听会让 bind
	// 直接失败，现象是面板再也打不开、重启也不恢复。
	//
	// 留空 = 沿用旧行为（面板按监听地址 + 局域网 IP 推导 API 地址）。
	// 合法值形如 https://nas.example.com:8443；已带 /v1 也接受，展示时不重复追加。
	PublicURL string `json:"public_url,omitempty"`

	APIKey    string `json:"api_key"`        // 空 = 不鉴权
	AdminPass string `json:"admin_password"` // 空 = 管理面板不鉴权（仅允许监听环回地址时为空）
	AuthDir   string `json:"auth_dir"`       // ./auths
	StateFile string `json:"state_file"`     // ./data/state.json
	Region    string `json:"region"`         // 只收 "cn"

	// Strategy 账号挑选策略：expire（临期优先，默认）/ credits（优先积分）/ roundrobin（负载均衡）。
	// 空串视为 expire。非法值在 normalize 时报错（面板只给三个合法选项）。
	Strategy string `json:"strategy,omitempty"`

	// MaxRotate 单次请求最多轮换多少个账号（候选耗尽即返回最后一次上游错误）。
	//
	// 为什么需要可配：3 次的默认值对小池够用，但账号多且大量处于冷却时，
	// 一次请求可能需要穿过更多冷却账号才能命中健康账号；此时 3 次会过早放弃，
	// 把「本可成功」的请求判成 503。上限 20，防止配置失误把单请求拖成
	// N 倍上游往返（每个候选至少一次完整请求）。
	// 0 / 负数 → 默认 3（保持历史行为不变）。
	MaxRotate int `json:"max_rotate,omitempty"`

	// ---- 会话粘性 ----
	//
	// SessionSticky 开启「按会话绑定账号」：同一 conversationId 恒定走同一账号。
	// 目的：上游 prompt 缓存命中（同一会话省缓存读取），且多轮对话里上下文里的
	// 工具调用 ID / assistant offset 跨轮对得上。
	//
	// 默认**开启**：客户端不发会话 id 时自动退化为「只有请求计数粘性」的旧行为，
	// 即最差情况等于没开，不存在退化风险。
	SessionSticky bool `json:"session_sticky"`
	// SessionStickyTTL 会话绑定有效期（分钟）。过期后重新分配，
	// 让长时间空闲的账号有机会被其它会话用上。默认 30，下限 1。
	SessionStickyTTL int `json:"session_sticky_ttl,omitempty"`
	// SessionStickyGC 后台清理间隔（分钟）。默认 5，下限 1。
	SessionStickyGC int `json:"session_sticky_gc,omitempty"`

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 默认 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int    `json:"checkin_hours,omitempty"` // 旧格式：[9,21]
		CheckinTimes   []string `json:"checkin_times,omitempty"` // 新格式：["09:00","21:30"]
		KeepaliveHours []int    `json:"keepalive_hours"`         // [22]
		// ExpiringThresholdHours 临期阈值（小时）：到期日在此范围内的积分视为「临期」,
		// Pick() 临期优先消耗。默认 24，下限 24（低于 24 会被钳到 24）。
		ExpiringThresholdHours int `json:"expiring_threshold_hours,omitempty"`

		// ---- 成长中心自动化 ----
		//
		// ⚠️ 这两项**默认全部关闭**，且必须由用户显式开启。
		//
		// 原因：它们会**真实改变账号状态**（兑换档位、消耗抽奖次数、派出旅行），
		// 与签到这类幂等动作不同 —— 抽奖次数一旦用掉不可撤销。默认开启会在用户
		// 不知情的情况下消耗其账号资源。
		//
		// 建议先用手动接口观察一两次结果（见 docs/任务体系协议.md），确认符合预期再开。

		// GrowthRedeemStreak 连登档位自动兑换 + 抽奖次数自动抽完。
		// 兑换按 streak 档位状态自动跳过未解锁项（未解锁返回 403，属预期）。
		GrowthRedeemStreak bool `json:"growth_redeem_streak,omitempty"`
		// GrowthTravel 猫猫旅行自动派出 + 到站自动领取。
		// 每自然日仅派一次（上游 daily_limit_reached 控制），重复调用无副作用。
		GrowthTravel bool `json:"growth_travel,omitempty"`

		// GrowthTravelLocation 旅行目的地（1~4，实测收益/时长区间相同）。0 视为 1。
		GrowthTravelLocation int `json:"growth_travel_location,omitempty"`

		// GrowthTasks 成长任务「一键完成」自动执行（默认关闭）。
		//
		// 这是**风险最高**的一项：它靠伪造客户端行为事件点亮任务进度，
		// 第三方逆向所得、本项目未独立实测；字段一旦与上游期望不符，
		// 上报会返回 200 却不入账（表现为「跑了但没涨」）。
		//
		// 关闭时面板仍可**手动**触发（POST /api/account/tasks），
		// 建议先手动跑一个账号核对结果，确认无误再打开定时自动。
		GrowthTasks bool `json:"growth_tasks,omitempty"`
		// GrowthTasksIncludeMP 自动执行时是否包含小程序口径任务。
		//
		// 默认关闭：mp 任务有反作弊（连发会被回滚），要求 45s/条的节奏，
		// 一项就可能跑几分钟，不适合放在无人的定时路径上。
		GrowthTasksIncludeMP bool `json:"growth_tasks_include_mp,omitempty"`
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
	} `json:"upstream"`

	// Server 入站 HTTP 服务参数。
	Server struct {
		// MaxRequestBodyMB 入站请求体上限（MiB）。0 = 默认 128。
		// 为什么要给到这么高：无状态客户端（Codex）每轮把整段历史原样重放，
		// 多张截图的 base64 会线性累积；gateway 转发前会把图缩到 1568px/JPEG，
		// 所以这个上限只需容纳**客户端原始体**，不代表上游会收到这么大的包。
		MaxRequestBodyMB int `json:"max_request_body_mb,omitempty"`
	} `json:"server"`

	// Compat 三接口兼容层（OpenAI Responses / Anthropic Messages）配置。
	// 零值即关闭模型名映射，仅接受 "channel/model" 形式。
	Compat struct {
		// DefaultChannel 无前缀模型名的傅底渠道，如 "workbuddy"。
		DefaultChannel string `json:"default_channel"`
		// ModelMap 裸模型名 → "channel/model"。key 以 * 结尾时按前缀通配匹配。
		ModelMap map[string]string `json:"model_map"`
		// CustomModels 裸模型名 → 候选 "channel/model" 阵列（如 workbuddy/glm-5.2）。
		//
		// 用来描述「一个虚拟模型名可以走哪些模型」：请求先匹配界面上
		// 用户勾选的候选列表（按免费/低费率排好序），哪个额度用尽就切下一个。
		CustomModels map[string][]string `json:"custom_models"`
		// MaxTokensCap 转发上游前对 max_tokens 封顶（0 = 不限制）。
		// Anthropic 客户端常发 64000，而多数上游上限更低，导致直接 400。
		MaxTokensCap int `json:"max_tokens_cap"`
	} `json:"compat"`

	// Proxies 单渠道上游代理：key 为渠道 kind（workbuddy/traework/qoder/qodercn/
	// qodercom/workbuddyai/qwenwork），value 为代理 URL（http/https/socks5）。
	// 空串或未列出的渠道直连。主要用途：给有区域限制的渠道走代理。
	Proxies map[string]string `json:"proxies,omitempty"`

	// CustomChannels 用户自定义 OpenAI 兼容上游实例（D 任务）。
	// 空切片 = 不启用该渠道，运行期与 v0.1.13 完全一致。面板配置后重启生效。
	CustomChannels []CustomChannel `json:"custom_channels,omitempty"`

	// RefreshIntervalMinutes 面板「模型列表和费率」的后台自动刷新间隔（分钟）。
	// 模型列表与费率都随上游变动，且面板依赖它们展示，故每 N 分钟统一拉一篇。
	// 0 = 默认 30 分钟；Normalize 钳制到 5～1440（5 分钟以下会打爆上游，
	// 24 小时以上用户按 anest 环境用旧缓存看）。
	RefreshIntervalMinutes int `json:"refresh_interval_minutes"`

	// 解析后
	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
	// ExpiringThresholdDur 临期阈值解析结果（normalize 里钳到 >= 24h）。
	ExpiringThresholdDur time.Duration `json:"-"`
	// SessionStickyTTLDur / SessionStickyGCDur 会话粘性的时间参数（解析后）。
	SessionStickyTTLDur time.Duration `json:"-"`
	SessionStickyGCDur  time.Duration `json:"-"`
}

// Default 默认配置。
//
// APIKey **不设固定默认值**，而是首次生成 config.json 时随机生成：
//
//   - 固定默认值意味着任何人拿到二进制就知道默认 key。发布到公开仓库后，
//     别人 clone 下来不配 config.json 就能直接访问使用者暴露的实例。
//   - 每个实例随机生成也让「忘记改 key」不再是安全隐患 —— 猜不出来。
//
// 随机源用 crypto/rand 而非 math/rand：key 的不可预测性是这里唯一的
// 要求，math/rand 的线性同余序列可被观察若干输出后反推状态。
// 生成失败时留空，由 normalize/首次启动的路径补齐（见 genAPIKey）。
func Default() *Config {
	c := &Config{
		Listen:    Listen{Host: "127.0.0.1", Port: 7863},
		APIKey:    genAPIKey(),
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		Region:    "cn",
		Strategy:  "expire",
		MaxRotate: 3,
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.CheckinTimes = []string{"09:00", "21:00"}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.ExpiringThresholdHours = 24
	c.Upstream.TimeoutSeconds = 120
	c.Compat.DefaultChannel = "workbuddy"
	c.Compat.MaxTokensCap = 32000
	c.Proxies = map[string]string{}
	c.CustomChannels = []CustomChannel{}
	c.SessionSticky = true
	c.SessionStickyTTL = 30
	c.SessionStickyGC = 5
	return c
}

// genAPIKey 生成随机 API Key（24 字节熵 → base64url 约 32 字符）。
//
// 为什么随机而不是固定字符串：
//
//	固定默认值会随二进制一起公开。本项目要发布到公开仓库，任何人下载 exe
//	就知道默认 key —— 若使用者没改（很多人不会改），等于把服务敞开着。
//	随机生成让「忘了改 key」不再是安全隐患。
//
// base64url 而非 hex：同熵下短 1/3（24B → 32 字符 vs 48 字符），
// 且 URL 安全，可直接放进客户端配置。去掉 '=' 填充避免 JSON 里出现转义困扰。
//
// crypto/rand 而非 math/rand：后者是线性同余序列，观察若干输出即可
// 反推内部状态并预测后续值 —— 对 key 这种「不可预测即可」的场景不可接受。
func genAPIKey() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// 极罕见（系统熵源故障）。此处不适合 panic —— 配置生成失败应让
		// 程序继续启动并由用户自行设置，因此退化为空值；
		// normalize 会保持它为空，监听非 127.0.0.1 时 main 会提示必须设置。
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Load 从文件读，再用 WILDWORK_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		// Default() 带有新字段默认值；老配置没有 checkin_times 时必须让旧的
		// checkin_hours 生效，而不能被 Default 的 [09:00,21:00] 覆盖。
		var shape struct {
			Schedule map[string]json.RawMessage `json:"schedule"`
		}
		if json.Unmarshal(raw, &shape) == nil {
			if _, ok := shape.Schedule["checkin_times"]; !ok {
				c.Schedule.CheckinTimes = nil
			}
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadOrCreate 读配置；文件不存在时用默认值创建（**并应用 WILDWORK_* 环境变量**）。
// 第二个返回值表示本次是否为首次创建。
//
// 为什么不让调用方自己 Default()：早期启动路径在「配置文件不存在」分支
// 直接取 Default()，**跳过了 applyEnv**。而容器部署最典型的情形恰恰是
// 「首次启动 + 无 config.json + 配置全来自环境变量」，此时 env 被静默忽略，
// 程序用上默认的 listen=127.0.0.1 与默认 API key，表现是
// 「容器起来了但 7863 端口不通」，而日志里没有任何异常 —— 很难定位。
func LoadOrCreate(path string) (*Config, bool, error) {
	c, err := Load(path)
	if err == nil {
		return c, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	c = Default()
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, false, err
	}
	// 落盘失败不阻断启动（与既有行为一致）：配置在内存中依然可用。
	if err := Save(c, path); err != nil {
		fmt.Fprintf(os.Stderr, "write default config: %v\n", err)
	}
	return c, true, nil
}

// Save 以 0600 原子写回配置。
func Save(c *Config, path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ParseClockTimes 将 HH:MM 列表转换为当天分钟数（0..1439）。
func ParseClockTimes(values []string) ([]int, error) {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		parts := strings.Split(strings.TrimSpace(v), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid time %q", v)
		}
		h, errH := strconv.Atoi(parts[0])
		m, errM := strconv.Atoi(parts[1])
		if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return nil, fmt.Errorf("invalid time %q", v)
		}
		minute := h*60 + m
		if !seen[minute] {
			seen[minute] = true
			out = append(out, minute)
		}
	}
	sort.Ints(out)
	return out, nil
}

// FormatClockTimes 将当天分钟数格式化为排序后的 HH:MM 列表。
func FormatClockTimes(minutes []int) []string {
	out := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	formatted := make([]string, 0, len(out))
	for _, m := range out {
		formatted = append(formatted, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return formatted
}

func applyEnv(c *Config) {
	if v := os.Getenv("WILDWORK_LISTEN"); v != "" {
		if l, err := ParseListen(v); err == nil {
			c.Listen = l
		}
	}
	if v := os.Getenv("WILDWORK_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WILDWORK_ADMIN_PASSWORD"); v != "" {
		c.AdminPass = v
	}
	if v := os.Getenv("WILDWORK_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WILDWORK_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WILDWORK_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("WILDWORK_STRATEGY"); v != "" {
		c.Strategy = v
	}
	if v := os.Getenv("WILDWORK_MAX_ROTATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxRotate = n
		}
	}
	if v := os.Getenv("WILDWORK_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("WILDWORK_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WILDWORK_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("WILDWORK_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("WILDWORK_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WILDWORK_DEFAULT_CHANNEL"); v != "" {
		c.Compat.DefaultChannel = v
	}
	if v := os.Getenv("WILDWORK_MAX_TOKENS_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Compat.MaxTokensCap = n
		}
	}
	// WILDWORK_REFRESH_INTERVAL_MINUTES 覆盖后台「模型列表 + 费率」自动刷新间隔。
	if v := os.Getenv("WILDWORK_REFRESH_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RefreshIntervalMinutes = n
		}
	}
}

// NormalizePublicURL 清洗并校验「外网访问地址」，返回去掉首尾空白与末尾 / 的结果。
// 空串合法（表示未配置，面板回退到按监听地址推导）。
//
// 校验故意只做「能不能当 URL 用」这一层：它是展示字段，填错的代价是复制到一个
// 打不开的地址，不该因此影响服务。严格校验放在面板输入口（SetPublicURL），
// 启动路径（normalize）只告警并清空 —— 见那里的注释。
func NormalizePublicURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("无法解析：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("需以 http:// 或 https:// 开头，实得 %q", s)
	}
	if u.Host == "" {
		return "", fmt.Errorf("缺少主机名：%q", s)
	}
	// 拒绝 https://user:pass@host —— 这个字段会被原样写进 app.log 和面板，
	// 带 userinfo 等于把凭据塞进日志（不变量 2：日志零凭据）。
	if u.User != nil {
		return "", fmt.Errorf("地址里不要带用户名/密码（https://user:pass@host 形式被拒绝）：%q", s)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("不要带查询参数或锚点：%q", s)
	}
	return strings.TrimRight(s, "/"), nil
}

// APIBaseFromPublic 由外网访问地址算出给客户端的 OpenAI 兼容基址。
// 已经以 /v1 结尾就原样用，否则补一个 /v1（用户常只填到域名+端口）。
func APIBaseFromPublic(pub string) string {
	pub = strings.TrimRight(strings.TrimSpace(pub), "/")
	if pub == "" {
		return ""
	}
	if strings.HasSuffix(pub, "/v1") {
		return pub
	}
	return pub + "/v1"
}

func (c *Config) normalize() error {
	var err error
	// public_url 是纯展示字段：填错只该让「复制到的地址不对」，不该让整个服务起不来。
	// 所以这里**不返回 error**，只告警并清空（面板会退回按监听地址推导）。
	// 真正的严格校验在面板写入口 SetPublicURL，那里能当场把原因告诉用户。
	if pu, perr := NormalizePublicURL(c.PublicURL); perr != nil {
		log.Printf("public_url %q 不合法（%v），已忽略并回退为按监听地址推导 API 地址", c.PublicURL, perr)
		c.PublicURL = ""
	} else {
		c.PublicURL = pu
	}
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// 入站请求体上限：0 = 默认；负数归零，过小/过大一律钳到合理区间
	//（下限 8MiB 沿用历史最低值，上限 1024MiB 防手滑写成天文数字把内存撑爆）。
	if c.Server.MaxRequestBodyMB < 0 {
		c.Server.MaxRequestBodyMB = 0
	}
	if c.Server.MaxRequestBodyMB > 0 && c.Server.MaxRequestBodyMB < 8 {
		c.Server.MaxRequestBodyMB = 8
	}
	if c.Server.MaxRequestBodyMB > 1024 {
		c.Server.MaxRequestBodyMB = 1024
	}
	// 临期阈值：默认 24h，下限 24h（日期粒度的到期判定低于一天没有意义）。
	if c.Schedule.ExpiringThresholdHours <= 0 {
		c.Schedule.ExpiringThresholdHours = 24
	}
	if c.Schedule.ExpiringThresholdHours < 24 {
		c.Schedule.ExpiringThresholdHours = 24
	}
	c.ExpiringThresholdDur = time.Duration(c.Schedule.ExpiringThresholdHours) * time.Hour
	if c.Compat.MaxTokensCap < 0 {
		c.Compat.MaxTokensCap = 0 // 负数视为「不限制」，避免误用导致 max_tokens 被置 0
	}
	if c.Listen.Port <= 0 {
		c.Listen.Port = 7863
	}
	c.AdminPass = strings.TrimSpace(c.AdminPass)
	// 代理配置清洗：剔除空值，校验 URL 形态（http/https/socks5）。
	for k, v := range c.Proxies {
		v = strings.TrimSpace(v)
		if v == "" {
			delete(c.Proxies, k)
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("proxies[%s]: 无效代理地址 %q（应为 http://host:port 或 socks5://host:port）", k, v)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
		default:
			return fmt.Errorf("proxies[%s]: 不支持的代理协议 %q（仅 http/https/socks5）", k, u.Scheme)
		}
		c.Proxies[k] = v
	}
	// 自定义渠道清洗：name 唯一非空、base_url 为 http/https、模型 id 去空、rate 归一。
	{
		seen := make(map[string]bool, len(c.CustomChannels))
		clean := make([]CustomChannel, 0, len(c.CustomChannels))
		for _, cc := range c.CustomChannels {
			cc.Name = strings.TrimSpace(cc.Name)
			if cc.Name == "" {
				continue
			}
			if seen[cc.Name] {
				return fmt.Errorf("custom_channels: 实例名重复 %q", cc.Name)
			}
			seen[cc.Name] = true
			cc.BaseURL = strings.TrimRight(strings.TrimSpace(cc.BaseURL), "/")
			cc.APIKey = strings.TrimSpace(cc.APIKey)
			if cc.BaseURL != "" {
				u, err := url.Parse(cc.BaseURL)
				if err != nil || u.Scheme == "" || u.Host == "" {
					return fmt.Errorf("custom_channels[%s]: 无效 base_url %q（应为 http://host:port/v1）", cc.Name, cc.BaseURL)
				}
				if u.Scheme != "http" && u.Scheme != "https" {
					return fmt.Errorf("custom_channels[%s]: 不支持的协议 %q（仅 http/https）", cc.Name, u.Scheme)
				}
			}
			models := make([]string, 0, len(cc.Models))
			for _, m := range cc.Models {
				m = strings.TrimSpace(m)
				if m != "" {
					models = append(models, m)
				}
			}
			cc.Models = models
			switch cc.Tag {
			case "reverse":
				cc.Tag = "webapi" // 旧值迁移（面板文案已改为 自定义API/webAPI）
			case "", "custom", "webapi":
			default:
				cc.Tag = "" // 未知标签一律按「自定义API」处理，不报错（纯展示字段）
			}
			// 并发上限：0=不限制；负数按 0 处理；上限 32（再多只会把免费源打爆）。
			if cc.MaxConcurrency < 0 || cc.MaxConcurrency > 32 {
				if cc.MaxConcurrency > 32 {
					cc.MaxConcurrency = 32
				} else {
					cc.MaxConcurrency = 0
				}
			}
			clean = append(clean, cc)
		}
		c.CustomChannels = clean
	}
	if c.Region == "" {
		c.Region = "cn"
	}
	c.Region = strings.ToLower(c.Region)
	if c.Region != "cn" && c.Region != "global" {
		return fmt.Errorf("region must be cn or global, got %q", c.Region)
	}
	// 挑号策略：空串 → 默认 expire；显式非法值必须报错而不是静默回落
	// （静默回落会让用户以为切到了 roundrobin，实际却在跑 expire）。
	s := strings.ToLower(strings.TrimSpace(c.Strategy))
	switch s {
	case "":
		c.Strategy = "expire"
	case "expire", "credits", "roundrobin":
		c.Strategy = s
	default:
		return fmt.Errorf("strategy must be one of expire/credits/roundrobin, got %q", c.Strategy)
	}
	// 单请求轮换上限：下限 1（否则一次都不试，直接把候选耗尽当成失败），
	// 上限 20（防止误配把单请求拖成 20 次上游往返）。
	if c.MaxRotate <= 0 {
		c.MaxRotate = 3
	}
	if c.MaxRotate > 20 {
		c.MaxRotate = 20
	}
	// 会话粘性时间参数：钳到下限 1 分钟（0 或负数视为未配置 → 落默认）。
	if c.SessionStickyTTL <= 0 {
		c.SessionStickyTTL = 30
	}
	if c.SessionStickyTTL < 1 {
		c.SessionStickyTTL = 1
	}
	if c.SessionStickyGC <= 0 {
		c.SessionStickyGC = 5
	}
	if c.SessionStickyGC < 1 {
		c.SessionStickyGC = 1
	}
	c.SessionStickyTTLDur = time.Duration(c.SessionStickyTTL) * time.Minute
	c.SessionStickyGCDur = time.Duration(c.SessionStickyGC) * time.Minute
	// 模型列表 + 费率 的后台刷新间隔：0 / 负数 → 默认 30 分钟；
	// 低于 5 分钟会打爆上游的模型/费率接口，超过 24 小时用户只能用旧缓存看面板。
	if c.RefreshIntervalMinutes <= 0 {
		c.RefreshIntervalMinutes = 30
	}
	if c.RefreshIntervalMinutes < 5 {
		c.RefreshIntervalMinutes = 5
	}
	if c.RefreshIntervalMinutes > 1440 {
		c.RefreshIntervalMinutes = 1440
	}
	// 兼容旧版 checkin_hours；新版本统一规范化为 HH:MM。
	if len(c.Schedule.CheckinTimes) == 0 {
		c.Schedule.CheckinTimes = make([]string, 0, len(c.Schedule.CheckinHours))
		for _, h := range c.Schedule.CheckinHours {
			if h < 0 || h > 23 {
				return fmt.Errorf("schedule.checkin_hours: hour out of range: %d", h)
			}
			c.Schedule.CheckinTimes = append(c.Schedule.CheckinTimes, fmt.Sprintf("%02d:00", h))
		}
	}
	mins, err := ParseClockTimes(c.Schedule.CheckinTimes)
	if err != nil {
		return fmt.Errorf("schedule.checkin_times: %w", err)
	}
	c.Schedule.CheckinTimes = FormatClockTimes(mins)
	if len(c.Schedule.CheckinHours) == 0 {
		for _, m := range mins {
			if m%60 == 0 {
				c.Schedule.CheckinHours = append(c.Schedule.CheckinHours, m/60)
			}
		}
	}
	return nil
}
