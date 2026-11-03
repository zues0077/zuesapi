package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePublicURL(t *testing.T) {
	ok := map[string]string{
		"":                              "",
		"   ":                           "",
		"https://nas.example.com:8443":  "https://nas.example.com:8443",
		"  http://192.168.1.5:7863/  ":  "http://192.168.1.5:7863",
		"https://a.b/v1":                "https://a.b/v1",
		"https://a.b/sub/":              "https://a.b/sub",
	}
	for in, want := range ok {
		got, err := NormalizePublicURL(in)
		if err != nil {
			t.Errorf("NormalizePublicURL(%q) 意外报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizePublicURL(%q) = %q，应为 %q", in, got, want)
		}
	}
	bad := []string{"nas.example.com:8443", "ftp://a.b", "https://", "https://a.b?x=1", "https://a.b#frag", "//a.b"}
	for _, in := range bad {
		if got, err := NormalizePublicURL(in); err == nil {
			t.Errorf("非法地址 %q 竟然通过校验，返回 %q", in, got)
		}
	}
}

func TestAPIBaseFromPublic(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"https://a.b:8443":             "https://a.b:8443/v1",
		"https://a.b:8443/":            "https://a.b:8443/v1",
		"https://a.b/v1":               "https://a.b/v1", // 已带 /v1 不重复追加
		"https://a.b/gateway":          "https://a.b/gateway/v1",
		"  https://a.b:8443/v1  ":      "https://a.b:8443/v1",
	}
	for in, want := range cases {
		if got := APIBaseFromPublic(in); got != want {
			t.Errorf("APIBaseFromPublic(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// TestLoadSurvivesInvalidPublicURL 钉住一条设计决定：public_url 是纯展示字段，
// 填错**绝不能让服务起不来**。
//
// 反面教材就是 fpk 那个 config.json 事故：一个只影响展示的字段被写坏，
// 结果 LoadOrCreate 返回 error → main.go fatal → 应用中心点「打开」几秒后退回
// 「启用」，用户完全无法自助恢复。展示类字段的错误降级成本地失效，不是全局不可用。
func TestLoadSurvivesInvalidPublicURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"listen":{"host":"0.0.0.0","port":7863},"api_key":"k","public_url":"nas.example.com:8443"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("public_url 非法不应让配置加载失败，实得: %v", err)
	}
	if cfg.PublicURL != "" {
		t.Fatalf("非法 public_url 应被清空回退，实得 %q", cfg.PublicURL)
	}
	if cfg.APIKey != "k" || cfg.Listen.Port != 7863 {
		t.Fatalf("其它字段被牵连: %+v", cfg.Listen)
	}
}

// TestLoadKeepsValidPublicURL 合法值要清洗后保留，并能存回磁盘。
func TestLoadKeepsValidPublicURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"listen":{"host":"0.0.0.0","port":7865},"api_key":"k","public_url":" https://n.b:8443/ "}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicURL != "https://n.b:8443" {
		t.Fatalf("应清洗末尾 / 并保留，实得 %q", cfg.PublicURL)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	again, err := Load(path)
	if err != nil || again.PublicURL != "https://n.b:8443" {
		t.Fatalf("往返后丢失 public_url: %v %+v", err, again)
	}
}

// TestSaveWritesListenAsMultilineObject 钉住一个跨语言事实，供 fpk 打包脚本依赖：
// 程序用 MarshalIndent 整体重写 config.json，**listen 落盘就是多行对象**。
//
// 为什么要在 Go 里测这个：fpk 的 cmd/main 用 sed 按行改 config.json，它必须知道
// 「listen 不是一行」，否则只替换首行会留下孤儿行把 JSON 改坏（v0.1.10 实发事故）。
// 这条断言一旦变红，说明 shell 侧的 patch_top_level / repair_listen_block 需要重审。
// 对应回归：scripts/test-fpk-config.sh。
func TestSaveWritesListenAsMultilineObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()
	cfg.Listen = Listen{Host: "127.0.0.1", Port: 7863}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "\n  \"listen\": {\n") {
		t.Fatalf("listen 应以两格缩进的多行对象落盘（shell 侧按此判断能否改行）：\n%s", s)
	}
	if !strings.Contains(s, "\n    \"host\": ") || !strings.Contains(s, "\n    \"port\": ") {
		t.Fatalf("listen 的子字段应在独立行上：\n%s", s)
	}
}
