package app

import (
	"strings"
	"testing"
)

// TestLoginCallbackSupported 面板据此决定「要不要显示粘贴框」：
// 只对本机回调渠道显示，避免给设备码/深链渠道摆一个点了必报错的入口。
func TestLoginCallbackSupported(t *testing.T) {
	yes := []string{"traework", "traecode", "qwenwork"}
	no := []string{"workbuddy", "workbuddyai", "qodercn", "qodercom", "glm", "raccoon", "loomy", "monkeycode", ""}
	for _, c := range yes {
		if !LoginCallbackSupported(c) {
			t.Errorf("%s 应有手工回调入口", c)
		}
	}
	for _, c := range no {
		if LoginCallbackSupported(c) {
			t.Errorf("%s 不该有手工回调入口（它不用本机回调）", c)
		}
	}
}

// TestSubmitLoginCallback_UnsupportedChannel 对不支持的渠道要给出**可行动**的说明，
// 而不是笼统的「失败」——否则用户会以为是自己粘错了。
func TestSubmitLoginCallback_UnsupportedChannel(t *testing.T) {
	a := &App{}
	err := a.SubmitLoginCallback("qodercn", "http://127.0.0.1:1/cb?code=x")
	if err == nil {
		t.Fatal("不支持本机回调的渠道应报错")
	}
	if !strings.Contains(err.Error(), "设备码") && !strings.Contains(err.Error(), "无需手工粘贴") {
		t.Fatalf("报错应说明原因，实得: %v", err)
	}
}

// TestSubmitLoginCallback_EmptyInput 空输入要在碰任何状态之前就拦住。
func TestSubmitLoginCallback_EmptyInput(t *testing.T) {
	a := &App{loginStateFP: t.TempDir() + "/login-state.json"}
	if err := a.SubmitLoginCallback("traework", "   "); err == nil {
		t.Fatal("空粘贴应报错")
	}
}
