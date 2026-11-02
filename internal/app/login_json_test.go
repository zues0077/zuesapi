package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-api/internal/auth"
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

// TestSubmitJSONAuth_RoundTripsThroughLoader 对**每个**支持 JSON 登录的渠道断言：
// 导入写完的凭据文件，必须能被该渠道自己的加载器读回同一个 uid。
//
// 这是本文件唯一有效的护栏。旧测试只断言 workbuddy 的文件名，而 workbuddy 恰好
// 「Kind == 文件前缀」，于是 TraeWork（Kind=traework、前缀=trae-）被写成
// traework-<uid>.json 也没人发现 —— 接口返回成功、reloadAccounts 又按 glob 扫不回
// 来，用户侧表现为「JSON 导入成功但面板不显示该账号」。逐渠道往返能锁死这一整类
// 前缀不一致（新增渠道时若再出现 Kind ≠ 前缀，这里必红）。
func TestSubmitJSONAuth_RoundTripsThroughLoader(t *testing.T) {
	cases := []struct {
		kind   provider.Kind
		loader func(string) ([]*auth.Auth, error)
		json   string
	}{
		{provider.WorkBuddy, func(d string) ([]*auth.Auth, error) { return auth.LoadWorkBuddyDir(d, "cn") },
			`{"auth":{"accessToken":"a1","refreshToken":"r1"},"account":{"uid":"u-wb"}}`},
		{provider.WorkBuddyAI, auth.LoadWorkBuddyAiDir,
			`{"auth":{"accessToken":"a2","refreshToken":"r2","domain":"www.workbuddy.ai"},"account":{"uid":"u-wbai"}}`},
		{provider.TraeWork, auth.LoadTraeDir,
			`{"auth":{"accessToken":"a3","refreshToken":"r3"},"account":{"uid":"u-trae"}}`},
		{provider.Qoder, auth.LoadQoderDir,
			`{"auth":{"accessToken":"a4","refreshToken":"r4"},"account":{"uid":"u-qd"}}`},
		{provider.QoderCN, auth.LoadQoderCNDir,
			`{"auth":{"accessToken":"a5","refreshToken":"r5"},"account":{"uid":"u-qcn"}}`},
		{provider.QoderCOM, auth.LoadQoderCOMDir,
			`{"auth":{"accessToken":"a6","refreshToken":"r6"},"account":{"uid":"u-qcm"}}`},
		{provider.QwenWork, auth.LoadQwenWorkDir,
			`{"auth":{"accessToken":"a7","refreshToken":"r7"},"account":{"uid":"u-qw"}}`},
		{provider.MonkeyCode, auth.LoadMonkeyCodeDir,
			`{"auth":{"accessToken":"oma-x","refreshToken":"r8"},"account":{"uid":"u-mc"}}`},
		{provider.Raccoon, auth.LoadRaccoonDir,
			`{"auth":{"accessToken":"a9","refreshToken":"r9"},"account":{"uid":"u-rac"}}`},
		{provider.Loomy, auth.LoadLoomyDir,
			`{"auth":{"accessToken":"a10","refreshToken":"r10"},"account":{"uid":"u-loomy"}}`},
		{provider.GLM, auth.LoadGLMDir,
			`{"auth":{"accessToken":"a11","refreshToken":"glm-r11"},"account":{"uid":"u-glm"}}`},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if !jsonLoginChannel[tc.kind] {
				t.Fatalf("渠道 %s 未列入 jsonLoginChannel 白名单，测试与实现已脱节", tc.kind)
			}
			a := newJSONLoginTestApp(t) // 每渠道独立目录，避免 glob 互相命中
			res, err := a.SubmitJSONAuth(tc.kind.String(), tc.json)
			if err != nil {
				t.Fatalf("SubmitJSONAuth: %v", err)
			}
			if res.Channel != tc.kind.String() {
				t.Fatalf("返回的 channel 应是渠道名 %q，实得 %q", tc.kind, res.Channel)
			}
			got, err := tc.loader(a.cfg.AuthDir)
			if err != nil {
				t.Fatalf("加载器报错: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("导入后该渠道加载器读到 %d 个账号（应为 1）—— 落盘前缀与加载器 glob 不一致，"+
					"面板会「导入成功但不显示」", len(got))
			}
			if got[0].UID != res.UID {
				t.Fatalf("uid 不符：文件里 %q，导入返回 %q", got[0].UID, res.UID)
			}
		})
	}
}

// TestSubmitJSONAuth_TraeWorkWritesTraePrefix 是用户实测缺陷的定点回归：
// TraeWork 的凭据必须落成 trae-<uid>.json（与浏览器登录 internal/login_trae.SaveAuth
// 一致），不能是 traework-<uid>.json。
func TestSubmitJSONAuth_TraeWorkWritesTraePrefix(t *testing.T) {
	a := newJSONLoginTestApp(t)
	res, err := a.SubmitJSONAuth("traework",
		`{"auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"4197274137669852"}}`)
	if err != nil {
		t.Fatalf("SubmitJSONAuth: %v", err)
	}
	if res.File != "trae-4197274137669852.json" {
		t.Fatalf("凭据文件名不对：%q（traework- 前缀没有任何加载器会读，账号不会出现在面板）", res.File)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.AuthDir, "traework-4197274137669852.json")); err == nil {
		t.Fatal("仍写出了孤儿 traework-*.json")
	}
	// traecode 与 traework 共账号，也应落到同一个 trae- 前缀下。
	if _, err := a.SubmitJSONAuth("traecode",
		`{"auth":{"accessToken":"a2","refreshToken":"r2"},"account":{"uid":"u-solo"}}`); err != nil {
		t.Fatalf("traecode 导入: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.AuthDir, "trae-u-solo.json")); err != nil {
		t.Fatalf("traecode 未落到 trae- 前缀: %v", err)
	}
}

// TestExportAccountJSON_PreservesAllCredentialFields 断言「导入 → 导出」往返不丢字段。
//
// 导出侧曾手工拼 map，漏掉 machineId/deviceId（以及 qoder 的 machineToken/
// machineType、MonkeyCode 的三把 secret）。对 TraeWork 而言这不是可有可无的字段：
// 签到与对话头都从 auth 文件读 x-device-id / X-Machine-Id，迁移一次就丢设备身份，
// 按 R37 实测正是 20401 / 4017 风控诱因。
func TestExportAccountJSON_PreservesAllCredentialFields(t *testing.T) {
	a := newJSONLoginTestApp(t)
	const uid = "u-full"
	if _, err := a.SubmitJSONAuth("traework", `{
	  "auth":{"accessToken":"a","refreshToken":"r","apiHost":"https://api.trae.com.cn",
	          "machineId":"m-1","deviceId":"d-1"},
	  "account":{"uid":"`+uid+`","nickname":"小李","enterpriseId":"e-1"}}`); err != nil {
		t.Fatalf("SubmitJSONAuth: %v", err)
	}
	doc, err := a.ExportAccountJSON(uid)
	if err != nil {
		t.Fatalf("ExportAccountJSON: %v", err)
	}
	sec, _ := doc["auth"].(map[string]any)
	if sec == nil {
		t.Fatalf("导出文档不是嵌套形: %+v", doc)
	}
	for key, want := range map[string]string{
		"accessToken": "a", "refreshToken": "r", "apiHost": "https://api.trae.com.cn",
		"machineId": "m-1", "deviceId": "d-1",
	} {
		if got, _ := sec[key].(string); got != want {
			t.Errorf("导出丢失/篡改 auth.%s：%q，应为 %q", key, got, want)
		}
	}
	acct, _ := doc["account"].(map[string]any)
	for key, want := range map[string]string{
		"uid": uid, "nickname": "小李", "enterpriseId": "e-1",
	} {
		if got, _ := acct[key].(string); got != want {
			t.Errorf("导出丢失 account.%s：%q，应为 %q", key, got, want)
		}
	}
	// 再导回第三个实例：字段必须还在（证明导出结果可直接用于迁移）。
	b := newJSONLoginTestApp(t)
	raw, _ := json.Marshal(doc)
	if _, err := b.SubmitJSONAuth("traework", string(raw)); err != nil {
		t.Fatalf("二次导入失败: %v", err)
	}
	got, err := auth.LoadTraeDir(b.cfg.AuthDir)
	if err != nil || len(got) != 1 {
		t.Fatalf("二次导入后加载失败: n=%d err=%v", len(got), err)
	}
	if got[0].DeviceID != "d-1" || got[0].MachineID != "m-1" {
		t.Fatalf("往返后设备身份丢失: machineId=%q deviceId=%q", got[0].MachineID, got[0].DeviceID)
	}
}

// TestExportAccountJSON_OmitsEmptyOptionalFields 保持导出 JSON 精简（旧版兼容）。
func TestExportAccountJSON_OmitsEmptyOptionalFields(t *testing.T) {
	a := newJSONLoginTestApp(t)
	if _, err := a.SubmitJSONAuth("workbuddy",
		`{"auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u-min"}}`); err != nil {
		t.Fatalf("SubmitJSONAuth: %v", err)
	}
	doc, err := a.ExportAccountJSON("u-min")
	if err != nil {
		t.Fatalf("ExportAccountJSON: %v", err)
	}
	sec, _ := doc["auth"].(map[string]any)
	for _, key := range []string{"apiHost", "machineId", "deviceId", "signingSecret"} {
		if _, ok := sec[key]; ok {
			t.Errorf("空字段 %q 不应出现在导出 JSON 里", key)
		}
	}
}
