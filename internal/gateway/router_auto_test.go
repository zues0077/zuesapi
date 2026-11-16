package gateway

import (
	"testing"

	"workbuddy-api/internal/provider"
)

func autoHits(hits ...provider.RouteHit) RouteLookup {
	return func(string) []provider.RouteHit { return hits }
}

// TestResolveAutoPrefersFreeThenLowRate 自动选渠的排序：
// 已知倍率中免费/低费率优先；未知倍率（Explicit=false）排在已知之后。
func TestResolveAutoPrefersFreeThenLowRate(t *testing.T) {
	rt := &Router{
		Default:  "auto",
		Channels: []string{"workbuddy", "glm", "qwenwork"},
		Lookup: autoHits(
			provider.RouteHit{Channel: "workbuddy", Rate: 1.2, Explicit: true},
			provider.RouteHit{Channel: "glm", Rate: 0, Explicit: true},
			provider.RouteHit{Channel: "qwenwork", Rate: 0.6, Explicit: true},
		),
	}
	got, err := rt.Resolve("glm-5.2")
	if err != nil {
		t.Fatalf("Resolve 报错：%v", err)
	}
	if want := "glm/glm-5.2"; got != want {
		t.Fatalf("got=%q want=%q（免费渠道应排第一）", got, want)
	}
}

func TestResolveAutoUnknownRateLast(t *testing.T) {
	rt := &Router{
		Default:  "auto",
		Channels: []string{"a", "b", "c"},
		Lookup: autoHits(
			provider.RouteHit{Channel: "a", Rate: 0, Explicit: false}, // 未知，不应冒充免费
			provider.RouteHit{Channel: "b", Rate: 2.0, Explicit: true},
			provider.RouteHit{Channel: "c", Rate: 0.6, Explicit: true},
		),
	}
	got, err := rt.Resolve("m")
	if err != nil {
		t.Fatalf("Resolve 报错：%v", err)
	}
	if want := "c/m"; got != want {
		t.Fatalf("got=%q want=%q（未知倍率不得排在已知低费率之前）", got, want)
	}
}

func TestResolveAutoFiltersUnknownChannels(t *testing.T) {
	rt := &Router{
		Default:  "auto",
		Channels: []string{"workbuddy"},
		Lookup: autoHits(
			provider.RouteHit{Channel: "ghost", Rate: 0, Explicit: true},
			provider.RouteHit{Channel: "workbuddy", Rate: 1.0, Explicit: true},
		),
	}
	got, err := rt.Resolve("m")
	if err != nil {
		t.Fatalf("Resolve 报错：%v", err)
	}
	if want := "workbuddy/m"; got != want {
		t.Fatalf("got=%q want=%q（未接入渠道应被过滤）", got, want)
	}
}

func TestResolveAutoNoHitsErrors(t *testing.T) {
	rt := &Router{Default: "auto", Channels: []string{"workbuddy"}, Lookup: autoHits()}
	if _, err := rt.Resolve("missing-model"); err == nil {
		t.Fatal("无候选应报错，不能静默落到某个渠道")
	}
}

func TestResolveAutoKeepsSuffix(t *testing.T) {
	rt := &Router{
		Default:  "auto",
		Channels: []string{"glm"},
		Lookup:   autoHits(provider.RouteHit{Channel: "glm", Rate: 0, Explicit: true}),
	}
	got, err := rt.Resolve("glm-5.2 (0.06x)")
	if err != nil {
		t.Fatalf("Resolve 报错：%v", err)
	}
	if got != "glm/glm-5.2 (0.06x)" {
		t.Fatalf("got=%q，倍率后缀应保留", got)
	}
}
