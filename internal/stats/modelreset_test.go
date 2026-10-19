package stats

import (
	"testing"
	"time"
)

// TestMarkModelRateLimitedSurfacesExhausted 验证反应式额度提示：撞限流记录重置点后，
// 今日模型行应带 Exhausted=true 与 ResetAt；且 canonical 键匹配能跨「带/不带倍率后缀」命中。
func TestMarkModelRateLimitedSurfacesExhausted(t *testing.T) {
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()

	// 客户端请求带倍率后缀的模型名记账。
	s.AddUsage("workbuddy/hy3 (0.00x)", 10, 20, 0, 100, 1.0, now)
	// 限流以裸名（无后缀）记录重置点，须能跨形态命中同一模型。
	reset := now.Add(2 * time.Hour)
	s.MarkModelRateLimited("workbuddy/hy3", reset, now)

	snap := s.Snapshot(now)
	if len(snap.Models) != 1 {
		t.Fatalf("want 1 model row, got %d", len(snap.Models))
	}
	m := snap.Models[0]
	if !m.Exhausted {
		t.Fatalf("model should be Exhausted after reset recorded, got %+v", m)
	}
	if m.ResetAt == "" {
		t.Fatalf("ResetAt should be set")
	}
}

// TestMarkModelRateLimitedIgnoresPast 过去时刻（已重置）不应记录，避免误报「已用尽」。
func TestMarkModelRateLimitedIgnoresPast(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	now := time.Now()
	s.AddUsage("workbuddy/hy3", 10, 20, 0, 100, 1.0, now)
	s.MarkModelRateLimited("workbuddy/hy3", now.Add(-time.Minute), now)

	snap := s.Snapshot(now)
	if snap.Models[0].Exhausted {
		t.Fatalf("past reset must not mark exhausted")
	}
}

// TestModelResetClearedOnRollDay 跨天清零：昨日的重置点不应污染新的一天。
func TestModelResetClearedOnRollDay(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	s.AddUsage("workbuddy/hy3", 10, 20, 0, 100, 1.0, day1)
	s.MarkModelRateLimited("workbuddy/hy3", day1.Add(3*time.Hour), day1)

	// 进入新的一天：模型消耗与重置点都清零，再记账同名模型时不应带旧重置点。
	day2 := day1.AddDate(0, 0, 1)
	s.AddUsage("workbuddy/hy3", 5, 5, 0, 100, 1.0, day2)
	snap := s.Snapshot(day2)
	if len(snap.Models) != 1 {
		t.Fatalf("want 1 model on new day, got %d", len(snap.Models))
	}
	if snap.Models[0].Exhausted {
		t.Fatalf("reset from previous day must be cleared on roll day")
	}
}
