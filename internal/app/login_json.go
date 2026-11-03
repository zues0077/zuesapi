package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/login_glm"
	"workbuddy-api/internal/provider"
)

// 本文件实现「JSON 登录」：用户把凭据 JSON 粘贴到面板，直接落盘为账号，
// 不走浏览器 OAuth / CDP / 本机客户端导入。
//
// 典型场景：
//   - 从另一台机器拷贝过来的 auths/<渠道>-<uid>.json（嵌套格式）；
//   - 手里只有 access_token / refresh_token（扁平格式）；
//   - OAuth 登录流程在本机不可用（协议回调被占用、无默认浏览器等）时的兜底。
//
// 落盘格式与 internal/auth.Parse 的嵌套分支严格对齐（复用 authDoc），
// token 统一走 writeAuthFile 加密（Windows DPAPI），日志绝不打印凭据值。

// JSONLoginResult JSON 登录结果（供面板展示）。
type JSONLoginResult struct {
	Channel string `json:"channel"`
	UID     string `json:"uid"`
	File    string `json:"file"`
	Note    string `json:"note,omitempty"`
}

// jsonFlatAuth 扁平格式：与嵌套格式字段同名的顶层 JSON。
type jsonFlatAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
	ApiHost      string `json:"apiHost"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// jsonLoginChannel 白名单：允许 JSON 登录的渠道。
// traecode 与 traework 共账号，统一走 traework；qoder 界面已下线但仍保留路由可导入。
var jsonLoginChannel = map[provider.Kind]bool{
	provider.WorkBuddy:   true,
	provider.WorkBuddyAI: true,
	provider.TraeWork:    true,
	provider.Qoder:       true,
	provider.QoderCN:     true,
	provider.QoderCOM:    true,
	provider.QwenWork:    true,
	provider.MonkeyCode:  true,
	provider.Raccoon:     true,
	provider.Loomy:       true,
	provider.GLM:         true,
}

// SubmitJSONAuth 提交用户粘贴的凭据 JSON：解析 → 校验 → 落盘 → 重载账号池。
//
// 支持两种格式（自动识别）：
//
//	嵌套（与 auths/*.json 一致）：
//	  {"auth":{"accessToken":"…","refreshToken":"…","expiresAt":0,"domain":"…"},
//	     "account":{"uid":"…","nickname":"…"}}
//	扁平：
//	  {"accessToken":"…","refreshToken":"…","expiresIn":3600,"uid":"…"}
//
// uid 缺失时尽力从 JWT claims（uid/name/sub）推导；glm 渠道可仅凭
// refreshToken 走上游验证取回 uid。返回保存后的 uid 供面板定位新账号。
func (a *App) SubmitJSONAuth(channel, jsonText string) (*JSONLoginResult, error) {
	k, err := jsonLoginKind(channel)
	if err != nil {
		return nil, err
	}
	uid, file, err := a.importAuthJSON(k, jsonText)
	if err != nil {
		return nil, err
	}
	a.reloadAccounts()
	a.afterAccountAdded(k)
	return &JSONLoginResult{
		Channel: string(k), // 回给面板的是渠道名（Kind），不是文件名前缀
		UID:     uid,
		File:    filepath.Base(file),
		Note:    "账号已通过 JSON 导入，凭据与浏览器登录落盘格式一致",
	}, nil
}

// jsonLoginKind 校验渠道白名单并归并共账号渠道（traecode → traework）。
func jsonLoginKind(channel string) (provider.Kind, error) {
	k := provider.Kind(strings.TrimSpace(channel))
	if k == provider.TraeCode {
		k = provider.TraeWork // 共账号渠道统一落 traework 前缀
	}
	if !jsonLoginChannel[k] {
		return "", fmt.Errorf("渠道 %q 不支持 JSON 登录", channel)
	}
	return k, nil
}

// importAuthJSON 解析并落盘**一条**凭据，不做 reload、不触发签到/刷新。
//
// 拆出来是为了批量导入：单条路径每条都 reload 一次尚可接受，批量 20 条就变成
// 20 次全渠道重扫 + 20 次 afterAccountAdded（会真实打上游接口）。批量在末尾
// 统一 reload 一次、按渠道各触发一次后置动作即可。
func (a *App) importAuthJSON(k provider.Kind, jsonText string) (uid, file string, err error) {
	return a.importAuthJSONWithBudget(k, jsonText, nil)
}

// importAuthJSONWithBudget 同上，但允许调用方限制「缺 uid 的 glm 条目触发上游验证」的次数。
//
// 批量路径必须限额：glm 每条没有 uid 的凭据都会真实打一次 Validate，200 条就可能是
// 200 次串行上游请求（几分钟），期间面板请求一直挂着。预算用满后同类条目直接失败，
// 不再打上游；单条路径传 nil，行为与以前一致（只验一条，没有放大问题）。
func (a *App) importAuthJSONWithBudget(k provider.Kind, jsonText string, glmValidateBudget *int) (uid, file string, err error) {
	jsonText = strings.TrimSpace(jsonText)
	if jsonText == "" {
		return "", "", fmt.Errorf("凭据 JSON 为空")
	}
	doc, err := parseAuthJSONWithBudget(k, jsonText, glmValidateBudget)
	if err != nil {
		return "", "", err
	}
	uid = strings.TrimSpace(doc.Account.UID)
	if uid == "" {
		return "", "", fmt.Errorf("JSON 里缺少 uid（account.uid）：无法生成凭据文件名")
	}
	file, err = a.writeAuthFile(authFilePrefix(k), uid, doc)
	if err != nil {
		return "", "", err
	}
	log.Printf("json 登录成功 channel=%s uid=%s file=%s", k, uid, filepath.Base(file))
	return uid, file, nil
}

// authFilePrefix 返回该渠道凭据文件的落盘前缀。
//
// 绝大多数渠道前缀恰好等于 provider.Kind，**唯一例外是 TraeWork**：Kind 是
// "traework"，而登录编排 internal/login_trae.SaveAuth 写的是 `trae-<uid>.json`，
// 加载器 auth.LoadTraeDir 也只扫 `trae-*.json`。照抄 Kind 会写出
// `traework-<uid>.json` —— 没有任何加载器认识它，SyncToDir 又不删磁盘文件，
// 现象是「接口返回导入成功、面板里却始终没有这个账号」，且与平台无关
// （Windows 上用 JSON 导入 TraeWork 同样不显示，只有走浏览器 OAuth 才正常）。
// 回归测试 TestSubmitJSONAuth_RoundTripsThroughLoader 逐渠道锁死这条映射。
func authFilePrefix(k provider.Kind) string {
	if k == provider.TraeWork {
		return "trae"
	}
	return string(k)
}

// ExportAccountJSON 把已有账号导出为可粘贴的凭据 JSON（嵌套 auths 格式，token 为明文）。
//
// 用途：迁移到另一台机器（配合 JSON 登录）、备份、或给另一实例共享账号。
// 文件落盘的 token 是 DPAPI 密文，导出时统一解回明文 —— 因此导出结果
// 等同于凭据本身，只在面板里展示、不写日志。
func (a *App) ExportAccountJSON(uid string) (map[string]any, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, fmt.Errorf("缺少 uid")
	}
	// 文件名形如 <kind>-<uid>.json；按后缀匹配可跨渠道命中（uid 全局唯一）。
	pattern := filepath.Join(a.cfg.AuthDir, "*-"+sanitizeUID(uid)+".json")
	matches, _ := filepath.Glob(pattern)
	if len(matches) > 0 {
		raw, err := os.ReadFile(matches[0])
		if err != nil {
			return nil, fmt.Errorf("读取凭据失败：%w", err)
		}
		au, err := auth.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("解析凭据失败：%w", err)
		}
		log.Printf("json 导出 uid=%s file=%s", uid, filepath.Base(matches[0]))
		return authToExportDoc(au), nil
	}

	// 兜底：auths/ 里没有对应文件时，从内存账号池里找（任何渠道、
	// 任何方式加进来的账号都行 —— 池内 Auth 字段本身就是解密后的明文）。
	if au := a.findPoolAuth(uid); au != nil {
		log.Printf("json 导出 uid=%s source=pool", uid)
		return authToExportDoc(au), nil
	}
	return nil, fmt.Errorf("未找到 uid=%s 的账号（既无凭据文件也不在账号池中）", uid)
}

// findPoolAuth 在所有渠道的账号池里按 uid 找账号（找不到返回 nil）。
func (a *App) findPoolAuth(uid string) *auth.Auth {
	for _, k := range []provider.Kind{
		provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork,
		provider.Qoder, provider.QoderCN, provider.QoderCOM, provider.QwenWork,
		provider.MonkeyCode, provider.Raccoon, provider.Loomy, provider.GLM,
	} {
		if rt := a.runtime(k); rt != nil && rt.Pool != nil {
			if au := rt.Pool.AuthByUID(uid); au != nil {
				return au
			}
		}
	}
	return nil
}

// authToExportDoc 把池内 Auth 转成嵌套导出文档（token 明文）。
//
// 必须覆盖 internal/auth.Auth 的**全部**凭据字段：少写一个，就等于「导出 → 换机
// → 导入」往返后该渠道永久缺这块身份。最要命的是 TraeWork 的 machineId/deviceId
// —— 签到与对话头都从 auth 文件读它们（internal/traework/headers.go 的
// X-Machine-Id / x-device-id），丢了就是「无设备身份」，按 R37 的实测结论正是
// 20401（设备数上限）与 4017（common risk control）的诱因；qoder 系的
// machineToken/machineType 与 MonkeyCode 的三把 secret 同理。
// 空字段不写入，保持 JSON 精简并兼容旧版导入侧。
func authToExportDoc(au *auth.Auth) map[string]any {
	// 池内 Auth 的 token 会被 RefreshToken 持锁改写（不变量 19/20），导出不加锁
	// 就可能读到「新 access + 旧 refresh」的混合体，粘到另一台机器上直接不可用。
	au.Lock()
	defer au.Unlock()
	sec := map[string]any{
		"accessToken":  au.AccessToken,
		"refreshToken": au.RefreshToken,
		"expiresAt":    au.ExpiresAt,
		"domain":       au.Domain,
	}
	for key, v := range map[string]string{
		"apiHost":       au.ApiHost,
		"machineId":     au.MachineID,
		"deviceId":      au.DeviceID,
		"machineToken":  au.MachineToken,
		"machineType":   au.MachineType,
		"signingSecret": au.SigningSecret,
		"consoleCookie": au.ConsoleCookie,
		"baizhiCookie":  au.BaizhiCookie,
	} {
		if v != "" {
			sec[key] = v
		}
	}
	acct := map[string]any{
		"uid":      au.UID,
		"nickname": au.Nickname,
	}
	if au.EnterpriseID != "" {
		acct["enterpriseId"] = au.EnterpriseID
	}
	return map[string]any{"auth": sec, "account": acct}
}

// parseAuthJSON 把粘贴的 JSON 解析成 authDoc，兼容嵌套与扁平两种格式。
func parseAuthJSON(k provider.Kind, text string) (authDoc, error) {
	return parseAuthJSONWithBudget(k, text, nil)
}

func parseAuthJSONWithBudget(k provider.Kind, text string, glmValidateBudget *int) (authDoc, error) {
	// 先按嵌套格式解。
	var nested struct {
		Auth    authSection    `json:"auth"`
		Account accountSection `json:"account"`
	}
	if err := json.Unmarshal([]byte(text), &nested); err == nil &&
		(strings.TrimSpace(nested.Auth.AccessToken) != "" || strings.TrimSpace(nested.Auth.RefreshToken) != "") {
		doc := authDoc{Auth: nested.Auth, Account: nested.Account}
		return normalizeAuthDocWithBudget(k, doc, glmValidateBudget)
	}

	// 再按扁平格式解。
	var flat jsonFlatAuth
	if err := json.Unmarshal([]byte(text), &flat); err != nil {
		return authDoc{}, fmt.Errorf("JSON 解析失败：%w（请确认是嵌套或扁平凭据格式）", err)
	}
	expiresAt := flat.ExpiresAt
	if expiresAt == 0 && flat.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + flat.ExpiresIn
	}
	doc := authDoc{
		Auth: authSection{
			AccessToken:  strings.TrimSpace(flat.AccessToken),
			RefreshToken: strings.TrimSpace(flat.RefreshToken),
			ExpiresAt:    expiresAt,
			Domain:       strings.TrimSpace(flat.Domain),
			ApiHost:      strings.TrimSpace(flat.ApiHost),
		},
		Account: accountSection{
			UID:          strings.TrimSpace(flat.UID),
			EnterpriseID: strings.TrimSpace(flat.EnterpriseID),
			Nickname:     strings.TrimSpace(flat.Nickname),
		},
	}
	return normalizeAuthDocWithBudget(k, doc, glmValidateBudget)
}

// normalizeAuthDoc 按渠道补默认值并做基础校验。
func normalizeAuthDoc(k provider.Kind, doc authDoc) (authDoc, error) {
	return normalizeAuthDocWithBudget(k, doc, nil)
}

func normalizeAuthDocWithBudget(k provider.Kind, doc authDoc, glmValidateBudget *int) (authDoc, error) {
	doc.Auth.AccessToken = strings.TrimSpace(doc.Auth.AccessToken)
	doc.Auth.RefreshToken = strings.TrimSpace(doc.Auth.RefreshToken)
	doc.Auth.Domain = strings.TrimSpace(doc.Auth.Domain)

	if k == provider.GLM {
		// glm 凭据就是 chatglm_refresh_token：access 可空，refresh 必填。
		if doc.Auth.RefreshToken == "" {
			return authDoc{}, fmt.Errorf("glm 渠道的 JSON 里缺少 refreshToken（chatglm_refresh_token）")
		}
		if doc.Auth.Domain == "" {
			doc.Auth.Domain = "chatglm.cn"
		}
		// uid 缺失时走上游验证取回（顺带验证凭据有效性）。
		if strings.TrimSpace(doc.Account.UID) == "" {
			if glmValidateBudget != nil {
				if *glmValidateBudget <= 0 {
					return authDoc{}, fmt.Errorf("glm 凭据缺少 uid，且本次批量已用满 %d 次上游验证；请在凭据里补上 uid 或分批导入（同类条目不再打上游）", jsonBatchValidateLimit)
				}
				*glmValidateBudget--
			}
			r, err := loginglm.Validate(doc.Auth.RefreshToken)
			if err != nil {
				return authDoc{}, fmt.Errorf("glm 凭据验证失败：%w", err)
			}
			doc.Account.UID = r.UID
			doc.Account.Nickname = r.Nickname
			doc.Auth.AccessToken = r.AccessToken
			doc.Auth.ExpiresAt = r.ExpiresAt
		}
		return doc, nil
	}

	if doc.Auth.AccessToken == "" && doc.Auth.RefreshToken == "" {
		return authDoc{}, fmt.Errorf("JSON 里既没有 accessToken 也没有 refreshToken")
	}
	// uid 缺失时从 JWT claims 推导（workbuddy 系 access_token 是 JWT）。
	if strings.TrimSpace(doc.Account.UID) == "" {
		for _, claim := range []string{"uid", "name", "sub"} {
			if v := strings.TrimSpace(jwtClaim(doc.Auth.AccessToken, claim)); v != "" {
				doc.Account.UID = v
				break
			}
		}
	}
	return doc, nil
}
