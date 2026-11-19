package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 2026-11-19 回归锁：trimCurrentMonth 的快速路径方向曾写反（首行 <= cutTs 反而跳过裁剪），
// 导致有序文件永远不裁剪（实测 863 行里 459 行超 7 天却一行没删）。
// 这里锁三件事：超期必裁、窗口内必留、解析失败必留（防方向改对后清空异常文件）。

func probeUsageLine(ts int64) string {
	return fmt.Sprintf(
		`{"ts":%d,"ch":"probe","uid":"u","model":"m","pt":1,"ct":1,"src":"upstream"}`, ts)
}

func daysAgo(d int) int64 { return time.Now().AddDate(0, 0, -d).Unix() }

// runTrim 在临时目录写入 usage 当月分段，跑一次 New（内部触发 trimCurrentMonth），读回剩余行。
func runTrim(t *testing.T, lines []string) []string {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, monthFile("usage", time.Now()))
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// 超期条目必须被裁掉（有序文件：首行最老）。
func TestTrimCurrentMonth_DropsExpired(t *testing.T) {
	old := probeUsageLine(daysAgo(keepDays + 10))
	mid := probeUsageLine(daysAgo(keepDays - 10))
	fresh := probeUsageLine(daysAgo(1))

	got := runTrim(t, []string{old, mid, fresh})
	if len(got) != 2 {
		t.Fatalf("期望裁掉超期那条后剩 2 行，实际 %d 行：%v", len(got), got)
	}
	for _, l := range got {
		if l == old {
			t.Fatalf("首行为 %d 天前的超期条目，未被裁剪", keepDays+10)
		}
	}
}

// 全在保留窗口内的文件必须原样保留（走快速路径，不重写）。
func TestTrimCurrentMonth_KeepsAllWithinWindow(t *testing.T) {
	lines := []string{
		probeUsageLine(daysAgo(keepDays - 1)),
		probeUsageLine(daysAgo(1)),
	}
	got := runTrim(t, lines)
	if len(got) != 2 {
		t.Fatalf("窗口内条目被误删：期望 2 行，实际 %d 行", len(got))
	}
	for i, l := range got {
		if l != lines[i] {
			t.Fatalf("第 %d 行内容被改动", i+1)
		}
	}
}

// 取不到 ts 的条目必须保守保留：否则方向改对后，格式异常文件会被整份清空。
func TestTrimCurrentMonth_KeepsUnparseable(t *testing.T) {
	bad := `{"ch":"probe","uid":"u"}` // 无 ts 字段 → parseTs 返回 0
	got := runTrim(t, []string{
		bad,
		probeUsageLine(daysAgo(keepDays + 10)),
		probeUsageLine(daysAgo(1)),
	})
	if len(got) != 2 {
		t.Fatalf("期望裁掉 1 条超期、保留坏行+新行共 2 行，实际 %d 行：%v", len(got), got)
	}
	found := false
	for _, l := range got {
		if l == bad {
			found = true
		}
	}
	if !found {
		t.Fatal("无 ts 的条目被误删（解析失败必须保守保留）")
	}
}
