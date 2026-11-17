// 本文件把「webai2api（duruoxian）手动凭证」直接写进本机那个服务，省掉用户
// 钻进它自己的管理面板这一步。
//
// 为什么由网关代劳：
//  1. 那个面板把访问限制成「客户端 IP + Host 都必须本机」，飞牛上只能靠局域网放宽
//     才进得去（已由 FPK 补丁处理），但用户仍要先找折叠项「手动填写凭证（不推荐）」；
//  2. 飞牛没有图形界面，它面板上的「浏览器登录」按钮必然失败，只能手动粘凭证；
//  3. 网关本来就和它同机，直接调它的 admin API 最省事，也避免把面板密码暴露到前端之外。
//
// 安全：只允许 http/https + **私有/回环地址**，避免这个接口被当成任意 URL 的代理（SSRF）。
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// webapiImportReq 面板提交的一次导入请求。
type webapiImportReq struct {
	BaseURL       string `json:"base_url"`       // 目标服务地址，如 http://127.0.0.1:8800
	AdminPassword string `json:"admin_password"` // 目标服务管理密码（默认 admin）
	Provider      string `json:"provider"`       // deepseek / chatglm / kimi / qwen
	Name          string `json:"name"`
	AuthToken     string `json:"auth_token"`
	DeviceID      string `json:"device_id"`
	SettingsToken string `json:"settings_token"`
	Cookie        string `json:"cookie"`
}

// webapiAllowedProviders 与上游 providers.PUBLIC_PROVIDERS 保持一致。
var webapiAllowedProviders = map[string]bool{
	"deepseek": true, "chatglm": true, "kimi": true, "qwen": true,
}

// isPrivateBaseURL 限制只能访问本机/局域网地址（含 localhost 与私有网段）。
func isPrivateBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("地址格式不正确，应形如 http://127.0.0.1:8800")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("只支持 http/https")
	}
	host := u.Hostname()
	if host == "localhost" {
		return strings.TrimRight(u.String(), "/"), nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("请填 IP 地址（如 127.0.0.1 或 192.168.1.246），不要用域名")
	}
	if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return "", fmt.Errorf("只允许本机或局域网地址")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// webapiPostJSON 向目标服务发一次 JSON 请求，返回状态码与响应体。
func webapiPostJSON(ctx context.Context, client *http.Client, fullURL, token string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// importWebAPAccount 登录目标 webai2api 管理端并新增一个平台账号。
// 返回目标服务的原始响应（ok/account 或 error），便于面板如实透出。
func importWebAPAccount(ctx context.Context, in webapiImportReq) (map[string]any, error) {
	base, err := isPrivateBaseURL(in.BaseURL)
	if err != nil {
		return nil, err
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider == "" {
		provider = "deepseek"
	}
	if !webapiAllowedProviders[provider] {
		return nil, fmt.Errorf("不支持的平台：%s（可选 deepseek / chatglm / kimi / qwen）", provider)
	}
	if strings.TrimSpace(in.AuthToken) == "" && strings.TrimSpace(in.Cookie) == "" {
		return nil, fmt.Errorf("auth_token 与 cookie 至少要填一个")
	}

	pwd := strings.TrimSpace(in.AdminPassword)
	if pwd == "" {
		pwd = "admin" // 上游默认密码
	}

	client := &http.Client{Timeout: 20 * time.Second}

	code, body, err := webapiPostJSON(ctx, client, base+"/admin/api/login", "", map[string]string{"password": pwd})
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%w（确认该服务已启动）", base, err)
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("登录目标服务失败（HTTP %d）：%s", code, snippet(body))
	}
	var lg struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &lg); err != nil || !lg.OK || lg.Token == "" {
		msg := lg.Error
		if msg == "" {
			msg = snippet(body)
		}
		return nil, fmt.Errorf("目标服务管理密码不对：%s", msg)
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = provider
	}
	payload := map[string]any{
		"provider":       provider,
		"name":           name,
		"auth_token":     strings.TrimSpace(in.AuthToken),
		"device_id":      strings.TrimSpace(in.DeviceID),
		"settings_token": strings.TrimSpace(in.SettingsToken),
		"cookie":         strings.TrimSpace(in.Cookie),
	}
	code, body, err = webapiPostJSON(ctx, client, base+"/admin/api/accounts", lg.Token, payload)
	if err != nil {
		return nil, fmt.Errorf("提交账号失败：%w", err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if code != http.StatusOK {
		return nil, fmt.Errorf("目标服务拒绝（HTTP %d）：%s", code, snippet(body))
	}
	if ok, _ := out["ok"].(bool); !ok {
		msg, _ := out["error"].(string)
		if msg == "" {
			msg = snippet(body)
		}
		return nil, fmt.Errorf("导入失败：%s", msg)
	}
	return out, nil
}

// snippet 截断响应体用于报错，避免把整页 HTML 塞进面板提示。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
