package prune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mk(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("x"), 0o644)
	return p
}

func day(n int) string { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }

// 最新一份永不删：snap_t3.json 是次日增量的基线，删了方法论历史就断了。
func TestNewestDataDirIsNeverRemoved(t *testing.T) {
	root := t.TempDir()
	for _, n := range []int{500, 400, 300} { // 全都超过 180 天
		mk(t, root, filepath.Join("data", "trending", day(n), "snap_t3.json"))
	}
	Run(root, []string{"trending"})
	newest := filepath.Join(root, "data", "trending", day(300))
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("最新一份被删了: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "trending", day(500))); err == nil {
		t.Error("超期的应被删掉")
	}
}

func TestRecentDataKept(t *testing.T) {
	root := t.TempDir()
	for _, n := range []int{1, 2, 3} {
		mk(t, root, filepath.Join("data", "momoyu", day(n), "raw.json"))
	}
	Run(root, []string{"momoyu"})
	for _, n := range []int{1, 2, 3} {
		if _, err := os.Stat(filepath.Join(root, "data", "momoyu", day(n))); err != nil {
			t.Errorf("%d 天前的数据被误删", n)
		}
	}
}

func TestRawHTMLAndLogs(t *testing.T) {
	root := t.TempDir()
	old := mk(t, root, filepath.Join("data", "trending", day(30), "page_raw_t3.html"))
	fresh := mk(t, root, filepath.Join("data", "trending", day(1), "page_raw_t3.html"))
	oldLog := mk(t, root, filepath.Join("logs", day(60)+"_ai.log"))
	sched := mk(t, root, filepath.Join("logs", "scheduler.log"))
	os.Chtimes(sched, time.Now().AddDate(0, 0, -90), time.Now().AddDate(0, 0, -90))

	Run(root, nil)
	if _, err := os.Stat(old); err == nil {
		t.Error("超期的原始页应被删")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("近期的原始页不该删")
	}
	if _, err := os.Stat(oldLog); err == nil {
		t.Error("超期日志应被删")
	}
	// scheduler.log 是长期追加的，不按天删
	if _, err := os.Stat(sched); err != nil {
		t.Error("scheduler.log 不该按天删")
	}
}

// 报告与站点是重建的唯一来源，任何情况下都不该被碰。
func TestReportsAreNeverTouched(t *testing.T) {
	root := t.TempDir()
	r := mk(t, root, filepath.Join("reports", day(900), "ai-news.md"))
	s := mk(t, root, filepath.Join("site", "2020-01-01.html"))
	Run(root, []string{"trending", "momoyu", "douban", "ainews"})
	for _, p := range []string{r, s} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s 被删了——它是重建站点的唯一来源", p)
		}
	}
}
