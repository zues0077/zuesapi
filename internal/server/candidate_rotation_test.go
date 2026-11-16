package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/pool"
	"workbuddy-api/internal/provider"
)

// 本文件锁定 E-3（候选切号）的核心语义：网关通过 `X-WorkBuddy-Candidates` 把候选实体阵列
// 传给内层，内层在额度用尽类错误（soft_rate / hard_credit）时切到下一候选实体；
// 没有下一实体或其他 rotatable 错误，回退到实体内旧的账号级旋转。

// candidateUpstream 按 uid 决定返回形态的可配置假上游：
// uid 前缀区分实体（候选），返回错误还是成功均由 uid 键映射。
type candidateUpstream struct {
	mu    sync.Mutex
	calls []string
	// statuses 按 uid 记录返回的错误码（0 = 成功）
	statuses map[string]int
	// kinds 按 uid 记录 Classify 结果
	kinds map[string]provider.ErrKind
}

func (u *candidateUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u *candidateUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	status, ok := u.statuses[a.UID]
	if !ok || status == 0 {
		return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), http.StatusOK, nil, nil
	}
	body := `{"status":6004,"message":"usage exceeds frequency limit, 您也可以切换其他模型继续使用，将在 2026-09-23 14:47:19 UTC+8 重置"}`
	if status == http.StatusUnauthorized {
		body = `{"status":40102,"message":"unauthorized user(40102)"}`
	}
	return nil, status, []byte(body), nil
}

func (u *candidateUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "m"}}, nil
}
func (u *candidateUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *candidateUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *candidateUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *candidateUpstream) DailyCheckin(*auth.Auth) error { return nil }

func (u *candidateUpstream) Classify(status int, body string) provider.ErrKind {
	if status == http.StatusOK {
		return provider.ErrNone
	}
	if status == http.StatusUnauthorized {
		return provider.ErrSessionDead
	}
	return provider.ErrSoftRate
}

func (u *candidateUpstream) Stream(w http.ResponseWriter, r io.Reader, _ string) (map[string]any, error) {
	_, _ = io.Copy(io.Discard, r)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	return nil, nil
}

func (u *candidateUpstream) Aggregate(r io.Reader, _ string) (map[string]any, error) {
	_, _ = io.Copy(io.Discard, r)
	return map[string]any{"choices": []any{}}, nil
}

func (u *candidateUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *candidateUpstream) callList() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string{}, u.calls...)
}

func newCandidateHandler(t *testing.T, up *candidateUpstream) *Handler {
	p1 := pool.New(t.TempDir() + "/state1.json")
	p1.Add(&auth.Auth{Kind: "qoder", AccessToken: "at-e1a", ExpiresAt: 4102444800, UID: "e1a"})
	p1.Add(&auth.Auth{Kind: "qoder", AccessToken: "at-e1b", ExpiresAt: 4102444800, UID: "e1b"})
	p2 := pool.New(t.TempDir() + "/state2.json")
	p2.Add(&auth.Auth{Kind: "glm", AccessToken: "at-e2a", ExpiresAt: 4102444800, UID: "e2a"})
	p2.Add(&auth.Auth{Kind: "glm", AccessToken: "at-e2b", ExpiresAt: 4102444800, UID: "e2b"})
	// 候选实体 e1a/e1b 分值更高，确保首挑确定性（pool.PickExcluding 遍历 map 顺序随机）
	p1.SetCreditDetail("e1a", 1000, 0, 0)
	p1.SetCreditDetail("e1b", 500, 0, 0)
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Qoder: {Kind: provider.Qoder, Pool: p1, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "completions"}}},
			provider.GLM: {Kind: provider.GLM, Pool: p2, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey:       "",
		HardCooldown: 60_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  60_000_000_000,
		MaxRotate:    3,
	})
}

// 候选阵列头，模拟网关行为（alias|cand1|cand2）。
func candidateHeader(alias string, entities ...string) string {
	if len(entities) == 0 {
		return ""
	}
	b := []string{alias}
	b = append(b, entities...)
	return strings.Join(b, "|")
}

// TestCandidateCutOnQuotaError 额度用尽类错误切号到下一候选实体（不在同一实体上死磕）。
func TestCandidateCutOnQuotaError(t *testing.T) {
	up := &candidateUpstream{
		statuses: map[string]int{
			"e1a": http.StatusTooManyRequests, // qoder 实体：e1a 撞 429
			"e1b": http.StatusTooManyRequests,
		},
	}
	h := newCandidateHandler(t, up)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddyai/alias","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("X-WorkBuddy-Candidates", candidateHeader("alias", "qoder/completions", "glm/m"))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（切到实体 2 的 e2a 成功）\n响应: %s", rec.Code, rec.Body.String())
	}
	calls := up.callList()
	if len(calls) != 2 || calls[0] != "e1a" || calls[1] != "e2a" {
		t.Fatalf("应 e1a 429 → 切号 e2a（调用序列 [e1a, e2a]），实际 %v", calls)
	}
}

// TestCandidatesExhaustedTransmitsLastUpstreamError 候选耗尽时透传**最后一次\n// 上游原始响应**（429 + 6004 原文），而不是包装成 503 no_healthy_account。
// 透传最后一个上游错误原文（429 + 6004），而非包装成 503 no_healthy_account。
func TestCandidatesExhaustedTransmitsLastUpstreamError(t *testing.T) {
	up := &candidateUpstream{
		statuses: map[string]int{
			"e1a": http.StatusTooManyRequests,
			"e1b": http.StatusTooManyRequests,
			"e2a": http.StatusTooManyRequests,
			"e2b": http.StatusTooManyRequests,
		},
	}
	h := newCandidateHandler(t, up)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddyai/alias","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("X-WorkBuddy-Candidates", candidateHeader("alias", "qoder/completions", "glm/m"))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, want 429（候选耗尽透传最后的上游 429，而非 503）\n响应: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("应透传上游原始响应体（含 6004），实际: %s", rec.Body.String())
	}
	calls := up.callList()
	// e1a 429 → 切号实体 2 → e2a 429（无下一实体，实体内旋转到 e2b 429），共 3 次。
	if len(calls) != 3 || calls[0] != "e1a" || calls[1] != "e2a" || calls[2] != "e2b" {
		t.Fatalf("应逐号尝试 e1a→e2a→e2b（额度用尽类错误有下一实体就切号，不试 e1b），实际 %d 次 %v", len(calls), calls)
	}
}

// TestSessionDeadRotatesWithinEntity 非额度类 rotatable 错误（session_dead）不切实体，
// 在实体内的账号池里继续旋转（与 E-3 之前的旧行为一致）。
func TestSessionDeadRotatesWithinEntity(t *testing.T) {
	up := &candidateUpstream{
		statuses: map[string]int{
			"e1a": http.StatusUnauthorized, // e1a session dead
		},
	}
	h := newCandidateHandler(t, up)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder/completions","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("X-WorkBuddy-Candidates", candidateHeader("alias", "qoder/completions"))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（e1b 修复）\n响应: %s", rec.Code, rec.Body.String())
	}
	calls := up.callList()
	if len(calls) != 2 || calls[0] != "e1a" || calls[1] != "e1b" {
		t.Fatalf("应 e1a 401 → 实体内切到 e1b（调用序列 [e1a, e1b]），实际 %v", calls)
	}
}

// TestNoCandidateHeaderPlainMode 没有候选头 = 普通模式，行为和旧逻辑完全一致。
func TestNoCandidateHeaderPlainMode(t *testing.T) {
	up := &candidateUpstream{
		statuses: map[string]int{
			"e1a": http.StatusTooManyRequests, // 实体内 e1a 撞 429
			"e1b": http.StatusTooManyRequests,
		},
	}
	h := newCandidateHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder/completions","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, want 429（候选耗尽透传）\n响应: %s", rec.Code, rec.Body.String())
	}
	calls := up.callList()
	if len(calls) != 2 || calls[0] != "e1a" || calls[1] != "e1b" {
		t.Fatalf("应逐号尝试实体内 e1a、e1b 各一次，实际 %d 次 %v", len(calls), calls)
	}
}
