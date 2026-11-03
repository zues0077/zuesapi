package app

import (
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-api/internal/config"
)

// newPublicURLApp 造一个带真实 config.json 的最小 App（SetPublicURL 会写盘）。
func newPublicURLApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 必须从 Default() 起步：cooldown/upstream 里的时长字段是 normalize 要解析的，
	// 手搓零值会让 Load 直接失败（那会让本测试在错误的前提下通过或崩溃）。
	cfg := config.Default()
	cfg.AuthDir = filepath.Join(dir, "auths")
	cfg.StateFile = filepath.Join(dir, "data", "state.json")
	cfg.Listen = config.Listen{Host: "0.0.0.0", Port: 7865}
	cfg.APIKey = "k"
	cfg.AdminPass = "p"
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("准备 config.json: %v", err)
	}
	return &App{cfg: cfg, cfgPath: path}, path
}

// TestSetPublicURL 覆盖：合法值清洗并落盘、非法值当场报错且**不改动原值**、空串清除。
func TestSetPublicURL(t *testing.T) {
	a, path := newPublicURLApp(t)

	if _, err := a.SetPublicURL("  https://nas.example.com:8443/  "); err != nil {
		t.Fatalf("合法地址被拒: %v", err)
	}
	if a.cfg.PublicURL != "https://nas.example.com:8443" {
		t.Fatalf("未清洗末尾斜杠/空白: %q", a.cfg.PublicURL)
	}
	// 必须真的落盘，否则重启就丢（面板设置类字段的通病）。
	reloaded, err := config.Load(path)
	if err != nil || reloaded.PublicURL != "https://nas.example.com:8443" {
		t.Fatalf("未持久化: err=%v value=%q", err, reloaded.PublicURL)
	}

	// 非法：报错，且原值保持不变（不能把坏值写进去）。
	if _, err := a.SetPublicURL("nas.example.com:8443"); err == nil {
		t.Fatal("缺 scheme 的地址应报错")
	} else if !strings.Contains(err.Error(), "外网访问地址") {
		t.Fatalf("报错文案应指明字段，实得: %v", err)
	}
	if a.cfg.PublicURL != "https://nas.example.com:8443" {
		t.Fatalf("非法输入不应改动已生效值，实得 %q", a.cfg.PublicURL)
	}

	// 空串 = 清除，顶栏退回按监听地址推导。
	if _, err := a.SetPublicURL("   "); err != nil {
		t.Fatalf("清除操作报错: %v", err)
	}
	if a.cfg.PublicURL != "" {
		t.Fatalf("应已清除，实得 %q", a.cfg.PublicURL)
	}
}

// TestSetPublicURLDoesNotTouchListen 钉住这个字段的核心边界：它是展示用的，
// 绝不能碰监听 —— 用户正是因为把外网域名填进「监听」才把面板搞成彻底打不开。
func TestSetPublicURLDoesNotTouchListen(t *testing.T) {
	a, _ := newPublicURLApp(t)
	before := a.cfg.Listen
	if _, err := a.SetPublicURL("https://nas.example.com:8443"); err != nil {
		t.Fatalf("SetPublicURL: %v", err)
	}
	if a.cfg.Listen != before {
		t.Fatalf("设置外网访问地址竟改动了监听: %+v → %+v", before, a.cfg.Listen)
	}
}
