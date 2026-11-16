// Package login_qwenwork 实现千问办公（IDE 桌面版同款）OAuth 授权码登录：
// PKCE(S256) + Ory Hydra 授权码流。
//
// 链路（逆向自 ref/qwenwork_auth_20260919.saz，见备忘 §9）：
//  1. Start：生成 PKCE + state，返回授权 URL（client_id=qwenwork-desktop-app，
//     redirect_uri=在 qwenwork 注册的回调地址）。
//     授权页成功 → 浏览器被重定向到 https://gateway.qwenwork.cn/oauth/callback?code=...
//  2. 由于程序在 NAS/容器运行，浏览器无法回连 NAS 上的随机端口，所以上游把 code
//     带回了已注册的 gateway 回调页；用户需要复制链接栏完整地址粘回面板（
//     SubmitLoginCallback，已验证可工作），程序按与自动回调完全相同的 pending 通道
//     拿 code 并兑换 token（见 app.login_callback.go 的设计总结）。
//  3. Poll 兜底：与其它渠道一致的轮询语义（检查回调结果文件 / pending 通道），
//     供 app.pollLogin 调用。
//
// 注意：桌面 IDE 官方 redirect_uri 是 https://gateway.qwenwork.cn/oauth/callback
// （桌面进程内兑换），QwenWork 容器 | NAS 部署形态下利用同一已注册地址，
// 代码不指向 127.0.0.1 本机端口。授权端点/Token 端点以官方
// .well-known/openid-configuration 为准：
//   authorization_endpoint = https://qwenwork.cn/oauth2/auth
//   token_endpoint           = https://qwenwork.cn/auth/oauth/token
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy-api/internal/auth"
)

// 上游常量（IDE 桌面版同款）。
const (
	// 千问办公 OAuth 端点（官方 .well-known/openid-configuration 实测值）
	OAuthBase   = "https://qwenwork.cn"
	AuthURL     = OAuthBase + "/oauth2/auth"
	TokenURL    = OAuthBase + "/auth/oauth/token"
	ClientID    = "qwenwork-desktop-app"
	Scope       = "openid offline_access"
	// 在 qwenwork 注册过的 redirect_uri：授权页能回调回这个地址（URL 由用户手动
	// 粘回程序，见 SubmitCallback），而不是本地随机端口。
	CallbackURI = "https://gateway.qwenwork.cn/oauth/callback"
	callbackTTL = 5 * time.Minute // 本机回调 server 最长等待
)

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

// state 落盘的 PKCE 状态（供 Poll 与 SubmitCallback 共享）。
type state struct {
	Verifier     string `json:"verifier"`
	RedirectURI  string `json:"redirect_uri"`
	AuthURL      string `json:"auth_url"`
	CreatedAt    int64  `json:"created_at"`
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

// Start 发起登录：生成 PKCE + state，返回授权 URL。
//
// 与桌面 IDE 相同：redirect_uri 使用已在 qwenwork 注册的 `https://gateway.qwenwork.cn/oauth/callback`，
// 而非本机随机端口（NAS/容器部署下浏览器回连不到本地）。授权完成后浏览器会显示
// `https://gateway.qwenwork.cn/oauth/callback?code=XXXX`（或报错/白屏），用户把地址栏
// 完整地址粘回面板，由 SubmitCallback → pending 通道 → Poll 继续完成兑换。
func Start(_ *http.Client, statePath string) (string, error) {
	verifier, challenge := makePKCE()

	redirectURI := CallbackURI

	authURL := fmt.Sprintf("%s?client_id=%s&code_challenge=%s&code_challenge_method=S256&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		AuthURL, ClientID, challenge,
		url.QueryEscape(redirectURI),
		url.QueryEscape(Scope),
		uuid4Hex(), // state 服务端只透传，无需承载内容
	)

	st := state{Verifier: verifier, RedirectURI: redirectURI, AuthURL: authURL, CreatedAt: time.Now().Unix()}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}

	// 注意：不再起本机回调 server（重定向目标是已注册的 gateway 地址）；
	// code 的注入全部走 SubmitCallback（ labios 路径与 pollLogin 轮询）。
	log.Printf("qwenwork 登录：已生成授权 URL，等待用户粘回回调地址（state 文件 %s）", statePath)

	return authURL, nil
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

// Poll 单次检查登录结果；未完成返回 ErrPending；成功兑换 token 并清理。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	// 超时清理
	if time.Since(time.Unix(st.CreatedAt, 0)) > callbackTTL {
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录超时（5 分钟），请重试")
	}

	code, errMsg, ok := pending.snapshot()
	if !ok {
		if errMsg != "" {
			Shutdown()
			_ = os.Remove(statePath)
			return Result{}, fmt.Errorf("登录失败: %s", errMsg)
		}
		return Result{}, ErrPending
	}
	defer func() { Shutdown(); _ = os.Remove(statePath) }()

	// 兑换 token（Hydra 标准 token endpoint，public client + PKCE）
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {st.RedirectURI},
		"client_id":     {ClientID},
		"code_verifier": {st.Verifier},
	}
	req, err := http.NewRequest(http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("token exchange http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Result{}, fmt.Errorf("token parse: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if tok.AccessToken == "" {
		return Result{}, fmt.Errorf("token exchange: missing access_token (body: %s)", truncate(string(raw), 200))
	}

	res := Result{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		IDToken:      tok.IDToken,
	}
	// 从 JWT 解 uid/nickname（token endpoint 不返回 userinfo）
	res.UID, res.Nickname = parseJWTIdentity(tok.AccessToken)
	if res.UID == "" {
		res.UID, _ = parseJWTIdentity(tok.IDToken)
	}
	return res, nil
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

// makePKCE 生成 verifier + S256 challenge（43 字符 base64url，RFC 7636）。
func makePKCE() (verifier, challenge string) {
	buf := make([]byte, 48)
	_, _ = rand.Read(buf)
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
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
