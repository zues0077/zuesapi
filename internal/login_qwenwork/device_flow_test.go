package login_qwenwork

import (
	"encoding/base64"
	"time"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStart_DeviceFlowURL 验证 Start 生成的是 device-flow 登录 URL，
// 参数齐全（channel 白名单精确校验，多一个少一个都会被网关拒）：
// challenge/challenge_method/nonce/machine_id/client_id/redirect_uri。
func TestStart_DeviceFlowURL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "login-state.json")
	authURL, err := Start(nil, fp)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "gateway.qwenwork.cn" || u.Path != "/device/selectAccounts" {
		t.Fatalf("登录 URL 不对: %s", authURL)
	}
	q := u.Query()
	for _, k := range []string{"challenge", "challenge_method", "nonce", "machine_id", "client_id", "redirect_uri"} {
		if q.Get(k) == "" {
			t.Errorf("登录 URL 缺少参数 %s: %s", k, authURL)
		}
	}
	if q.Get("challenge_method") != "S256" {
		t.Errorf("challenge_method = %q, want S256", q.Get("challenge_method"))
	}
	if q.Get("client_id") != DeviceClientID {
		t.Errorf("client_id = %q, want %q", q.Get("client_id"), DeviceClientID)
	}
	if q.Get("redirect_uri") != DeviceRedirectURI {
		t.Errorf("redirect_uri = %q, want %q", q.Get("redirect_uri"), DeviceRedirectURI)
	}
	// challenge 必须是 verifier 的 S256（43 字符 base64url）
	st, err := readState(fp)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if st.Challenge != pkceChallenge(st.Verifier) {
		t.Error("state.challenge 不是 verifier 的 S256")
	}
	if len(st.Verifier) < 43 || len(st.Verifier) > 128 {
		t.Errorf("verifier 长度 %d 不在 43..128", len(st.Verifier))
	}
	if q.Get("challenge") != st.Challenge || q.Get("nonce") != st.Nonce {
		t.Error("URL 的 challenge/nonce 与 state 不一致")
	}
	// machine_id 必须已持久化
	if _, err := os.Stat(filepath.Join(dir, machineIDFile)); err != nil {
		t.Errorf("machine_id 未持久化: %v", err)
	}
}

// TestStart_MachineIDReuse 验证 machine_id 跨轮复用（服务端视为同一设备）。
func TestStart_MachineIDReuse(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "login-state.json")
	u1, _ := Start(nil, fp)
	q1, _ := url.Parse(u1)
	id1 := q1.Query().Get("machine_id")
	// 模拟下一轮：删 state 但保留 machine_id 文件
	_ = os.Remove(fp)
	u2, _ := Start(nil, fp)
	q2, _ := url.Parse(u2)
	if id2 := q2.Query().Get("machine_id"); id2 != id1 {
		t.Fatalf("machine_id 未复用: %q -> %q", id1, id2)
	}
}

// devicePollHarness 用可控 gateway 替换常量，构造一次 Poll 所需的 state 文件。
func devicePollHarness(t *testing.T, handler http.HandlerFunc) (*http.Client, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	orig := gatewayBase
	gatewayBase = srv.URL
	t.Cleanup(func() { gatewayBase = orig })
	fp := filepath.Join(t.TempDir(), "login-state.json")
	st := state{
		Verifier:  "verifier-abc",
		Challenge: "challenge-xyz",
		Nonce:     "nonce-123",
		MachineID: "machine-1",
		CreatedAt: currentUnix(),
	}
	if err := writeState(fp, st); err != nil {
		t.Fatal(err)
	}
	return &http.Client{}, fp
}

// TestPoll_Pending 404 与 400(INVALID_DEVICE_FLOW) 都视为进行中。
func TestPoll_Pending(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"404 未登记", 404, `{}`},
		{"400 INVALID_DEVICE_FLOW", 400, `{"errorCode":"INVALID_DEVICE_FLOW","errorMessage":"pending"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fp := devicePollHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			})
			// poll 请求参数必须带 nonce/verifier/challenge_method
			_, err := Poll(client, fp)
			if err != ErrPending {
				t.Fatalf("want ErrPending, got %v", err)
			}
		})
	}
}

// TestPoll_Success 2xx 且三连绑定匹配 → 返回 token（含 JWT uid）。
func TestPoll_Success(t *testing.T) {
	// 造一个带 user_id payload 的假 JWT（parseJWTIdentity 从 base64url 解）
	jwt := "h." + base64urlJSON(`{"user_id":"uid-42","username":"雪碧7266"}`) + ".s"
	client, fp := devicePollHarness(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("nonce") != "nonce-123" || q.Get("verifier") != "verifier-abc" || q.Get("challenge_method") != "S256" {
			t.Errorf("poll 参数不对: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"token":"` + jwt + `","refresh_token":"ory_rt_xxx","nonce":"nonce-123","code_challenge":"challenge-xyz","code_challenge_method":"S256","expires_in":3600}`))
	})
	res, err := Poll(client, fp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.UID != "uid-42" || res.Nickname != "雪碧7266" {
		t.Errorf("身份解析不对: uid=%q nick=%q", res.UID, res.Nickname)
	}
	if res.AccessToken != jwt || res.RefreshToken != "ory_rt_xxx" || res.ExpiresIn != 3600 {
		t.Errorf("token 不对: %+v", res)
	}
	// 成功后 state 文件应被清理
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Error("成功后 state 文件未清理")
	}
}

// TestPoll_BindingMismatch 三连绑定不匹配 → 拒绝采纳（防串号）。
func TestPoll_BindingMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"nonce 不符", `{"token":"t","refresh_token":"r","nonce":"EVIL","code_challenge":"challenge-xyz","code_challenge_method":"S256"}`},
		{"challenge 不符", `{"token":"t","refresh_token":"r","nonce":"nonce-123","code_challenge":"EVIL","code_challenge_method":"S256"}`},
		{"method 不符", `{"token":"t","refresh_token":"r","nonce":"nonce-123","code_challenge":"challenge-xyz","code_challenge_method":"plain"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fp := devicePollHarness(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := Poll(client, fp)
			if err == nil || !strings.Contains(err.Error(), "绑定校验失败") {
				t.Fatalf("want 绑定校验失败, got %v", err)
			}
		})
	}
}

// TestPoll_ProtocolError 400 非 INVALID_DEVICE_FLOW → 终态失败（不是进行中）。
func TestPoll_ProtocolError(t *testing.T) {
	client, fp := devicePollHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"errorCode":"BAD_REQUEST","errorMessage":"bad token"}`))
	})
	_, err := Poll(client, fp)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 终态失败, got %v", err)
	}
}

// currentUnix / base64urlJSON 测试辅助。
func currentUnix() int64 { return time.Now().Unix() }
func base64urlJSON(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
