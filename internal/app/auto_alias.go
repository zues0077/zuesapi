// auto_alias.go 自动优选别名的费率跟随重算。
//
// 背景：面板「其他客户端 → 自动优选生成」会创建两个多候选别名（付费主力 + 全免费），
// 它们的候选是按当时的费率从实时清单里挑出来的「快照」。上游模型上下线、费率变动后，
// 快照会过期（挑中的模型可能下线/变贵/账号全故障）。本文件让这些别名随费率刷新自动更新。
//
// 设计要点（对照前端 app.js 的 routePool/pickCheapCandidates/pickFreeCandidates，口径必须一致）：
//   - 只重算 cfg.Compat.AutoAliases 里登记过的别名；没登记的一个都不碰（零回归）。
//   - 每个别名记 Group（paid/free）与 Generated（上次系统挑出的候选）。
//     重算时保留 Generated 之外的候选项——那是用户手工追加的，系统不替他决定。
//   - 触发点挂在 RefreshPricing 成功后（费率刚变），并受 pricingBusy 串行化保护。
//   - 挑候选复用 FeesInfo 同源的 buildFeesChannels，筛选/排序口径与前端逐条对齐。
package app

import (
	"log"
	"regexp"
	"sort"
	"strings"

	"workbuddy-api/internal/config"
	"workbuddy-api/internal/provider"
)

// autoRouteBlocklist 与前端 app.js 的 ROUTE_BLOCKLIST 保持一致：
// 明显不是对话模型的条目，即使上游标了 supports_tools 也不能进优选池。
var autoRouteBlocklist = []*regexp.Regexp{
	regexp.MustCompile(`(^|[_-])advisor([_-]|$)`),
	regexp.MustCompile(`computer[_-]?use`),
	regexp.MustCompile(`embedding`),
	regexp.MustCompile(`rerank`),
	regexp.MustCompile(`(^|[_-])tts([_-]|$)`),
	regexp.MustCompile(`whisper`),
	regexp.MustCompile(`(^|[_-])image([_-]|$)`),
	regexp.MustCompile(`(^|[_-])video([_-]|$)`),
	regexp.MustCompile(`(^|[_-])ocr([_-]|$)`),
}

// autoRouteMainMinRate 与前端 ROUTE_MAIN_MIN_RATE 一致：付费主力最低倍率 0.05x。
const autoRouteMainMinRate = 0.05

// autoRouteCount 与前端 ROUTE_CUSTOM_COUNT 一致：每组最多挑 4 个候选。
const autoRouteCount = 4

// autoCandidate 优选池里的一行候选（前端 routePool 的一行）。
type autoCandidate struct {
	channel   string
	model     string
	rate      float64
	priced    bool
	free      bool
	exhausted bool
}

// asChannelModel 转成面板/路由表通用的 "channel/model" 形式。
func (c autoCandidate) asChannelModel() string { return c.channel + "/" + c.model }

// recomputeAutoAliases 在费率刷新成功后调用：重算所有登记的自动别名。
// 无自动别名时零开销返回；挑不到候选的组保持原样，绝不清空用户配置。
func (a *App) recomputeAutoAliases() {
	a.mu.Lock()
	specs := a.cfg.Compat.AutoAliases
	if len(specs) == 0 {
		a.mu.Unlock()
		return
	}
	// 拷贝一份，避免持锁做网络/统计查询。
	specCopy := make(map[string]config.AutoAliasSpec, len(specs))
	for k, v := range specs {
		specCopy[k] = v
	}
	a.mu.Unlock()

	pool := a.autoRoutePool()
	paid := pickAutoBy(pool, func(c autoCandidate) bool {
		return c.priced && !c.free && c.rate >= autoRouteMainMinRate
	})
	free := pickAutoBy(pool, func(c autoCandidate) bool {
		return c.free && !c.exhausted
	})

	changed := false
	a.mu.Lock()
	nextModels := make(map[string][]string, len(a.cfg.Compat.CustomModels))
	for k, v := range a.cfg.Compat.CustomModels {
		nextModels[k] = append([]string(nil), v...)
	}
	nextSpecs := make(map[string]config.AutoAliasSpec, len(specCopy))
	for name, spec := range specCopy {
		nextSpecs[name] = spec
		var fresh []autoCandidate
		switch spec.Group {
		case "paid":
			fresh = paid
		case "free":
			fresh = free
		default:
			// group 非法（旧配置/手改）：原样保留，不重算。
			continue
		}
		// 挑不到就保持原样——上游临时全挂时不能把用户配置清空。
		if len(fresh) == 0 {
			continue
		}
		freshCM := make([]string, 0, len(fresh))
		for _, c := range fresh {
			freshCM = append(freshCM, c.asChannelModel())
		}
		merged := mergeAutoCandidates(freshCM, nextModels[name], spec.Generated)
		if !sameStringSlice(nextModels[name], merged) {
			changed = true
		}
		nextModels[name] = merged
		ns := nextSpecs[name]
		ns.Generated = freshCM
		nextSpecs[name] = ns
	}
	specsChanged := !sameSpecGenerated(specCopy, nextSpecs)
	if changed || specsChanged {
		a.cfg.Compat.CustomModels = nextModels
		a.cfg.Compat.AutoAliases = nextSpecs
		if err := config.Save(a.cfg, a.cfgPath); err != nil {
			log.Printf("auto_alias: 保存配置失败: %v", err)
		} else {
			log.Printf("auto_alias: 费率变化后已重算 %d 个自动别名（候选变化=%v）", len(nextSpecs), changed)
		}
	}
	defCh, cap, mm := a.cfg.Compat.DefaultChannel, a.cfg.Compat.MaxTokensCap, a.cfg.Compat.ModelMap
	a.mu.Unlock()

	if changed && a.compatSyncer != nil {
		a.compatSyncer(defCh, cap, mm, nextModels, nextSpecs)
	}
}

// autoRoutePool 复刻前端 routePool：从费率清单 + 账号状态构造候选池并排序。
func (a *App) autoRoutePool() []autoCandidate {
	// 与 FeesInfo 同源：渠道模型列表 + 费率缓存。
	a.pricingMu.Lock()
	cached := append([]provider.ModelPricing(nil), a.pricingCache...)
	a.pricingMu.Unlock()
	if len(cached) == 0 {
		return nil
	}
	var modelsByKind map[provider.Kind][]provider.ModelInfo
	if a.handler != nil {
		modelsByKind = a.handler.CachedChannelModels()
	}

	dead := a.autoDeadChannels()
	webapi := a.autoWebapiChannels()

	return buildAutoPool(buildFeesChannels(modelsByKind, cached, feeChannelOrder), dead, webapi)
}

// buildAutoPool 把费率面板的渠道行转成优选候选池并排序（纯函数，便于单测）。
// 筛选与排序口径与前端 app.js 的 routePool() 逐条对齐。
func buildAutoPool(channels []feesChannel, dead, webapi map[string]bool) []autoCandidate {
	out := make([]autoCandidate, 0)
	for _, ch := range channels {
		if dead[ch.Channel] || webapi[ch.Channel] {
			continue
		}
		for _, m := range ch.Models {
			if !m.SupportsTools || blockedAutoModel(m.Model) {
				continue
			}
			out = append(out, autoCandidate{
				channel: ch.Channel, model: m.Model, rate: m.Rate,
				priced: m.Priced, free: m.Free, exhausted: m.Exhausted,
			})
		}
	}
	// 未被限流优先 → 免费优先 → 已定价优先 → 倍率升序 → 渠道名字母序。
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.exhausted != y.exhausted {
			return !x.exhausted
		}
		if x.free != y.free {
			return x.free
		}
		if x.priced != y.priced {
			return x.priced
		}
		if x.rate != y.rate {
			return x.rate < y.rate
		}
		return x.channel < y.channel
	})
	return out
}

// pickAutoBy 复刻前端 pickCandidatesBy：先跨渠道各取一个最便宜的，不够再回头补第二个，最多 4 个。
func pickAutoBy(pool []autoCandidate, filter func(autoCandidate) bool) []autoCandidate {
	out := make([]autoCandidate, 0, autoRouteCount)
	usedCh := make(map[string]bool)
	for _, c := range pool {
		if !filter(c) || usedCh[c.channel] {
			continue
		}
		usedCh[c.channel] = true
		out = append(out, c)
		if len(out) >= autoRouteCount {
			return out
		}
	}
	if len(out) < autoRouteCount {
		for _, c := range pool {
			if !filter(c) {
				continue
			}
			dup := false
			for _, picked := range out {
				if picked.channel == c.channel && picked.model == c.model {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			out = append(out, c)
			if len(out) >= autoRouteCount {
				break
			}
		}
	}
	return out
}

// autoDeadChannels 找「账号全挂」的渠道，口径同前端 deadChannels：
// 只有该渠道有账号记录且每个都 disabled/cooling/reason/until 非空时才判死。
func (a *App) autoDeadChannels() map[string]bool {
	groups := make(map[string][]autoStatus)
	for _, s := range a.allStatuses() {
		g := a.accountGroup(s.UID)
		groups[g] = append(groups[g], autoStatus{
			disabled: s.Disabled, cooling: s.Cooling, reason: s.Reason, until: !s.Until.IsZero(),
		})
	}
	dead := make(map[string]bool)
	for g, list := range groups {
		if len(list) == 0 {
			continue
		}
		allBad := true
		for _, s := range list {
			if !(s.disabled || s.cooling || strings.TrimSpace(s.reason) != "" || s.until) {
				allBad = false
				break
			}
		}
		if allBad {
			dead[g] = true
		}
	}
	return dead
}

// autoStatus 账号可用性的最小投影。
type autoStatus struct {
	disabled bool
	cooling  bool
	reason   string
	until    bool
}

// autoWebapiChannels 找 webapi 强度不足的渠道，口径同前端 webapiChannels：
// customopenai + tag=webapi 的自定义渠道名。
func (a *App) autoWebapiChannels() map[string]bool {
	out := map[string]bool{"customopenai": true}
	a.mu.Lock()
	ccs := a.cfg.CustomChannels
	a.mu.Unlock()
	for _, cc := range ccs {
		if strings.Contains(strings.ToLower(cc.Tag), "webapi") && cc.Name != "" {
			out[cc.Name] = true
		}
	}
	return out
}

// mergeAutoCandidates 把新挑的候选（主力，排前面）与用户手工追加项拼起来。
// current 里不在 generated 里的就是手工项——系统重算只替换自己上次生成的那部分。
func mergeAutoCandidates(fresh, current, generated []string) []string {
	gen := make(map[string]bool, len(generated))
	for _, g := range generated {
		gen[g] = true
	}
	out := append([]string(nil), fresh...)
	seen := make(map[string]bool, len(out))
	for _, c := range out {
		seen[c] = true
	}
	for _, cm := range current {
		if gen[cm] || seen[cm] {
			continue // 上次自动生成的（将被 fresh 取代）或已重复
		}
		seen[cm] = true
		out = append(out, cm)
	}
	return out
}

func blockedAutoModel(name string) bool {
	for _, re := range autoRouteBlocklist {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameSpecGenerated(a, b map[string]config.AutoAliasSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va.Group != vb.Group || !sameStringSlice(va.Generated, vb.Generated) {
			return false
		}
	}
	return true
}
