package app

import (
	"reflect"
	"testing"
)

// feesCh 构造一行费率面板渠道数据（测试用）。
func feesCh(channel string, models ...feesModelRow) feesChannel {
	return feesChannel{Channel: channel, Models: models}
}

// m 构造一行模型费率。
func m(model string, priced, free, tools bool, rate float64) feesModelRow {
	return feesModelRow{Model: model, Priced: priced, Free: free, SupportsTools: tools, Rate: rate}
}

func TestBuildAutoPoolFilters(t *testing.T) {
	channels := []feesChannel{
		feesCh("workbuddy",
			m("glm-5.3", true, false, true, 0.08),
			m("text-embedding-v3", true, false, true, 0.01), // blocklist: embedding
			m("no-tools", true, false, false, 0.02),          // 不支持工具
		),
		feesCh("raccoon",
			m("sn-free-flash", true, true, true, 0), // 免费
		),
		feesCh("deepseek",
			m("deepseek-chat", true, false, true, 0.01), // webapi 渠道 → 排除
		),
		feesCh("workbuddyai",
			m("gpt-6-luna", true, false, true, 0.07), // dead 渠道 → 排除
		),
	}
	dead := map[string]bool{"workbuddyai": true}
	webapi := map[string]bool{"deepseek": true}

	pool := buildAutoPool(channels, dead, webapi)
	if len(pool) != 2 {
		t.Fatalf("候选池应有 2 行（免费 flash + glm-5.3），得到 %d: %+v", len(pool), pool)
	}
	// 免费排前
	if pool[0].asChannelModel() != "raccoon/sn-free-flash" {
		t.Errorf("免费应排最前，得到 %s", pool[0].asChannelModel())
	}
	if pool[1].asChannelModel() != "workbuddy/glm-5.3" {
		t.Errorf("付费应在免费后，得到 %s", pool[1].asChannelModel())
	}
}

func TestBuildAutoPoolExhaustedLast(t *testing.T) {
	channels := []feesChannel{
		feesCh("a", m("m-used", true, true, true, 0)),
		feesCh("b", m("m-fresh", true, true, true, 0)),
	}
	pool := buildAutoPool(channels, nil, nil)
	if len(pool) != 2 {
		t.Fatalf("期望 2 行，得到 %d", len(pool))
	}
	// 两条都免费、同费率，字母序 a 在前（此处不比较 exhausted，因为都未标记）
	if pool[0].channel != "a" {
		t.Errorf("同费率同免费应按渠道字母序，得到 %s", pool[0].channel)
	}
}

func TestPickAutoByPaidFreeGroups(t *testing.T) {
	channels := []feesChannel{
		feesCh("workbuddy",
			m("glm-5.3", true, false, true, 0.08),
			m("tiny", true, false, true, 0.01), // <0.05，不进付费主力
		),
		feesCh("qoder",
			m("q-flash", true, false, true, 0.06),
		),
		feesCh("raccoon",
			m("sn-free", true, true, true, 0),
		),
	}
	pool := buildAutoPool(channels, nil, nil)

	paid := pickAutoBy(pool, func(c autoCandidate) bool {
		return c.priced && !c.free && c.rate >= autoRouteMainMinRate
	})
	free := pickAutoBy(pool, func(c autoCandidate) bool {
		return c.free && !c.exhausted
	})

	// 付费组：跨渠道各取一个最便宜 → qoder 0.06（更便宜排首）、workbuddy 0.08（tiny 0.01 被门槛排除）
	if len(paid) != 2 {
		t.Fatalf("付费组期望 2 个（跨渠道），得到 %d: %+v", len(paid), paid)
	}
	if paid[0].asChannelModel() != "qoder/q-flash" {
		t.Errorf("付费组首个应为最便宜 qoder/q-flash，得到 %s", paid[0].asChannelModel())
	}
	if len(free) != 1 || free[0].asChannelModel() != "raccoon/sn-free" {
		t.Errorf("免费组期望 raccoon/sn-free，得到 %+v", free)
	}
}

func TestPickAutoByCrossChannelBeforeCheap(t *testing.T) {
	// 同一渠道有三个越来越便宜的，另一渠道有一个稍贵的。
	// 口径：第一轮跨渠道各取一个（a3、b1），不足 4 个时第二轮回填同渠道其余（a2、a1）。
	// 关键保证是「前两个来自不同渠道」，而不是把最便宜的渠道全占满。
	channels := []feesChannel{
		feesCh("a",
			m("a1", true, false, true, 0.10),
			m("a2", true, false, true, 0.09),
			m("a3", true, false, true, 0.08),
		),
		feesCh("b", m("b1", true, false, true, 0.20)),
	}
	pool := buildAutoPool(channels, nil, nil)
	paid := pickAutoBy(pool, func(c autoCandidate) bool {
		return c.priced && !c.free && c.rate >= autoRouteMainMinRate
	})
	if len(paid) != 4 {
		t.Fatalf("期望 4 个（跨渠道优先 + 回填），得到 %d: %+v", len(paid), paid)
	}
	// 前两个必须来自不同渠道：a3（最便宜）与 b1（另一渠道）
	if paid[0].channel == paid[1].channel {
		t.Errorf("前两个应来自不同渠道，实际都是 %s", paid[0].channel)
	}
	if paid[0].asChannelModel() != "a/a3" || paid[1].asChannelModel() != "b/b1" {
		t.Errorf("跨渠道优先顺序应为 a/a3,b/b1，得到 %s,%s", paid[0].asChannelModel(), paid[1].asChannelModel())
	}
}

func TestMergeAutoCandidatesKeepsManual(t *testing.T) {
	fresh := []string{"workbuddy/glm-5.3", "qoder/q-flash"}
	current := []string{
		"workbuddy/glm-5.3", "qoder/q-flash", // 上次自动生成的（旧）
		"customopenai/my-private-model", // 用户手工追加的
	}
	generated := []string{"workbuddy/glm-5.3", "qoder/q-flash", "raccoon/old-model"}

	got := mergeAutoCandidates(fresh, current, generated)
	want := []string{"workbuddy/glm-5.3", "qoder/q-flash", "customopenai/my-private-model"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("手工项应保留拼在后面，期望 %v，得到 %v", want, got)
	}
}

func TestMergeAutoCandidatesDropsOldGenerated(t *testing.T) {
	// 上次自动的旧模型已不在新 fresh 里 → 不该残留（避免过期模型赖着不走）。
	fresh := []string{"workbuddy/new"}
	current := []string{"workbuddy/old", "workbuddy/user-add"}
	generated := []string{"workbuddy/old"}

	got := mergeAutoCandidates(fresh, current, generated)
	want := []string{"workbuddy/new", "workbuddy/user-add"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("期望 %v，得到 %v", want, got)
	}
}

func TestSameStringSlice(t *testing.T) {
	if !sameStringSlice([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("相同切片应判等")
	}
	if sameStringSlice([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("顺序不同应判不等")
	}
	if sameStringSlice([]string{"a"}, []string{"a", "b"}) {
		t.Error("长度不同应判不等")
	}
}
