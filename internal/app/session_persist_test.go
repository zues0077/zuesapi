package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoginCookieIsPersistent 登录必须下发**持久** cookie：
// 会话 cookie（无 Max-Age）在浏览器关闭后即失效，用户每次重开都要重输密码。
func TestLoginCookieIsPersistent(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "s3cret-pass")
	mux := panelMux(a)

	w := doReq(mux, "POST", "/api/auth/login", `{"password":"s3cret-pass"}`, nil)
	if w.Code != 200 {
		t.Fatalf("登录应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var ck *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("登录未下发会话 cookie")
	}
	if ck.MaxAge <= 0 {
		t.Fatalf("cookie 必须是持久 cookie（Max-Age>0），实际 %d", ck.MaxAge)
	}
	if want := int(sessionTTL / time.Second); ck.MaxAge != want {
		t.Errorf("cookie Max-Age 应与会话 TTL 一致：want %d got %d", want, ck.MaxAge)
	}

	// 受保护请求通过后应重下同一 token 的 cookie（滑动续期），否则浏览器侧会先过期
	w2 := doReq(mux, "GET", "/api/state", "", ck)
	if w2.Code != 200 {
		t.Fatalf("带有效 cookie 的 /api/state 应 200，实际 %d", w2.Code)
	}
	renewed := false
	for _, c := range w2.Result().Cookies() {
		if c.Name == sessionCookie && c.Value == ck.Value && c.MaxAge > 0 {
			renewed = true
		}
	}
	if !renewed {
		t.Error("有效会话的请求应重下 cookie 以续期")
	}
}

// TestSessionSurvivesRestart 会话表落盘：进程重启后同一 token 仍有效，
// 但口令指纹变了（改过密码）就不能通过。
func TestSessionSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), sessionFile)
	s1 := newAuthState()
	s1.bind(fp)
	tok := s1.issue("brief-abc")

	s2 := newAuthState() // 模拟重启后的新进程
	s2.bind(fp)
	if !s2.valid(tok, "brief-abc") {
		t.Fatal("重启后同一 token 应仍然有效")
	}
	if s2.valid(tok, "other-brief") {
		t.Fatal("口令指纹不匹配时不应通过")
	}
}

// TestExpiredSessionNotRestored 过期条目不得被恢复。
func TestExpiredSessionNotRestored(t *testing.T) {
	fp := filepath.Join(t.TempDir(), sessionFile)
	d := sessionDisk{Sessions: map[string]diskSession{
		tokHash("old-token"):  {Exp: time.Now().Add(-time.Hour).Unix(), Label: "b"},
		tokHash("live-token"): {Exp: time.Now().Add(time.Hour).Unix(), Label: "b"},
	}}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s := newAuthState()
	s.bind(fp)
	if s.valid("old-token", "b") {
		t.Error("已过期会话不应被恢复")
	}
	if !s.valid("live-token", "b") {
		t.Error("未过期会话应被恢复")
	}
}

// TestDropAllClearsDisk 改密码（dropAll）必须同步清空磁盘副本，否则重启后旧会话复活。
func TestDropAllClearsDisk(t *testing.T) {
	fp := filepath.Join(t.TempDir(), sessionFile)
	s := newAuthState()
	s.bind(fp)
	tok := s.issue("b")
	s.dropAll()

	s2 := newAuthState()
	s2.bind(fp)
	if s2.valid(tok, "b") {
		t.Error("dropAll 后磁盘副本应已清空，重启不应恢复旧会话")
	}
}

// TestRevokeClearsDisk 退出登录必须立刻把该会话从磁盘抹掉。
func TestRevokeClearsDisk(t *testing.T) {
	fp := filepath.Join(t.TempDir(), sessionFile)
	s := newAuthState()
	s.bind(fp)
	tok := s.issue("b")
	s.revoke(tok)

	s2 := newAuthState()
	s2.bind(fp)
	if s2.valid(tok, "b") {
		t.Error("退出登录后该 token 不应在重启后复活")
	}
}

// TestSessionFileStoresHashOnly 落盘文件里不得出现 token 原文（只有 SHA-256）。
func TestSessionFileStoresHashOnly(t *testing.T) {
	fp := filepath.Join(t.TempDir(), sessionFile)
	s := newAuthState()
	s.bind(fp)
	tok := s.issue("b")

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) > 0 && strings.Contains(string(raw), tok) {
		t.Fatal("落盘文件不得包含 token 原文")
	}
	var d sessionDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("落盘格式应为合法 JSON: %v", err)
	}
	if _, ok := d.Sessions[tokHash(tok)]; !ok {
		t.Error("落盘应以 tokenHash 为键")
	}
}
