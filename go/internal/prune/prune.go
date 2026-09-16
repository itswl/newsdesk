// Package prune 按保留期清理旧数据与日志。
//
// 保留期按「这东西坏了之后还查不查得着」来定，不是按体积：
//
//	原始抓取页  纯诊断用途，出问题当天就看了，留 7 天
//	任务日志    排查回溯窗口，留 30 天
//	采集数据    trending 快照是方法论基线，留 180 天；且最新一份永不删
//	报告与站点  每天几十 KB，全留——它们是重建站点的唯一来源
package prune

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	RawHTMLDays = 7
	LogDays     = 30
	DataDays    = 180
)

var reDate = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)

// olderThan 优先按路径里的日期判断，取不到才退回 mtime——
// 按 mtime 会把「重跑一次旧日期」的目录判成新的。
func olderThan(path string, days int) bool {
	if m := reDate.FindStringSubmatch(path); m != nil {
		if d, err := time.Parse("2006-01-02", m[1]); err == nil {
			return time.Since(d) > time.Duration(days)*24*time.Hour
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) > time.Duration(days)*24*time.Hour
}

type Result struct {
	Removed []string
	Freed   int64
}

func Run(root string, sources []string) Result {
	var r Result
	rm := func(p string) {
		if fi, err := os.Stat(p); err == nil {
			r.Freed += size(p, fi)
		}
		if os.RemoveAll(p) == nil {
			r.Removed = append(r.Removed, p)
		}
	}

	// 原始抓取页：诊断用，出问题当天就看了
	hits, _ := filepath.Glob(filepath.Join(root, "data", "trending", "*", "page_raw*.html"))
	for _, f := range hits {
		if olderThan(f, RawHTMLDays) {
			rm(f)
		}
	}
	// 任务日志
	logs, _ := filepath.Glob(filepath.Join(root, "logs", "*.log"))
	for _, f := range logs {
		if filepath.Base(f) == "scheduler.log" {
			continue // 长期追加的，按大小另行处理
		}
		if olderThan(f, LogDays) {
			rm(f)
		}
	}
	// 采集数据目录：**最新一份永不删**。
	// data/trending/<日期>/snap_t3.json 是次日增量的基线，删了方法论历史就断了。
	for _, src := range sources {
		dirs, _ := filepath.Glob(filepath.Join(root, "data", src, "*"))
		var days []string
		for _, d := range dirs {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() && reDate.MatchString(filepath.Base(d)) {
				days = append(days, d)
			}
		}
		sort.Strings(days)
		if len(days) <= 1 {
			continue
		}
		for _, d := range days[:len(days)-1] { // 跳过最新那一份
			if olderThan(d, DataDays) {
				rm(d)
			}
		}
	}
	return r
}

func size(p string, fi os.FileInfo) int64 {
	if !fi.IsDir() {
		return fi.Size()
	}
	var n int64
	filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if i, e := d.Info(); e == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

func (r Result) Print(root string) {
	if len(r.Removed) == 0 {
		fmt.Println("  无需清理")
		return
	}
	fmt.Printf("  清理 %d 项，释放 %.1f MB\n", len(r.Removed), float64(r.Freed)/(1<<20))
}
