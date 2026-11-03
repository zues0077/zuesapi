package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

)

// TestProxyClientMapIsSingleSourceOfTruth 钉住 R35 同族的接线缺陷：**启动路径与面板
// 热更新路径必须共用同一份「渠道 → 需要套代理的 client」表**。
//
// 为什么单独测：SetTransportProxy 会**新建** Transport，表里少列一项，那条 client
// 就永远走直连 —— 历史上热更新漏过 monkeycode / raccoon / loomy 三项（启动有、
// 热更新没有），现象是「面板改了这三个渠道的代理毫无反应」，而且删掉代理后旧
// Transport 永久残留（applyProxies 对未配置渠道也要套直连，漏列就连直连都清不掉）。
// 这类「两处各写一份、后来只改一处」的漂移，编译器与单元测试都发现不了，只能静态钉。
func TestProxyClientMapIsSingleSourceOfTruth(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("解析 main.go: %v", err)
	}

	var builder *ast.FuncLit      // proxyClientMap := func() ... {}
	callsites := 0                // applyProxies(...) 调用次数
	inlineMaps := 0               // 直接写在调用点的 map 字面量
	ast.Inspect(f, func(n ast.Node) bool {
		// 找 proxyClientMap 的定义
		if as, ok := n.(*ast.AssignStmt); ok {
			if len(as.Lhs) == 1 {
				if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == "proxyClientMap" {
					if fl, ok := as.Rhs[0].(*ast.FuncLit); ok {
						builder = fl
					}
				}
			}
		}
		// 找 applyProxies 调用
		if ce, ok := n.(*ast.CallExpr); ok {
			if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "applyProxies" && len(ce.Args) == 2 {
				callsites++
				switch a := ce.Args[1].(type) {
				case *ast.CallExpr: // applyProxies(cfg, proxyClientMap())
					if id, ok := a.Fun.(*ast.Ident); !ok || id.Name != "proxyClientMap" {
						t.Errorf("applyProxies 的第二个参数不是 proxyClientMap()，而是别的调用")
					}
				case *ast.CompositeLit: // applyProxies(cfg, map[...]{...}) —— 不允许
					_ = a
					inlineMaps++
				}
			}
		}
		return true
	})

	if builder == nil {
		t.Fatal("main.go 里找不到 proxyClientMap 定义（被改回两处各写一份了？）")
	}
	if callsites != 2 {
		t.Fatalf("applyProxies 应恰好调用 2 次（启动 + 热更新），实得 %d", callsites)
	}
	if inlineMaps != 0 {
		t.Fatalf("有 %d 处把 client 表内联写在 applyProxies 调用点，必须改用 proxyClientMap()", inlineMaps)
	}

	// 表里必须覆盖所有需要套代理的渠道。按 **Go 标识符**匹配（源码里写的是
	// provider.MonkeyCode.String()，不是字面量 "monkeycode"），并先确认切片非空 ——
	// 否则「取不到函数体」会让下面的包含检查全部静默通过，又是一盏不亮的灯。
	body := string(src[fset.Position(builder.Pos()).Offset:fset.Position(builder.End()).Offset])
	if len(body) < 200 {
		t.Fatalf("没取到 proxyClientMap 的函数体（长度 %d），测试无法判定", len(body))
	}
	for _, ident := range []string{
		"WorkBuddy", "WorkBuddyAI", "TraeWork", "Qoder", "QoderCN", "QoderCOM",
		"QwenWork", "MonkeyCode", "Raccoon", "Loomy", "GLM",
	} {
		if !strings.Contains(body, "provider."+ident+".String()") {
			t.Errorf("proxyClientMap 缺渠道 provider.%s —— 该渠道的代理会静默不生效", ident)
		}
	}
	// 每个渠道的 StreamHTTP 也要在（R35：漏了流式就走直连，且删代理时清不掉）。
	if n := strings.Count(body, "StreamHTTP"); n < 11 {
		t.Errorf("proxyClientMap 里 StreamHTTP 只出现 %d 次（应 ≥11）—— 有渠道的流式没套代理", n)
	}
	// BillingHTTP 只能有 workbuddy 一处（别的渠道没有独立计费 client）。
	if n := strings.Count(body, "BillingHTTP"); n != 1 {
		t.Errorf("BillingHTTP 出现 %d 次（应为 1）—— 表被改动了，请核对", n)
	}
}
