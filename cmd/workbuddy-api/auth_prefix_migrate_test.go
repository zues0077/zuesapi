package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-api/internal/auth"
)

// writeAuth 在 dir 下放一个最小可用的 TraeWork 凭据文件（内容决定加载器能否解析）。
func writeAuth(t *testing.T, dir, name, uid string) string {
	t.Helper()
	fp := filepath.Join(dir, name)
	body := `{"auth":{"accessToken":"a-` + uid + `","refreshToken":"r"},"account":{"uid":"` + uid + `"}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatalf("写 %s: %v", name, err)
	}
	return fp
}

// TestMigrateAuthFilePrefixes 覆盖旧版 JSON 导入留下的孤儿文件：
// traework-<uid>.json 没有任何加载器会读（LoadTraeDir 只扫 trae-*.json），
// 用户侧现象是「导入返回成功、面板始终不显示该账号」。启动时改名自愈。
func TestMigrateAuthFilePrefixes(t *testing.T) {
	dir := t.TempDir()
	orph := writeAuth(t, dir, "traework-4197274137669852.json", "4197274137669852")

	migrateAuthFilePrefixes(dir)

	if _, err := os.Stat(orph); err == nil {
		t.Fatal("孤儿 traework-*.json 仍在，改名未发生")
	}
	got, err := auth.LoadTraeDir(dir)
	if err != nil {
		t.Fatalf("LoadTraeDir: %v", err)
	}
	if len(got) != 1 || got[0].UID != "4197274137669852" {
		t.Fatalf("改名后加载器仍读不到账号: %+v", got)
	}
	if got[0].Kind != "traework" {
		t.Fatalf("Kind 应由加载器归一化为 traework，实得 %q", got[0].Kind)
	}
	// 幂等：再跑一次不应报错、不应产生新文件。
	before := readDir(t, dir)
	migrateAuthFilePrefixes(dir)
	if after := readDir(t, dir); strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("二次执行改动了目录: %v → %v", before, after)
	}
}

// TestMigrateAuthFilePrefixes_NeverClobbers 目标同名文件已存在时不得覆盖：
// 同 uid 两份凭据并存说明用户导入过两次（或跨机器拷贝过），静默覆盖会丢真凭据。
func TestMigrateAuthFilePrefixes_NeverClobbers(t *testing.T) {
	dir := t.TempDir()
	writeAuth(t, dir, "trae-dup.json", "dup")                       // 正常账号（浏览器登录产物）
	orph := writeAuth(t, dir, "traework-dup.json", "dup-orphan-tok") // 孤儿

	migrateAuthFilePrefixes(dir)

	if _, err := os.Stat(orph); err != nil {
		t.Fatalf("孤儿文件被动过了（应原样保留让用户处置）: %v", err)
	}
	keep, err := os.ReadFile(filepath.Join(dir, "trae-dup.json"))
	if err != nil {
		t.Fatalf("正式凭据被覆盖: %v", err)
	}
	if !strings.Contains(string(keep), `"uid":"dup"`) {
		t.Fatalf("正式凭据内容被改写: %s", keep)
	}
}

// TestMigrateAuthFilePrefixes_IgnoresOtherChannels 确认改名表只作用于出过问题的前缀，
// 不会把 workbuddy/qoder 等正常文件卷进来。
func TestMigrateAuthFilePrefixes_IgnoresOtherChannels(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"workbuddy-u1.json", "workbuddyai-u2.json",
		"qodercn-u3.json", "glm-u4.json"} {
		writeAuth(t, dir, n, strings.TrimSuffix(n, ".json"))
	}
	before := readDir(t, dir)
	migrateAuthFilePrefixes(dir)
	if after := readDir(t, dir); strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("其它渠道文件被动过: %v → %v", before, after)
	}
}

func readDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}
