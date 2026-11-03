package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/provider"
)

// batchText 把任意结构序列化成「粘贴用」文本。
func batchText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func nested(channel, uid, extra string) map[string]any {
	doc := map[string]any{
		"auth":    map[string]any{"accessToken": "acc-" + uid, "refreshToken": "ref-" + uid},
		"account": map[string]any{"uid": uid, "nickname": "n-" + uid},
	}
	if channel != "" {
		doc["channel"] = channel
	}
	if extra != "" {
		doc["auth"].(map[string]any)["deviceId"] = extra
	}
	return doc
}

// TestSubmitJSONAuthBatch_MixedChannels 一次导入多个渠道，并逐渠道用**自己的加载器**读回。
// 特别钉住 TraeWork：批量路径也必须落 trae-*.json（复用 authFilePrefix，不另写一套）。
func TestSubmitJSONAuthBatch_MixedChannels(t *testing.T) {
	a := newJSONLoginTestApp(t)
	text := batchText(t, []any{
		nested("workbuddy", "u-wb", ""),
		nested("traework", "4197274137669852", ""),
		nested("glm", "u-glm", ""),
	})
	res, err := a.SubmitJSONAuthBatch(text)
	if err != nil {
		t.Fatalf("批量导入报错: %v（失败明细 %+v）", err, res.Failed)
	}
	if res.Total != 3 || res.OKCount != 3 || len(res.Failed) != 0 {
		t.Fatalf("回执不对: total=%d ok=%d failed=%+v", res.Total, res.OKCount, res.Failed)
	}
	if !res.Reloaded {
		t.Fatal("成功导入后应重载账号池")
	}
	if _, err := os.Stat(filepath.Join(a.cfg.AuthDir, "trae-4197274137669852.json")); err != nil {
		t.Fatalf("TraeWork 未落 trae- 前缀: %v", err)
	}
	for _, tc := range []struct {
		kind provider.Kind
		load func(string) ([]*auth.Auth, error)
		uid  string
	}{
		{provider.WorkBuddy, func(d string) ([]*auth.Auth, error) { return auth.LoadWorkBuddyDir(d, "cn") }, "u-wb"},
		{provider.TraeWork, auth.LoadTraeDir, "4197274137669852"},
		{provider.GLM, auth.LoadGLMDir, "u-glm"},
	} {
		got, err := tc.load(a.cfg.AuthDir)
		if err != nil || len(got) != 1 || got[0].UID != tc.uid {
			t.Fatalf("渠道 %s 读回失败: n=%d err=%v", tc.kind, len(got), err)
		}
	}
}

// TestSubmitJSONAuthBatch_ExportEnvelopeRoundTrip 「导出全部」的产物原样粘回必须能导入，
// 且 TraeWork 的 deviceId 不能在往返中丢失（丢了就是无设备身份，R37 的 20401/4017 诱因）。
func TestSubmitJSONAuthBatch_ExportEnvelopeRoundTrip(t *testing.T) {
	src := newJSONLoginTestApp(t)
	if _, err := src.SubmitJSONAuth("traework",
		`{"auth":{"accessToken":"a","refreshToken":"r","machineId":"m-9","deviceId":"d-9"},`+
			`"account":{"uid":"u-rt","nickname":"迁移号"}}`); err != nil {
		t.Fatalf("准备源账号: %v", err)
	}
	doc, err := src.ExportAllAccountsJSON()
	if err != nil {
		t.Fatalf("导出全部: %v", err)
	}
	if n := doc["count"].(int); n != 1 {
		t.Fatalf("导出条数应为 1，实得 %v", doc["count"])
	}
	b, _ := json.MarshalIndent(doc, "", "  ")

	dst := newJSONLoginTestApp(t)
	res, err := dst.SubmitJSONAuthBatch(string(b))
	if err != nil || res.OKCount != 1 {
		t.Fatalf("把导出文件粘回失败: err=%v res=%+v", err, res)
	}
	got, err := auth.LoadTraeDir(dst.cfg.AuthDir)
	if err != nil || len(got) != 1 {
		t.Fatalf("粘回后读不到账号: n=%d err=%v", len(got), err)
	}
	if got[0].DeviceID != "d-9" || got[0].MachineID != "m-9" {
		t.Fatalf("往返丢失设备身份: machineId=%q deviceId=%q", got[0].MachineID, got[0].DeviceID)
	}
}

// TestSubmitJSONAuthBatch_PartialFailure 个别失败不阻断其余，且回执带**序号**
// （用户面对几十条粘贴文本，只说「有一条失败」等于没说）。
func TestSubmitJSONAuthBatch_PartialFailure(t *testing.T) {
	a := newJSONLoginTestApp(t)
	text := batchText(t, []any{
		nested("workbuddy", "ok-1", ""),
		nested("nosuchchannel", "bad-1", ""),         // 渠道不支持
		map[string]any{"auth": map[string]any{"accessToken": "x"}}, // 缺 channel
	})
	res, err := a.SubmitJSONAuthBatch(text)
	if err != nil {
		t.Fatalf("有成功项时不应整体报错: %v", err)
	}
	if res.OKCount != 1 || len(res.Failed) != 2 {
		t.Fatalf("应为 1 成功 2 失败，实得 ok=%d failed=%+v", res.OKCount, res.Failed)
	}
	idx := map[int]bool{}
	for _, f := range res.Failed {
		idx[f.Index] = true
		if strings.TrimSpace(f.Error) == "" {
			t.Fatalf("失败项 #%d 没有原因文本", f.Index)
		}
	}
	if !idx[1] || !idx[2] {
		t.Fatalf("失败序号应为 1 和 2，实得 %v", idx)
	}
}

// TestSubmitJSONAuthBatch_AllFailed 全部失败时既回结构化明细、也返回 error
// （面板据此提示「一条都没进去」）。
func TestSubmitJSONAuthBatch_AllFailed(t *testing.T) {
	a := newJSONLoginTestApp(t)
	res, err := a.SubmitJSONAuthBatch(batchText(t, []any{
		nested("nosuch", "x1", ""), nested("nosuch", "x2", ""),
	}))
	if err == nil {
		t.Fatal("全部失败应返回 error")
	}
	if res == nil || len(res.Failed) != 2 || res.OKCount != 0 {
		t.Fatalf("应同时带回逐条明细，实得 %+v", res)
	}
}

// TestSubmitJSONAuthBatch_Oversize 超大粘贴要挡住：每条都要解析+落盘，
// glm 渠道还会触发上游验证。
func TestSubmitJSONAuthBatch_Oversize(t *testing.T) {
	a := newJSONLoginTestApp(t)
	items := make([]any, jsonBatchLimit+1)
	for i := range items {
		items[i] = nested("workbuddy", fmt.Sprintf("u-%d", i), "")
	}
	if _, err := a.SubmitJSONAuthBatch(batchText(t, items)); err == nil {
		t.Fatalf("超过 %d 条应被拒绝", jsonBatchLimit)
	} else if !strings.Contains(err.Error(), "最多") {
		t.Fatalf("报错文案应说明上限，实得: %v", err)
	}
}

// TestSubmitJSONAuthBatch_Forms 三种粘贴形态都要能用。
func TestSubmitJSONAuthBatch_Forms(t *testing.T) {
	t.Run("裸数组", func(t *testing.T) {
		a := newJSONLoginTestApp(t)
		if res, err := a.SubmitJSONAuthBatch(batchText(t, []any{nested("workbuddy", "u1", "")})); err != nil || res.OKCount != 1 {
			t.Fatalf("裸数组失败: %v %+v", err, res)
		}
	})
	t.Run("accounts信封", func(t *testing.T) {
		a := newJSONLoginTestApp(t)
		env := map[string]any{"version": "0.1.11", "accounts": []any{nested("workbuddy", "u2", "")}}
		if res, err := a.SubmitJSONAuthBatch(batchText(t, env)); err != nil || res.OKCount != 1 {
			t.Fatalf("信封形态失败: %v %+v", err, res)
		}
	})
	t.Run("json字符串字段", func(t *testing.T) {
		a := newJSONLoginTestApp(t)
		items := []any{map[string]any{
			"channel": "workbuddy",
			"json":    `{"accessToken":"a","refreshToken":"r","uid":"u3"}`,
		}}
		if res, err := a.SubmitJSONAuthBatch(batchText(t, items)); err != nil || res.OKCount != 1 {
			t.Fatalf("json 字符串形态失败: %v %+v", err, res)
		}
	})
	t.Run("单条对象", func(t *testing.T) {
		a := newJSONLoginTestApp(t)
		if res, err := a.SubmitJSONAuthBatch(batchText(t, nested("workbuddy", "u4", ""))); err != nil || res.OKCount != 1 {
			t.Fatalf("单条对象失败: %v %+v", err, res)
		}
	})
}

// TestSubmitJSONAuthBatch_RejectsGarbage 非 JSON / 空文本要报错且不写任何文件。
func TestSubmitJSONAuthBatch_RejectsGarbage(t *testing.T) {
	a := newJSONLoginTestApp(t)
	for _, s := range []string{"", "  ", "not json", "[1,2,3]", `{"foo":"bar"}`} {
		res, err := a.SubmitJSONAuthBatch(s)
		if err == nil {
			t.Fatalf("非法输入 %q 竟成功了: %+v", s, res)
		}
	}
	ents, _ := os.ReadDir(a.cfg.AuthDir)
	if len(ents) != 0 {
		t.Fatalf("非法输入不应落任何文件，实得 %d 个", len(ents))
	}
}

// TestExportAllAccountsJSON_ChannelAndDedup 钉住两条边界：
//  1. workbuddyai 文件不能被 `workbuddy*.json` 的 glob 抢走标成 workbuddy；
//  2. 每个文件只出现一次（两个加载器都可能命中同一文件）。
//
// 另外验证「cn 实例也能导出国际版账号」——按 cfg.Region 过滤会静默漏备份。
func TestExportAllAccountsJSON_ChannelAndDedup(t *testing.T) {
	a := newJSONLoginTestApp(t)
	a.cfg.Region = "cn" // 关键：cn 实例也要能备份 global 账号
	if _, err := a.SubmitJSONAuth("workbuddy",
		`{"auth":{"accessToken":"a1","refreshToken":"r1","domain":"copilot.tencent.com"},"account":{"uid":"cn-1"}}`); err != nil {
		t.Fatalf("导入 cn 号: %v", err)
	}
	if _, err := a.SubmitJSONAuth("workbuddyai",
		`{"auth":{"accessToken":"a2","refreshToken":"r2","domain":"www.workbuddy.ai"},"account":{"uid":"gl-1"}}`); err != nil {
		t.Fatalf("导入国际版号: %v", err)
	}
	if _, err := a.SubmitJSONAuth("traework",
		`{"auth":{"accessToken":"a3","refreshToken":"r3"},"account":{"uid":"tr-1"}}`); err != nil {
		t.Fatalf("导入 traework 号: %v", err)
	}

	doc, err := a.ExportAllAccountsJSON()
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	raw, _ := json.Marshal(doc["accounts"])
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析导出结果: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应导出 3 条，实得 %d：%s", len(list), raw)
	}
	want := map[string]string{"cn-1": "workbuddy", "gl-1": "workbuddyai", "tr-1": "traework"}
	seen := map[string]int{}
	for _, it := range list {
		uid, _ := it["account"].(map[string]any)["uid"].(string)
		ch, _ := it["channel"].(string)
		seen[uid]++
		if exp, ok := want[uid]; !ok {
			t.Fatalf("导出里出现未知账号 %q", uid)
		} else if ch != exp {
			t.Fatalf("账号 %s 的 channel 是 %q，应为 %q（glob 边界没守住）", uid, ch, exp)
		}
	}
	for uid, n := range seen {
		if n != 1 {
			t.Fatalf("账号 %s 出现 %d 次，未按文件去重", uid, n)
		}
	}
}
