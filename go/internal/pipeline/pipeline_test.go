package pipeline

import (
	"os"
	"path/filepath"
	"regexp"
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

// 临时目录的路径只在 ScratchDir 定义一次，prompt 里写占位，由执行器代入。
func TestPromptSubstitutesScratchDir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "prompts"), 0o755)
	os.WriteFile(filepath.Join(root, "prompts", "trending.md"),
		[]byte("脚本写进 <临时目录>/，报告写进 reports/<今天日期>/"), 0o644)
	got, err := Prompt(root, "trending", "2026-10-20")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "<临时目录>") || !strings.Contains(got, "脚本写进 tmp/trending/") {
		t.Fatalf("临时目录没代入：%s", got)
	}
}

// 仓库里真实的四份 prompt 都要把辅助脚本指到临时目录。
//
// 指错了不会报错：Write 写工作目录之外要审批，无人值守当场拒；在 Bash 里内联带
// 引号花括号的代码也拒。2026-10-20 那档 trending 两条路都撞过，最后把脚本写在了
// 仓库根上。占位没写的话，执行器建好、清好的目录模型根本不知道。
func TestRealPromptsUseScratchDir(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, key := range []string{"ai", "douban", "trending", "momoyu"} {
		got, err := Prompt(root, key, "2026-10-20")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, ScratchDir(key)+"/") {
			t.Errorf("%s 的 prompt 没把辅助脚本指到 %s/（写占位 <临时目录>）", key, ScratchDir(key))
		}
	}
}

func TestResetScratchStartsEmpty(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "tmp", "trending", "calc.py")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("print(1)"), 0o644)

	if err := resetScratch(root, "trending", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("上一轮留下的脚本应被清掉")
	}
	if fi, err := os.Stat(filepath.Join(root, "tmp", "trending")); err != nil || !fi.IsDir() {
		t.Fatalf("开跑前应建好空目录：%v", err)
	}
	if err := resetScratch(root, "trending", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "tmp", "trending")); !os.IsNotExist(err) {
		t.Fatalf("收尾后目录应不存在：%v", err)
	}
}

// 清理是沙箱外的进程在删东西，而分析层能在仓库里建符号链接。tmp 或 tmp/<任务>
// 被换成指向仓库外的链接时，只能摘掉链接本身，不能顺着删到目标里去。
func TestResetScratchDoesNotFollowSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	keep := filepath.Join(outside, "douban", "keep.txt")
	os.MkdirAll(filepath.Dir(keep), 0o755)
	os.WriteFile(keep, []byte("x"), 0o644)

	// tmp 本身是链接：tmp/douban 实际指向 outside/douban
	if err := os.Symlink(outside, filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := resetScratch(root, "douban", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("顺着 tmp 链接删到仓库外去了：%v", err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "tmp")); err != nil || !fi.IsDir() {
		t.Fatalf("tmp 应换成真目录：%v", err)
	}

	// 最后一级是链接
	link := filepath.Join(root, "tmp", "douban")
	os.RemoveAll(link)
	if err := os.Symlink(filepath.Join(outside, "douban"), link); err != nil {
		t.Fatal(err)
	}
	if err := resetScratch(root, "douban", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("顺着 tmp/douban 链接删到仓库外去了：%v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("链接本身应被摘掉：%v", err)
	}
}

// 流水线各步骤的输出必须走调用方给的 writer，不能直接打到标准输出。
//
// 守护进程下标准输出只落在 scheduler.log、还没有任务前缀，单档日志里就少了那一块，
// 而且不报错：翻译的用量、每个源的抓取结果、凭据自检的结论先后都这样丢过。新加的
// 打印照抄 fmt.Printf 的话，这里会拦下。
func TestStepsDoNotPrintToStdout(t *testing.T) {
	direct := regexp.MustCompile(`\bfmt\.Print(f|ln)?\(`)
	for _, pkg := range []string{"fetch", "history", "leak", "translate", "engine"} {
		files, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("找不到 internal/%s 的源文件：%v", pkg, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if direct.MatchString(line) {
					t.Errorf("%s:%d 直接打到标准输出，改用传进来的 writer：%s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
