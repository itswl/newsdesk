package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/task"
)

// prompt 里的日期由执行器代入。让模型自己算日期会算错（时区、跨零点），
// 而算错的后果是它去读一个不存在的目录然后自己编内容。
func TestPromptSubstitutesDate(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "prompts"), 0o755)
	os.WriteFile(filepath.Join(root, "prompts", "douban.md"),
		[]byte("读 data/douban/<今天>/list.json，写 reports/<今天日期>/douban.md"), 0o644)
	got, err := Prompt(root, "douban", "2026-09-17")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "<今天") {
		t.Fatalf("占位没换掉：%s", got)
	}
	if !strings.Contains(got, "data/douban/2026-09-17/list.json") ||
		!strings.Contains(got, "reports/2026-09-17/douban.md") {
		t.Fatalf("日期代入不对：%s", got)
	}
}

func TestPromptUnknownTask(t *testing.T) {
	if _, err := Prompt(t.TempDir(), "nope", "2026-09-17"); err == nil {
		t.Fatal("未知任务应报错")
	}
}

// Trending 有正式稿和草稿两个候选，取最新的那份。
func TestReportPathPicksNewestCandidate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "reports", "2026-09-17")
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "github-trending_draft1.md")
	os.WriteFile(old, []byte("draft"), 0o644)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	final := filepath.Join(dir, "github-trending.md")
	os.WriteFile(final, []byte("final"), 0o644)

	tr, _ := task.Find("trending")
	got, ok := ReportPath(root, "2026-09-17", tr)
	if !ok || filepath.Base(got) != "github-trending.md" {
		t.Fatalf("应取最新的那份，得到 %s（ok=%v）", got, ok)
	}
}

// 空文件不算产出：模型「完成」了但没写出内容，当成成功会让整天静默缺一档。
func TestReportPathIgnoresEmptyFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "reports", "2026-09-17")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "douban.md"), nil, 0o644)
	db, _ := task.Find("douban")
	if _, ok := ReportPath(root, "2026-09-17", db); ok {
		t.Fatal("空文件不该算作产出")
	}
}

// 模型输出可以是几十万字，而失败分类只需要末尾那几行——
// 真正的报错总在最后。
func TestRingBufferKeepsTail(t *testing.T) {
	r := &ringBuffer{max: 16}
	r.Write([]byte("0123456789"))
	r.Write([]byte("abcdefghij"))
	if got := r.String(); got != "456789abcdefghij" {
		t.Fatalf("环形缓冲应只留末尾，得到 %q", got)
	}
}

func TestSecretValuesSkipsShortAndNonSecret(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.conf")
	os.WriteFile(p, []byte(strings.Join([]string{
		"GITHUB_TOKEN=none", // 太短，会命中一切
		"FALLBACK_AUTH_TOKEN=sk-abcdefghijklmnop",
		"SITE_URL=https://example.com", // 不是凭据
		"ALERT_WEBHOOK=https://hook.example.com/abcdefghijkl",
	}, "\n")), 0o644)
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := secretValues(c)
	if _, ok := got["GITHUB_TOKEN"]; ok {
		t.Fatal("none 这种短值会命中一切，必须剔掉")
	}
	if _, ok := got["SITE_URL"]; ok {
		t.Fatal("站点地址不是凭据")
	}
	for _, k := range []string{"FALLBACK_AUTH_TOKEN", "ALERT_WEBHOOK"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s 应当纳入自检", k)
		}
	}
}

func TestGithubTokenNoneMeansUnauthenticated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.conf")
	os.WriteFile(p, []byte("GITHUB_TOKEN=none\n"), 0o644)
	c, _ := config.Load(p)
	// 令牌是可选的，别让它变回必需：Trending 按 50 次调用设计，
	// 装得进未认证的 60 次/小时预算
	if got := githubToken(t.Context(), c); got != "" {
		t.Fatalf("显式填 none 应当强制不认证，得到 %q", got)
	}
}

// VAPID 的两把钥匙在同一个配置文件里，靠**名字**被自检区分开：
//
//	PUSH_VAPID_PRIVATE_KEY  含 KEY → 收进自检。它泄进产出就等于别人能冒名推送
//	PUSH_VAPID_PUBLIC       不含   → 不收。它按设计印在每一个页面上，
//	                                 一旦被当成凭据，**每次发布都会因为
//	                                 「产出里有凭据」而中止**，而且报错指向的是
//	                                 站点文件，很难一眼看出根因在命名
func TestSecretValuesTreatsVAPIDKeysByName(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.conf")
	os.WriteFile(p, []byte(strings.Join([]string{
		"PUSH_VAPID_PUBLIC=BEXAMPLE0123456789abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ012",
		"PUSH_VAPID_PRIVATE_KEY=MIGEXAMPLE0123456789abcdefghijklmnopqrstuvwxyz0123456789",
	}, "\n")), 0o644)
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := secretValues(c)
	if _, ok := got["PUSH_VAPID_PRIVATE_KEY"]; !ok {
		t.Error("私钥必须纳入发布前自检")
	}
	if _, ok := got["PUSH_VAPID_PUBLIC"]; ok {
		t.Error("公钥印在每个页面上，被当成凭据会让每次发布都中止")
	}
}
