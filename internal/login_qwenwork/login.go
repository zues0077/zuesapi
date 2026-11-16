// Package login_qwenwork 实现千问办公网页 device-flow 登录（无 App、无回调）。
//
// 链路（逆向自 QwenWorkCN 1.2.5 app.asar + 2026-09-30 在线探活，参考 xrp-cosy 规范）：
//  1. Start：生成 PKCE verifier/challenge(S256) + nonce + 持久化 machine_id，
//     返回 gateway.qwenwork.cn/device/selectAccounts?... 登录 URL。
//  2. 浏览器打开该 URL → 302 到 qwenwork.cn/oauth2/auth（Hydra）→ 用户钉钉扫码/登录
//     → 服务端把授权结果与我们的 nonce/challenge 绑定（完成态登记在服务端）。
//     全程不需要本地回调监听，也不需要用户粘贴回调地址。
//  3. Poll：拿 verifier 轮询 gateway.qwenwork.cn/api/v1/deviceToken/poll?nonce&verifier，
//     404 / 400(INVALID_DEVICE_FLOW) = 进行中，2xx = 拿到 token+refresh_token；
//     严格校验响应的 nonce/code_challenge/code_challenge_method 三连绑定后返回。
//
// 为什么不用 OAuth 授权码回调：gateway.qwenwork.cn/oauth/callback 是官方桌面客户端
// 专用回调页，要求 state 是客户端进程预置的会话。NAS 上用随机 state 发起授权，选完
// 账号跳过去会报 INVALID_OAUTH_CALLBACK（state 校验失败、code 被吞），地址栏不会留下
// 可复制的 code；device_code grant 对 qwenwork-desktop-app 未开放、动态注册被 WAF 拦、
// loopback 回调未注册 —— 故网页 OAuth 自兑这条路彻底不通，改用 device flow。
//
// refresh_token 为 ory_rt_ 前缀，与 App 链路同构，落盘后走 internal/qwenwork 的
// deviceToken/refresh 续期链，无需额外改造。
package login_qwenwork

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy-api/internal/auth"
)

// 上游常量（device flow，2026-09-30 探活实证）。
const (
	// DeviceAuthBase 网页登录页 origin（/device/selectAccounts 挂这里）
	DeviceAuthBase = "https://gateway.qwenwork.cn"
	// DevicePollPath 轮询端点（GET ?nonce&verifier&challenge_method=S256）
	DevicePollPath = "/api/v1/deviceToken/poll"
	// DeviceClientID device-flow 专用 OAuth client（注意与桌面 OAuth 的
	// qwenwork-desktop-app 不同；该值来自 QwenWorkCN 1.2.5 asar 常量）
	DeviceClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// DeviceRedirectURI App 深链回跳参数，纯网页登录无意义但 selectAccounts 白名单必填
	DeviceRedirectURI = "qwenwork-cn://"
	// PKCEChars verifier 字符集（OAuth RFC 7636 unreserved + App 实测）
	PKCEChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

	// machineIDFile 设备标识持久化文件名（从 statePath 派生，同目录）
	machineIDFile = "qwenwork-machine-id.txt"

	// 轮询行为（对齐 xrp-cosy 实测语义）
	pollInterval       = 1 * time.Second   // 每次 Poll 调用内的请求间隔
	pollRequestTimeout = 10 * time.Second  // 单请求超时
	maxNetFailures     = 5                 // 连续网络错误容忍次数
	callbackTTL        = 5 * time.Minute   // 复用旧名：登录总超时（app 层 loginTimeout 同值）
)

// gatewayBase 是 var 而非 const，便于单测替换成 httptest server。
var gatewayBase = "https://gateway.qwenwork.cn"

// ErrPending 表示授权尚未完成。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭证与账号信息。
type Result struct {
	AccessToken  string
	RefreshToken string // ory_rt_（轮换）
	ExpiresIn    int64  // 秒
	UID          string
	Nickname     string
	// IDToken 仅诊断用（不落盘到 auth，避免多余敏感面）
	IDToken string
}

// state 落盘的 device-flow 状态（供 Poll 共享）。
// 旧字段 RedirectURI/AuthURL 保留仅为兼容旧测试与历史 state 文件，device flow 不写它们。
type state struct {
	Verifier    string `json:"verifier"`               // PKCE verifier（poll 用）
	Challenge   string `json:"challenge,omitempty"`    // PKCE S256 challenge（绑定校验用）
	Nonce       string `json:"nonce,omitempty"`        // 本轮登录 nonce（绑定校验用）
	MachineID   string `json:"machine_id,omitempty"`   // 持久化设备标识
	RedirectURI string `json:"redirect_uri,omitempty"` // deprecated: OAuth 遗留，仅兼容
	AuthURL     string `json:"auth_url,omitempty"`     // deprecated: OAuth 遗留，仅兼容
	CreatedAt   int64  `json:"created_at"`
}

// pendingCode 展示的授权码（内存通道 + 文件双保险）：
// 手工粘贴（SubmitCallback）与自动回调（若有本机 server 监听时）注入同一条锁。
type pendingCode struct {
	mu       sync.Mutex
	code     string
	errMsg   string
	received bool
}

var (
	// pending 是登录流程唯一的数据通道（自动/手工粘贴共用）。
	pending = &pendingCode{}
)

// NewClient 简单 HTTP 客户端（无 cookie 需求；token 兑换用）。
func NewClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// Start 发起 device-flow 登录：生成 PKCE + nonce + 持久化 machine_id，
// 返回 gateway.qwenwork.cn/device/selectAccounts 登录 URL 供浏览器打开。
//
// 与网页 OAuth 的本质区别：用户完成登录后授权结果由服务端按 nonce/challenge 绑定，
// 程序拿 verifier 轮询 deviceToken/poll 直接换 token —— 不需要本地回调，也不需要用户
// 粘贴回调地址（NAS/容器部署下这是唯一能走通的路）。
func Start(_ *http.Client, statePath string) (string, error) {
	verifier := makeVerifier()
	challenge := pkceChallenge(verifier)
	nonce := uuid4Hex()
	machineID := resolveMachineID(statePath)

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machineID)
	q.Set("client_id", DeviceClientID)
	q.Set("redirect_uri", DeviceRedirectURI)
	authURL := DeviceAuthBase + "/device/selectAccounts?" + q.Encode()

	st := state{
		Verifier:  verifier,
		Challenge: challenge,
		Nonce:     nonce,
		MachineID: machineID,
		CreatedAt: time.Now().Unix(),
	}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}

	log.Printf("qwenwork 登录：已生成 device-flow 登录 URL（machine_id=%s，state 文件 %s）",
		maskMachineID(machineID), statePath)
	return authURL, nil
}

// resolveMachineID 读取（或首次生成并持久化）设备标识。
// 复用即稳定：服务端视其为同一设备，避免每轮登录都变成新设备。
func resolveMachineID(statePath string) string {
	fp := filepath.Join(filepath.Dir(statePath), machineIDFile)
	if raw, err := os.ReadFile(fp); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	id := uuid4Hex()
	if err := os.WriteFile(fp, []byte(id), 0o600); err != nil {
		log.Printf("qwenwork 登录：machine_id 持久化失败（本次用内存值）：%v", err)
	}
	return id
}

func maskMachineID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// SubmitCallback 让用户把浏览器地址栏里那串回调地址**手工**交回来。
//
// 为什么必须有这条路：授权完成后，上游把浏览器重定向到已在 qwenwork 注册过的
// 地址（`https://gateway.qwenwork.cn/oauth/callback?code=…`）；程序在 NAS/容器里跑时，
// 这个地址只在**用户的浏览器**上可见，送不回 NAS 这边。
// **授权其实已经成功，code 就在那条 URL 里**，用户复制地址栏粘过来即可。
//
// 注入走的是同一个 pending 通道，所以 Poll 的兑换、超时、清理逻辑
// 与自动路径完全一致，不存在两套实现。
//
// 接受三种粘贴形态：完整回调 URL、只带 query 的片段、裸 code（用户常只复制 code 值）。
func SubmitCallback(statePath, pasted string) error {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return fmt.Errorf("粘贴内容为空")
	}
	// 必须有进行中的登录：否则一个迟到的粘贴会把 code 塞进 pending，
	// 污染下一次登录的兑换（state 文件在 Poll 成功/超时/取消时都会删）。
	if _, err := os.Stat(statePath); err != nil {
		return fmt.Errorf("没有进行中的登录：请先点「+ 千问办公」拿到授权链接，授权完再粘贴回调")
	}

	code, errMsg := "", ""
	if u, err := url.Parse(pasted); err == nil && (u.Scheme != "" || strings.Contains(pasted, "?")) {
		q := u.Query()
		if e := q.Get("error"); e != "" {
			errMsg = e
			if d := q.Get("error_description"); d != "" {
				errMsg = d + " (" + e + ")"
			}
		} else {
			code = q.Get("code")
		}
	} else {
		code = pasted // 裸 code
	}

	if errMsg != "" {
		pending.fail(errMsg)
		log.Printf("qwenwork 登录：手工粘贴的回调带 error=%s，已按失败处理", errMsg)
		return nil
	}
	if code == "" {
		return fmt.Errorf("没解析出 code：请粘贴浏览器地址栏里那条完整回调地址（含 ?code=…），或只贴 code 值")
	}
	pending.set(code)
	// 只记长度，绝不记 code（不变量 2：日志零凭据）。
	log.Printf("qwenwork 登录：已接受手工粘贴的授权码（%d 字符），等待兑换", len(code))
	return nil
}

// netFailures 连续网络错误计数（成功响应后清零，达到上限视为终态失败）。
// app 每 2s 调一次 Poll，计数跨调用累积；用 state 的 nonce 做 key，换一轮登录自动重置。
var netFailures = struct {
	sync.Mutex
	nonce string
	count int
}{}

// Poll 单次 deviceToken/poll 请求；未完成返回 ErrPending；成功校验绑定后返回 Result。
//
// 语义（对齐 xrp-cosy 实测）：
//   - 404 / 400{errorCode:INVALID_DEVICE_FLOW} = 登录进行中 → ErrPending
//   - 其它 4xx/5xx = 协议/服务错误 → 终态失败
//   - 2xx = 完成 → 三连绑定校验（nonce/code_challenge/code_challenge_method）后返回 token
//   - 网络错误（含单请求超时）：连续达 5 次才视为终态失败
//   - 总超时与 app.loginTimeout（5min）一致，由 app 层 deadline 兜底；这里也自清一次。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	// 老 state 文件（OAuth 时代）没有 nonce/challenge：提示重发登录
	if st.Nonce == "" || st.Challenge == "" || st.Verifier == "" {
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录状态已过期（旧版格式），请重新点「+ 千问办公」发起登录")
	}
	// 超时清理
	if time.Since(time.Unix(st.CreatedAt, 0)) > callbackTTL {
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录超时（5 分钟），请重试")
	}

	q := url.Values{}
	q.Set("nonce", st.Nonce)
	q.Set("verifier", st.Verifier)
	q.Set("challenge_method", "S256")
	pollURL := gatewayBase + DevicePollPath + "?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-Id", uuid4Hex())
	req.Header.Set("X-QwenWork-Version", "1.2.5")
	// 单请求超时：用带超时的 client，避免拖住 app 的 2s ticker
	pollClient := *client
	pollClient.Timeout = pollRequestTimeout
	resp, err := pollClient.Do(req)
	if err != nil {
		// 网络错误：连续计数，未达上限按进行中处理
		if incrNetFailure(st.Nonce) < maxNetFailures {
			log.Printf("qwenwork device poll 网络错误（第 %d 次，容忍中）：%v", netFailureCount(), err)
			return Result{}, ErrPending
		}
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("deviceToken/poll 连续 %d 次网络错误：%w", maxNetFailures, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// 进行中：404 或 400{errorCode:INVALID_DEVICE_FLOW}
	if resp.StatusCode == http.StatusNotFound {
		resetNetFailure()
		return Result{}, ErrPending
	}
	if resp.StatusCode == http.StatusBadRequest {
		var peek struct {
			ErrorCode string `json:"errorCode"`
		}
		_ = json.Unmarshal(raw, &peek)
		if peek.ErrorCode == "INVALID_DEVICE_FLOW" {
			resetNetFailure()
			return Result{}, ErrPending
		}
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("deviceToken/poll 400: %s", truncate(string(raw), 300))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("deviceToken/poll http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	// 2xx：解析 + 三连绑定校验（防串号采纳他人凭据）
	var tok struct {
		Token                string `json:"token"`
		RefreshToken         string `json:"refresh_token"`
		Nonce                string `json:"nonce"`
		CodeChallenge        string `json:"code_challenge"`
		CodeChallengeMethod  string `json:"code_challenge_method"`
		ExpiresIn            int64  `json:"expires_in"`
		IDToken              string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Result{}, fmt.Errorf("deviceToken/poll parse: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if tok.Nonce != st.Nonce || tok.CodeChallenge != st.Challenge || tok.CodeChallengeMethod != "S256" {
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("deviceToken/poll 绑定校验失败（nonce/challenge 不匹配），拒绝采纳")
	}
	if tok.Token == "" || tok.RefreshToken == "" {
		return Result{}, fmt.Errorf("deviceToken/poll 响应缺少 token/refresh_token")
	}
	resetNetFailure()
	defer func() { Shutdown(); _ = os.Remove(statePath) }()

	res := Result{
		AccessToken:  tok.Token,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		IDToken:      tok.IDToken,
	}
	// 从 JWT 解 uid/nickname（poll 响应不返回 userinfo）
	res.UID, res.Nickname = parseJWTIdentity(tok.Token)
	if res.UID == "" {
		res.UID, _ = parseJWTIdentity(tok.IDToken)
	}
	return res, nil
}

// incrNetFailure/resetNetFailure/netFailureCount 网络错误计数（按 nonce 隔离，换轮重置）。
func incrNetFailure(nonce string) int {
	netFailures.Lock()
	defer netFailures.Unlock()
	if netFailures.nonce != nonce {
		netFailures.nonce, netFailures.count = nonce, 0
	}
	netFailures.count++
	return netFailures.count
}
func resetNetFailure() {
	netFailures.Lock()
	netFailures.nonce, netFailures.count = "", 0
	netFailures.Unlock()
}
func netFailureCount() int {
	netFailures.Lock()
	defer netFailures.Unlock()
	return netFailures.count
}

// Shutdown 当前无本地回调 server，保留此函数名用于对齐 app 层的调用
//（无实际作用，避免外部调用出现引用错误）。
func Shutdown() {}

// ---------------------------------------------------------------------------
// 保存凭证（对齐 qoder 渠道的 SaveAuth 契约）
// ---------------------------------------------------------------------------

// SaveAuth 以嵌套形原子写 auth 文件（与 internal/auth.Parse 读取格式一致）。
// 文件名 qwenwork-<uid>.json。
func SaveAuth(authDir string, r Result) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	expiresAt := int64(0)
	if r.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).Unix()
	} else {
		expiresAt = time.Now().Add(48 * time.Hour).Unix() // 观测 JWT 寿命 48h
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  auth.EncryptSecret(r.AccessToken),
			"refreshToken": auth.EncryptSecret(r.RefreshToken),
			"expiresAt":    expiresAt,
			"domain":       "qwenwork.cn",
			"apiHost":      "https://gateway.qwenwork.cn",
		},
		"account": map[string]any{
			"uid":      r.UID,
			"nickname": r.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, "qwenwork-"+r.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// pendingCode 操作
func (p *pendingCode) reset() {
	p.mu.Lock()
	p.code, p.errMsg, p.received = "", "", false
	p.mu.Unlock()
}
func (p *pendingCode) set(code string) { p.mu.Lock(); p.code, p.received = code, true; p.mu.Unlock() }
func (p *pendingCode) fail(msg string) { p.mu.Lock(); p.errMsg, p.received = msg, true; p.mu.Unlock() }

// snapshot 返回 (code, errMsg, 是否拿到 code)。
//
// ok 的语义必须是「拿到了 code」，不能直接用 received：fail() 也会把 received
// 置 true，于是 Poll 里的 `if !ok { if errMsg != "" → 按失败返回 }` 永远进不去，
// 用户在上游页面点「取消」时拿到的是「token exchange: missing access_token」
// 这种莫名其妙的消息，而不是真实原因（access_denied / user cancelled）。
// 该缺陷由 TestSubmitCallback_ErrorParam 抓到。
func (p *pendingCode) snapshot() (code, errMsg string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.errMsg, p.code != ""
}

// makeVerifier 生成 PKCE verifier：字符集与长度分布对齐 QwenWorkCN App
//（43 + rand(0..85) → 43..128，CSPRNG、无模偏），字符集 PKCEChars。
func makeVerifier() string {
	lengthN, err := rand.Int(rand.Reader, big.NewInt(86)) // [0,86)
	if err != nil {
		return strings.Repeat("A", 43) // 极端兜底：安全强度仍满足 RFC 7636
	}
	length := 43 + int(lengthN.Int64())
	charsN := big.NewInt(int64(len(PKCEChars)))
	out := make([]byte, length)
	for i := range out {
		idx, err := rand.Int(rand.Reader, charsN)
		if err != nil {
			return strings.Repeat("A", 43)
		}
		out[i] = PKCEChars[idx.Int64()]
	}
	return string(out)
}

// pkceChallenge 计算 S256 challenge（base64url 无填充，恒 43 字符）。
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// uuid4Hex 简单 state（服务端透传，不承载内容）。
func uuid4Hex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseJWTIdentity 从 access/id token 解 uid 与 username。
func parseJWTIdentity(token string) (uid, name string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	p := parts[1]
	// base64url 无填充 → 标准解码需补齐到 4 的倍数。
	// 注意：Go 的 % 保留被除数符号，-len(p)%4 在 len%4!=0 时为负，
	// strings.Repeat 负数会 panic（实测踩中：login poll panic: strings: negative Repeat count）。
	if r := len(p) % 4; r != 0 {
		p += strings.Repeat("=", 4-r)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return "", ""
	}
	var payload struct {
		Sub      string `json:"sub"`
		UserID   string `json:"user_id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "", ""
	}
	uid = payload.UserID
	if uid == "" {
		uid = payload.Sub
	}
	return uid, payload.Username
}

func readState(path string) (state, error) {
	var st state
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
