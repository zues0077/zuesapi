package app

import (
	"fmt"
	"log"
	"strings"

	loginqwenwork "workbuddy-api/internal/login_qwenwork"
	logintrae "workbuddy-api/internal/login_trae"
	"workbuddy-api/internal/provider"
)

// 本文件解决一个**部署形态**带来的缺陷：浏览器授权登录在本机回调上会失效。
//
// 千问办公与 TraeWork 逆向的是桌面 IDE 的 OAuth 流（PKCE + 本机回调）：程序在
// **自己所在机器**上起一个只监听 127.0.0.1:随机端口 的临时回调 server。
//   - Windows 单文件版：浏览器与程序同机 → 正常。
//   - 飞牛 / Docker：程序在 NAS 上监听，浏览器在用户电脑上 → 授权完成后上游把浏览器
//     重定向到 http://127.0.0.1:<port>/…，那是**用户自己的电脑**，端口没人监听 →
//     拒绝连接。授权其实已经成功，凭证就在那条 URL 里，只是送不回 NAS。
//
// 修法不是改绑定地址：上游注册的 redirect_uri 就是 127.0.0.1，改成 NAS 的 IP 会被
// 授权服务器拒绝（且会把回调端口暴露到局域网）。这里做的是**把那条 URL 交回给人**：
// 面板提示用户复制地址栏内容粘回来，程序按与自动回调完全相同的解析/落盘路径处理。
//
// 受影响范围：仅 qwenwork 与 traework/traecode（源码里用本机回调）。
// qoder / qodercn / qodercom 走设备码轮询、智谱清言走 CDP 或手工粘 Cookie、
// 小浣熊走协议深链、MonkeyCode / Loomy 从本机客户端导入 —— 都不依赖跨机回调，不受影响。

// SubmitLoginCallback 接受用户手工粘贴的回调地址（或裸 code），交给对应渠道的
// 与自动回调同一套注入路径。之后原有的轮询流程会完成兑换，无需额外操作。
func (a *App) SubmitLoginCallback(channel, pasted string) error {
	if strings.TrimSpace(pasted) == "" {
		return fmt.Errorf("请先粘贴浏览器地址栏里的完整回调地址")
	}
	k := provider.Kind(strings.TrimSpace(channel))
	if k == provider.TraeCode {
		k = provider.TraeWork // 与 JSON 登录一致：共账号渠道统一走 traework
	}
	// 先判渠道是否支持，再看登录状态：不支持的渠道（qoder 系等）应当直接说明
	// 「你不需要粘这个」，而不是笼统的「登录状态未初始化」。
	switch k {
	case provider.TraeWork, provider.QwenWork:
	default:
		return fmt.Errorf("渠道 %s 不使用本机回调登录，无需手工粘贴（qoder 系走设备码轮询、智谱清言走 CDP 或粘 Cookie）", channel)
	}
	if a.loginStateFP == "" {
		return fmt.Errorf("登录状态未初始化")
	}
	switch k {
	case provider.TraeWork:
		err := logintrae.SubmitCallback(a.loginStateFP, pasted)
		a.noteManualCallback("traework", err)
		return err
	case provider.QwenWork:
		err := loginqwenwork.SubmitCallback(a.loginStateFP, pasted)
		a.noteManualCallback("qwenwork", err)
		return err
	}
	return nil
}

// noteManualCallback 只记「哪个渠道收到了一次手工粘贴、成没成」，不记内容 ——
// 粘贴进来的东西就是凭据（不变量 2：日志零 token）。
func (a *App) noteManualCallback(channel string, err error) {
	if err != nil {
		log.Printf("%s 手工回调被拒：%v", channel, err)
		return
	}
	log.Printf("%s 已接受手工回调，等待轮询兑换", channel)
}

// LoginCallbackSupported 报告某渠道是否有「手工交回回调」这条路（面板据此决定
// 要不要显示粘贴框，避免给不支持的渠道摆一个点了只会报错的入口）。
func LoginCallbackSupported(channel string) bool {
	switch provider.Kind(strings.TrimSpace(channel)) {
	case provider.TraeWork, provider.TraeCode, provider.QwenWork:
		return true
	default:
		return false
	}
}
