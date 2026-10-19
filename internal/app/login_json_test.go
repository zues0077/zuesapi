package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-api/internal/config"
	"workbuddy-api/internal/provider"
)

// newJSONLoginTestApp 构造仅写入凭据所需的最小 App（AuthDir 指向临时目录）。
func newJSONLoginTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{cfg: &config.Config{AuthDir: dir}}
}

func TestParseAuthJSON_Nested(t *testing.T) {
	text := `{
	  "auth": {"accessToken": "acc-1", "refreshToken": "ref-1", "expiresAt": 1900000000, "domain": "copilot.tencent.com"},
	  "account": {"uid": "u-123", "nickname": "张三"}
	}`
	doc, err := parseAuthJSON(provider.WorkBuddy, text)
	if err != nil {
		t.Fatalf("parseAuthJSON: %v", err)
	}
	if doc.Auth.AccessToken != "acc-1" || doc.Auth.RefreshToken != "ref-1" {
		t.Fatalf("auth 字段未对齐: %+v", doc.Auth)
	}
	if doc.Account.UID != "u-123" || doc.Account.Nickname != "张三" {
		t.Fatalf("account 字段未对齐: %+v", doc.Account)
	}
}

func TestParseAuthJSON_Flat(t *testing.T) {
	text := `{"accessToken":"acc-2","refreshToken":"ref-2","expiresIn":3600,"uid":"u-456"}`
	doc, err := parseAuthJSON(provider.WorkBuddy, text)
	if err != nil {
		t.Fatalf("parseAuthJSON: %v", err)
	}
	if doc.Auth.AccessToken != "acc-2" {
		t.Fatalf("accessToken 丢失: %+v", doc.Auth)
	}
	if doc.Auth.ExpiresAt <= 0 {
		t.Fatalf("expiresIn 未换算为 expiresAt: %d", doc.Auth.ExpiresAt)
	}
	if doc.Account.UID != "u-456" {
		t.Fatalf("uid 丢失: %+v", doc.Account)
	}
}

func TestParseAuthJSON_GLMDefaults(t *testing.T) {
	text := `{"refreshToken":"glm-ref-1","uid":"glm-1"}`
	doc, err := parseAuthJSON(provider.GLM, text)
	if err != nil {
		t.Fatalf("parseAuthJSON: %v", err)
	}
	if doc.Auth.Domain != "chatglm.cn" {
		t.Fatalf("glm domain 未补默认值: %q", doc.Auth.Domain)
	}
}

func TestParseAuthJSON_RejectsEmpty(t *testing.T) {
	if _, err := parseAuthJSON(provider.WorkBuddy, `{"account":{"uid":"u-1"}}`); err == nil {
		t.Fatal("无 token 的 JSON 应报错")
	}
	if _, err := parseAuthJSON(provider.WorkBuddy, "not json"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestSubmitJSONAuth_WritesFileAndReloads(t *testing.T) {
	a := newJSONLoginTestApp(t)
	res, err := a.SubmitJSONAuth("workbuddy",
		`{"auth":{"accessToken":"acc-x","refreshToken":"ref-x"},"account":{"uid":"u-x"}}`)
	if err != nil {
		t.Fatalf("SubmitJSONAuth: %v", err)
	}
	if res.Channel != "workbuddy" || res.UID != "u-x" {
		t.Fatalf("结果不对: %+v", res)
	}
	want := filepath.Join(a.cfg.AuthDir, "workbuddy-u-x.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("凭据文件未落盘: %v", err)
	}
	raw, _ := os.ReadFile(want)
	// writeAuthFile 会把 token 加密（Windows DPAPI），明文不应出现在文件里。
	if strings.Contains(string(raw), "acc-x") {
		t.Fatal("access_token 以明文落盘")
	}
	if !strings.Contains(string(raw), `"auth"`) {
		t.Fatal("落盘格式不是嵌套结构")
	}
}

func TestSubmitJSONAuth_RejectsUnknownChannel(t *testing.T) {
	a := newJSONLoginTestApp(t)
	if _, err := a.SubmitJSONAuth("nosuch", `{"accessToken":"a"}`); err == nil {
		t.Fatal("未知渠道应报错")
	}
}
