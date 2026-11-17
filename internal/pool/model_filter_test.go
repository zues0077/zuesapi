package pool

import (
	"testing"

	"workbuddy-api/internal/auth"
)

// TestPickForModelRespectsAccountModelSet 自定义渠道的虚拟账号会声明各自模型集，
// 选号必须按模型过滤：否则 A 的模型会被发给没有该模型的上游 B（实测静默走错）。
func TestPickForModelRespectsAccountModelSet(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", Models: []string{"only-a"}})
	p.Add(&auth.Auth{UID: "b", Models: []string{"only-b"}})

	if got := p.PickForModel("only-b"); got == nil || got.UID != "b" {
		t.Fatalf("only-b 应由 b 服务，got=%v", got)
	}
	if got := p.PickForModel("only-a"); got == nil || got.UID != "a" {
		t.Fatalf("only-a 应由 a 服务，got=%v", got)
	}
	// 两者都不声明 → 无可用账号
	if got := p.PickForModel("unknown-model"); got != nil {
		t.Fatalf("未声明模型不应被任何账号接单，got=%v", got.UID)
	}
	// 两个账号都声明的模型：正常选号（不因过滤而变空）
	p.Add(&auth.Auth{UID: "c", Models: []string{"common"}})
	p.Add(&auth.Auth{UID: "d", Models: []string{"common"}})
	if got := p.PickForModel("common"); got == nil {
		t.Fatal("common 应有可用账号")
	}
}

// TestPickForModelWithoutModelSetUnchanged 内置渠道账号 Models 为空 ⇒ 不按模型过滤，
// 行为必须与改造前完全一致（任何模型都能被选到）。
func TestPickForModelWithoutModelSetUnchanged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "x"})
	if got := p.PickForModel("whatever-model"); got == nil || got.UID != "x" {
		t.Fatalf("Models 为空的账号应能接任何模型，got=%v", got)
	}
}

// TestPickForModelEmptyModelIgnoresModelSet model=="" 表示不按模型过滤（如刷新积分/保活）。
func TestPickForModelEmptyModelIgnoresModelSet(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", Models: []string{"m1"}})
	if got := p.PickForModel(""); got == nil || got.UID != "a" {
		t.Fatalf("空模型名不应被模型集过滤挡住，got=%v", got)
	}
}
