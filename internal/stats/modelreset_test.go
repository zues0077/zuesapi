package stats

import (
	"testing"
	"time"
)

// TestModelQuotaExhausted 验证反应式额度：撞限流记录重置点后，ModelQuota 应返回
// exhausted=true 与未来 resetAt；且 canonical 键能跨「带/不带倍率后缀」命中同一模型。
func TestModelQuotaExhausted(t *testing.T) {
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	s.AddUsage("workbuddy/hy3 (0.00x)", 10, 20, 0, 100, 1.0, now)
	reset := now.Add(2 * time.Hour)
	s.MarkModelRateLimited("workbuddy/hy3", reset, now)

	reqs, exhausted, resetAt := s.ModelQuota("workbuddy/hy3")
	if reqs != 1 {
		t.Fatalf("want reqs=1, got %d", reqs)
	}
	if !exhausted {
		t.Fatalf("want exhausted=true")
	}
	if !resetAt.Equal(reset.Truncate(time.Second)) && resetAt.Before(now) {
		t.Fatalf("resetAt not future: %v", resetAt)
	}
}

// TestModelQuotaIgnoresPast 过去时刻（已重置）不应记为用尽。
func TestModelQuotaIgnoresPast(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	now := time.Now()
	s.AddUsage("workbuddy/hy3", 10, 20, 0, 100, 1.0, now)
	s.MarkModelRateLimited("workbuddy/hy3", now.Add(-time.Minute), now)

	_, exhausted, _ := s.ModelQuota("workbuddy/hy3")
	if exhausted {
		t.Fatalf("past reset must not mark exhausted")
	}
}

// TestModelQuotaRollDayCleared 跨天清零：昨日重置点不污染新的一天。
func TestModelQuotaRollDayCleared(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	s.AddUsage("workbuddy/hy3", 10, 20, 0, 100, 1.0, day1)
	s.MarkModelRateLimited("workbuddy/hy3", day1.Add(3*time.Hour), day1)

	// 触发跨天归档（Snapshot/AddUsage 内部 rollDayLocked 会清零 modelReset）。
	day2 := day1.AddDate(0, 0, 1)
	s.AddUsage("workbuddy/hy3", 5, 5, 0, 100, 1.0, day2)
	reqs, exhausted, _ := s.ModelQuota("workbuddy/hy3")
	if exhausted {
		t.Fatalf("previous-day reset must be cleared on roll day")
	}
	if reqs != 1 {
		t.Fatalf("new day should count only the day2 request (reqs=1), got %d", reqs)
	}
}

// TestModelQuotaUnknownModel 无记录模型返回零值，不 panic。
func TestModelQuotaUnknownModel(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	reqs, exhausted, resetAt := s.ModelQuota("workbuddy/never-used")
	if reqs != 0 || exhausted || !resetAt.IsZero() {
		t.Fatalf("unknown model should be zero: reqs=%d exhausted=%v reset=%v", reqs, exhausted, resetAt)
	}
}
