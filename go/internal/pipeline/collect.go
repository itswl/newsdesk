// Package pipeline 串起一次完整的任务：采集 → 历史 → 模型 → 翻译 → 建站 → 自检 → 发布。
//
// 调度器和手动 `newsdesk run` 走的是同一条，不存在「手动能跑、定时跑不起来」
// 这种只差一层的坑。
package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/fetch"
	"github.com/itswl/newsdesk/internal/history"
	"github.com/itswl/newsdesk/internal/store"
)

// source 任务 key → 数据源目录名。
var source = map[string]string{
	"ai": "ainews", "douban": "douban", "trending": "trending", "momoyu": "momoyu",
}

// Collect 采集这个任务需要的数据并落盘。
//
// 所有联网都在这一层。分析层跑在沙箱里出不了网，这是两个引擎可互换的前提——
// 没有联网能力的引擎会转而用 shell 去本地翻文件硬凑答案，答案碰巧对但来源
// 完全不可控。
func Collect(ctx context.Context, key, date string, cfg *config.Config, st *store.Store) error {
	c := fetch.NewClient(30 * time.Second)
	switch key {
	case "ai":
		return fetch.AINews(ctx, c, st, date, fetch.AIOpts{
			Hours:   cfg.Int("AINEWS_HOURS", 36),
			PerFeed: cfg.Int("AINEWS_PER_FEED", 12),
		})
	case "douban":
		return fetch.Douban(ctx, c, st, date, fetch.DoubanOpts{
			Tag:   cfg.Get("DOUBAN_TAG", "热门"),
			N:     cfg.Int("DOUBAN_N", 10),
			Sort:  cfg.Get("DOUBAN_SORT", "recommend"),
			Pause: 400 * time.Millisecond,
		})
	case "trending":
		gh := fetch.NewGitHub(c, githubToken(ctx, cfg))
		return fetch.Trending(ctx, c, gh, st, date, cfg.Get("TRENDING_TIER", "t3"))
	case "momoyu":
		return fetch.Momoyu(ctx, c, st, date)
	}
	return fmt.Errorf("未知任务 %q", key)
}

// History 生成跨期比对。没有历史不是错误——首期本来就没有。
func History(key, date string, st *store.Store) error {
	src, ok := source[key]
	if !ok {
		return fmt.Errorf("未知任务 %q", key)
	}
	return history.Build(st, src, date)
}
