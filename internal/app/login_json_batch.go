package app

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/provider"
)

// 本文件实现「JSON 批量导入 / 导出」：一次粘贴/下载多个账号，而不是逐个操作。
//
// 与单条版（login_json.go）的关系：落盘复用同一个 importAuthJSON，因此渠道白名单、
// 前缀映射（authFilePrefix）、字段补全与校验完全一致 —— 不会出现「单条能进、批量
// 进不去」这类两套实现漂移的坑。
//
// 批量的关键差异是**只 reload 一次**：单条路径每条都会 reloadAccounts() +
// afterAccountAdded()（后者会真实打上游拉积分/模型），批量若沿用就是 N 次全渠道
// 重扫 + N 次上游请求。

// jsonBatchLimit 单次批量上限。粘贴几十上百条是正常迁移量；设上限是为了挡住
// 误粘超大文本（每条都要解析 + 落盘，glm 渠道还可能触发一次上游验证）。
const jsonBatchLimit = 200

// jsonBatchValidateLimit 一次批量里最多触发多少次「缺 uid 的 glm 上游验证」。
//
// glm 凭据没带 uid 时要打一次 Validate 才能拿到 uid；200 条都没带就是 200 次串行
// 上游请求（几分钟），期间整个导入请求挂住。超过预算后同类条目直接返回可读错误，
// 让用户补 uid 或分批导入，不给上游无限放大。
const jsonBatchValidateLimit = 20

// JSONBatchFailure 批量导入里失败的一条（带序号，便于用户对着原文定位）。
type JSONBatchFailure struct {
	Index   int    `json:"index"`
	Channel string `json:"channel,omitempty"`
	UID     string `json:"uid,omitempty"`
	Error   string `json:"error"`
}

// JSONBatchResult 批量导入回执：成功与失败分开列，整体不因为个别失败而报错。
type JSONBatchResult struct {
	Total    int                `json:"total"`
	OKCount  int                `json:"ok_count"`
	OK       []JSONLoginResult  `json:"ok"`
	Failed   []JSONBatchFailure `json:"failed"`
	Reloaded bool               `json:"reloaded"`
}

// batchEntry 解析出来的一条：渠道 + 凭据正文（嵌套或扁平，交给 importAuthJSON）。
//
// Err 非空表示「这条在解析阶段就废了」（缺 channel、不是对象等）。它仍然占一个
// 序号，只是不碰账号池 —— 不能因为用户少写一个 channel 就整批拒绝，那会让
// 「粘 30 条错 1 条」变成「30 条全白忙」，也拿不到逐条回执。
type batchEntry struct {
	Channel string
	Payload string
	Err     string
}

// SubmitJSONAuthBatch 批量导入。接受以下几种粘贴形态（自动识别）：
//
//	导出信封：   {"version":"…","accounts":[{"channel":"traework","auth":{…},"account":{…}}]}
//	裸数组：     [{"channel":"workbuddy","auth":{…},"account":{…}}, …]
//	手工批量：   [{"channel":"glm","json":"{\"refreshToken\":\"…\",\"uid\":\"…\"}"}]
//	单条对象：   {"channel":"traework","auth":{…},"account":{…}}
//
// 逐条独立处理：某条失败不中断其余，最后统一 reload 一次、按出现过的渠道各触发
// 一次后置动作。全部失败才返回 error（面板据此提示「一条都没进去」）。
func (a *App) SubmitJSONAuthBatch(text string) (*JSONBatchResult, error) {
	entries, err := parseBatchEntries(text)
	if err != nil {
		return nil, err
	}
	res := &JSONBatchResult{Total: len(entries)}
	seenKind := make(map[provider.Kind]bool, len(entries))
	glmValidateBudget := jsonBatchValidateLimit

	for i, e := range entries {
		if e.Err != "" {
			res.Failed = append(res.Failed, JSONBatchFailure{Index: i, Channel: e.Channel, Error: e.Err})
			continue
		}
		k, kerr := jsonLoginKind(e.Channel)
		if kerr != nil {
			res.Failed = append(res.Failed, JSONBatchFailure{Index: i, Channel: e.Channel, Error: kerr.Error()})
			continue
		}
		uid, file, ierr := a.importAuthJSONWithBudget(k, e.Payload, &glmValidateBudget)
		if ierr != nil {
			res.Failed = append(res.Failed, JSONBatchFailure{Index: i, Channel: string(k), Error: ierr.Error()})
			continue
		}
		res.OK = append(res.OK, JSONLoginResult{
			Channel: string(k), UID: uid, File: filepath.Base(file),
			Note: "批量导入",
		})
		seenKind[k] = true
	}

	res.OKCount = len(res.OK)
	if res.OKCount == 0 {
		return res, fmt.Errorf("批量导入没有一条成功（共 %d 条），见逐条原因", res.Total)
	}
	a.reloadAccounts()
	res.Reloaded = true
	// 后置动作按渠道各一次（内部会刷新积分/模型表），同渠道多条不重复触发。
	for k := range seenKind {
		a.afterAccountAdded(k)
	}
	// 只记条数与渠道，绝不记凭据（不变量 2：日志零 token）。
	log.Printf("json 批量导入 总=%d 成功=%d 失败=%d 渠道=%d", res.Total, res.OKCount, len(res.Failed), len(seenKind))
	return res, nil
}

// parseBatchEntries 把粘贴文本归一成 []batchEntry。
func parseBatchEntries(text string) ([]batchEntry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("批量内容为空")
	}
	var root any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, fmt.Errorf("JSON 解析失败：%v（需要数组、{\"accounts\":[…]} 信封或单条对象）", err)
	}

	items, err := normalizeBatchItems(root)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("批量内容为空数组")
	}
	if len(items) > jsonBatchLimit {
		return nil, fmt.Errorf("单次最多导入 %d 条（本次 %d 条），请分批", jsonBatchLimit, len(items))
	}

	entries := make([]batchEntry, 0, len(items))
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("批量项必须是对象，实际是 %T", raw)
		}
		entries = append(entries, entryFromMap(m))
	}
	return entries, nil
}

// normalizeBatchItems 取出待处理的数组：兼容裸数组、{"accounts":[…]} 信封、单条对象。
func normalizeBatchItems(root any) ([]any, error) {
	switch v := root.(type) {
	case []any:
		return v, nil
	case map[string]any:
		if inner, ok := v["accounts"]; ok {
			arr, ok := inner.([]any)
			if !ok {
				return nil, fmt.Errorf(`"accounts" 应为数组，实际是 %T`, inner)
			}
			return arr, nil
		}
		// 没有 accounts 数组：若本身带 channel，就当成「只有一条」处理。
		if _, ok := v["channel"]; ok {
			return []any{v}, nil
		}
		return nil, fmt.Errorf(`找不到数组：请粘贴账号数组、{"accounts":[…]} 或带 "channel" 的单条对象`)
	default:
		return nil, fmt.Errorf("顶层应为数组或对象，实际是 %T", root)
	}
}

// entryFromMap 从一条对象里抽出 channel + 凭据正文；问题记在 Err 上，不中断整批。
//
// 三种写法都要能用，否则「导出文件直接再导回去」这条最常用的路会失败：
//  1. {channel, auth:{…}, account:{…}}   —— 导出格式，正文 = 去掉 channel 的对象
//  2. {channel, json:"…"}                —— 手工批量，正文 = 那个字符串
//  3. {channel, accessToken:…, uid:…}    —— 扁平凭据混在批量里，正文 = 去掉 channel 的对象
func entryFromMap(m map[string]any) batchEntry {
	ch, _ := m["channel"].(string)
	if strings.TrimSpace(ch) == "" {
		return batchEntry{Err: "批量项缺少 channel（指明 workbuddy/traework/glm 等渠道）"}
	}
	if s, ok := m["json"].(string); ok && strings.TrimSpace(s) != "" {
		return batchEntry{Channel: ch, Payload: s}
	}
	rest := make(map[string]any, len(m))
	for k, v := range m {
		if k == "channel" {
			continue
		}
		rest[k] = v
	}
	if len(rest) == 0 {
		return batchEntry{Channel: ch, Err: fmt.Sprintf("渠道 %s 的这条没有凭据内容", ch)}
	}
	blob, err := json.Marshal(rest)
	if err != nil {
		return batchEntry{Channel: ch, Err: fmt.Sprintf("渠道 %s 的凭据序列化失败：%v", ch, err)}
	}
	return batchEntry{Channel: ch, Payload: string(blob)}
}

// authLoader 渠道 → 凭据加载器。导出全部账号要用；与 reloadAccounts 的渠道集合一致。
type authLoader struct {
	kind provider.Kind
	load func(dir string) ([]*auth.Auth, error)
}

// allAuthLoaders 列出所有「从 auths/ 目录加载」的渠道。
//
// WorkBuddy 的加载器 glob 是 `workbuddy*.json`，会连带命中 `workbuddyai-*.json`，
// 导出**不按 cfg.Region 过滤**，且要避开一条 glob 边界：
//   - `workbuddy*.json` 会连带命中 `workbuddyai-*.json`。WorkBuddy 这一项必须显式
//     排除 workbuddyai- 前缀，否则导出会把国际版账号标成 channel=workbuddy，粘回去
//     就落错文件名、加载器再按 region 一过滤 → 账号凭空消失（比报错难查得多）。
//   - 备份/迁移的语义是「磁盘上有什么就给什么」。按 region 过滤会让一个 cn 实例
//     静默漏掉同目录下的国际版账号，用户拿到的是一份「看起来成功」的残缺备份。
//     所以 cn / global 两种都取，再按文件路径去重。
func allAuthLoaders() []authLoader {
	return []authLoader{
		{provider.WorkBuddy, func(d string) ([]*auth.Auth, error) {
			var out []*auth.Auth
			for _, reg := range []string{"cn", "global"} {
				list, err := auth.LoadWorkBuddyDir(d, reg)
				if err != nil {
					return nil, err
				}
				for _, au := range list {
					if strings.HasPrefix(filepath.Base(au.FilePath), "workbuddyai-") {
						continue // 归 WorkBuddyAI 渠道
					}
					out = append(out, au)
				}
			}
			return out, nil
		}},
		{provider.WorkBuddyAI, auth.LoadWorkBuddyAiDir},
		{provider.TraeWork, auth.LoadTraeDir},
		{provider.Qoder, auth.LoadQoderDir},
		{provider.QoderCN, auth.LoadQoderCNDir},
		{provider.QoderCOM, auth.LoadQoderCOMDir},
		{provider.QwenWork, auth.LoadQwenWorkDir},
		{provider.MonkeyCode, auth.LoadMonkeyCodeDir},
		{provider.Raccoon, auth.LoadRaccoonDir},
		{provider.Loomy, auth.LoadLoomyDir},
		{provider.GLM, auth.LoadGLMDir},
	}
}

// ExportAllAccountsJSON 导出全部账号为一个 JSON（每条带 channel），
// 可直接整份粘回「批量 JSON 导入」—— 导出与导入互为逆操作，不需要手工改格式。
//
// ⚠️ 输出里是**明文凭据**（磁盘上是 DPAPI 密文/容器内明文，导出统一解回明文），
// 等同于一份「所有账号的钥匙」。只在已鉴权的面板里提供，日志只记条数。
func (a *App) ExportAllAccountsJSON() (map[string]any, error) {
	return a.exportAllAccountsJSON(allAuthLoaders())
}

// exportAllAccountsJSON 可用注入的加载器列表调用，便于回归「某渠道加载失败时
// 回执必须带 warnings，而不是静默给出一份残缺备份」。
func (a *App) exportAllAccountsJSON(loaders []authLoader) (map[string]any, error) {
	byChannel := map[string]int{}
	accounts := make([]map[string]any, 0, 16)
	seen := map[string]bool{}
	var errs []string

	for _, l := range loaders {
		auths, err := l.load(a.cfg.AuthDir)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", l.kind, err))
			continue
		}
		for _, au := range auths {
			key := au.FilePath
			if key == "" {
				key = string(l.kind) + "|" + au.UID // 无文件路径（内存账号）时按渠道+uid 去重
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			doc := authToExportDoc(au)
			doc["channel"] = string(l.kind)
			accounts = append(accounts, doc)
			byChannel[string(l.kind)]++
		}
	}
	// 加载器顺序固定，但 map 计数要稳定输出时仍需排序（前端展示用）。
	chans := make([]string, 0, len(byChannel))
	for c := range byChannel {
		chans = append(chans, c)
	}
	sort.Strings(chans)

	if len(accounts) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("导出失败：%s", strings.Join(errs, "; "))
	}
	// 只记条数与渠道分布，绝不记凭据。
	log.Printf("json 批量导出 账号=%d 渠道=%s 警告=%d", len(accounts), strings.Join(chans, ","), len(errs))
	out := map[string]any{
		"version":     Version,
		"exported_at": time.Now().Format(time.RFC3339),
		"count":       len(accounts),
		"accounts":    accounts,
	}
	// 部分渠道加载失败时必须显式告知：否则用户拿到的是一份「看起来成功」的残缺备份，
	// 换机后才发现少了账号，比整份导出失败难查得多。
	if len(errs) > 0 {
		out["warnings"] = errs
	}
	return out, nil
}
