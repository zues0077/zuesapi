package upstream

import "testing"

// TestIsFreeBadge 验证免费徽标判定：无条件免费算 free，时段性促销（夜间免费/折扣）不算。
func TestIsFreeBadge(t *testing.T) {
	cases := []struct {
		note string
		want bool
	}{
		{"限时免费", true},
		{"独家限免", true},
		{"限免", true},
		{"免费", true},
		{"夜间免费", false}, // 时段性促销，平时照常计费
		{"夜间折扣", false},
		{"独家优惠", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isFreeBadge(c.note); got != c.want {
			t.Errorf("isFreeBadge(%q)=%v want %v", c.note, got, c.want)
		}
	}
}

// TestParseBadgeFreeVariants 确认 parseBadge 能从上流 tags 里取出免费文案（含无颜色后缀）。
func TestParseBadgeFreeVariants(t *testing.T) {
	label, color := parseBadge([]string{"craft", "badge:限时免费"})
	if label != "限时免费" || color != "" {
		t.Fatalf("got label=%q color=%q", label, color)
	}
	if !isFreeBadge(label) {
		t.Fatalf("限时免费 should be free badge")
	}
}
