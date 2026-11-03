package login_qwenwork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touchState(t *testing.T) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "login-state.json")
	if err := os.WriteFile(fp, []byte(`{"verifier":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestSubmitCallback_FullCallbackURL 主路径：把浏览器地址栏那条完整回调地址粘回来，
// code 注入到与自动回调同一个 pending 通道，Poll 下一轮就会去兑换。
func TestSubmitCallback_FullCallbackURL(t *testing.T) {
	fp := touchState(t)
	pending.reset()
	if err := SubmitCallback(fp, "http://127.0.0.1:43985/callback?code=CODE-77&state=xyz"); err != nil {
		t.Fatalf("SubmitCallback: %v", err)
	}
	code, errMsg, ok := pending.snapshot()
	if !ok || code != "CODE-77" || errMsg != "" {
		t.Fatalf("code 未注入: ok=%v code=%q err=%q", ok, code, errMsg)
	}
}

// TestSubmitCallback_BareCode 用户常常只复制 code 值，也要能用。
func TestSubmitCallback_BareCode(t *testing.T) {
	fp := touchState(t)
	pending.reset()
	if err := SubmitCallback(fp, "  CODE-88  "); err != nil {
		t.Fatalf("裸 code 被拒: %v", err)
	}
	code, _, ok := pending.snapshot()
	if !ok || code != "CODE-88" {
		t.Fatalf("裸 code 未注入: ok=%v code=%q", ok, code)
	}
}

// TestSubmitCallback_ErrorParam 上游回 error/error_description 时按失败处理，
// 让 Poll 立刻把原因报给面板，而不是干等到 5 分钟超时。
func TestSubmitCallback_ErrorParam(t *testing.T) {
	fp := touchState(t)
	pending.reset()
	if err := SubmitCallback(fp, "http://127.0.0.1:1/callback?error=access_denied&error_description=user%20cancelled"); err != nil {
		t.Fatalf("error 回调不应报错: %v", err)
	}
	_, errMsg, ok := pending.snapshot()
	if ok || !strings.Contains(errMsg, "access_denied") {
		t.Fatalf("未按失败处理: ok=%v err=%q", ok, errMsg)
	}
}

// TestSubmitCallback_Guards 空内容、无进行中登录、解析不出 code 三种情况都必须拒绝，
// 且不能往 pending 里塞任何东西（否则迟到的粘贴会污染下一次登录）。
func TestSubmitCallback_Guards(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	if err := SubmitCallback(missing, "http://x/y?code=Z"); err == nil ||
		!strings.Contains(err.Error(), "没有进行中的登录") {
		t.Fatalf("无进行中登录应被拒: %v", err)
	}
	fp := touchState(t)
	pending.reset()
	for _, bad := range []string{"", "   ", "http://127.0.0.1:1/callback?foo=bar"} {
		if err := SubmitCallback(fp, bad); err == nil {
			t.Fatalf("非法输入 %q 竟然成功", bad)
		}
	}
	if _, _, ok := pending.snapshot(); ok {
		t.Fatal("失败路径不应注入 code")
	}
}
