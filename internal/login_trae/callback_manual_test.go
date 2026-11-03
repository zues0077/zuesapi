package login_trae

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRawState 直接落一份 state 文件（SubmitCallback 走的是同一份文件）。
func writeRawState(t *testing.T, path string, st state) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readRawState(t *testing.T, path string) state {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 state: %v", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("解析 state: %v", err)
	}
	return st
}

// TestSubmitCallback_NoLoginInFlight 没有进行中的登录时必须报错，
// 不能让一次迟到的粘贴污染下一次兑换。
func TestSubmitCallback_NoLoginInFlight(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "login-state.json")
	err := SubmitCallback(fp, "http://127.0.0.1:9/authorize?refreshToken=abc")
	if err == nil || !strings.Contains(err.Error(), "没有进行中的登录") {
		t.Fatalf("应报「没有进行中的登录」，实得: %v", err)
	}
}

// TestSubmitCallback_RefreshTokenURL 老流程：地址栏里带 refreshToken。
func TestSubmitCallback_RefreshTokenURL(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "login-state.json")
	writeRawState(t, fp, state{CodeVerifier: "v1", MachineID: "m", DeviceID: "d"})

	raw := "http://127.0.0.1:43985/authorize?refreshToken=RT-123&userInfo=%7B%7D"
	if err := SubmitCallback(fp, raw); err != nil {
		t.Fatalf("SubmitCallback: %v", err)
	}
	st := readRawState(t, fp)
	if st.RefreshToken != "RT-123" {
		t.Fatalf("refreshToken 没写进 state: %q", st.RefreshToken)
	}
	if st.Err != "" {
		t.Fatalf("应清掉终态错误让 Poll 走兑换，实得 Err=%q", st.Err)
	}
	if st.CodeVerifier != "v1" || st.DeviceID != "d" {
		t.Fatalf("原有 PKCE/设备字段被覆盖: %+v", st)
	}
}

// TestSubmitCallback_ClearsStickyError 已固化终态错误（如 20401）后再粘贴成功回调，
// 必须能重新走兑换 —— 否则用户换个浏览器路径也救不回来。
func TestSubmitCallback_ClearsStickyError(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "login-state.json")
	writeRawState(t, fp, state{Err: "device limit reached", AuthCode: ""})
	if err := SubmitCallback(fp, "http://127.0.0.1:1/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22AC-9%22%7D"); err != nil {
		t.Fatalf("SubmitCallback: %v", err)
	}
	st := readRawState(t, fp)
	if st.Err != "" || st.AuthCode != "AC-9" {
		t.Fatalf("Err 未清或 AuthCode 未写入: %+v", st)
	}
}

// TestSubmitCallback_RejectsGarbage 粘错内容要报错且**不动** state。
func TestSubmitCallback_RejectsGarbage(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "login-state.json")
	writeRawState(t, fp, state{RefreshToken: "keep-me"})
	before := readRawState(t, fp)
	for _, bad := range []string{"", "   ", "http://127.0.0.1:1/authorize?foo=bar", "not a url at all"} {
		if err := SubmitCallback(fp, bad); err == nil {
			t.Fatalf("非法输入 %q 竟然成功", bad)
		}
	}
	after := readRawState(t, fp)
	if after.RefreshToken != before.RefreshToken {
		t.Fatal("失败路径改动了 state")
	}
}

// TestParseCallback_Forms 顺带钉住三种回调形态都能解析（手工粘贴与自动回调共用它）。
func TestParseCallback_Forms(t *testing.T) {
	cases := []struct{ name, url, want string }{
		{"refreshToken", "/authorize?refreshToken=A1", "A1"},
		{"userJwt", "/authorize?userJwt=%7B%22RefreshToken%22%3A%22B2%22%7D", "B2"},
	}
	for _, c := range cases {
		if got := ParseCallback(c.url).RefreshToken; got != c.want {
			t.Errorf("%s 形态解析失败: got %q want %q", c.name, got, c.want)
		}
	}
}
