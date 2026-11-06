// Package pipeline 串起一次完整的任务：采集 → 历史 → 模型 → 翻译 → 建站 → 自检 → 发布。
//
// 调度器和手动 `newsdesk run` 走的是同一条，不存在「手动能跑、定时跑不起来」
// 这种只差一层的坑。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
//
// out 是这一档的任务日志：每个源的抓取结果要落在那里，不能只进 scheduler.log。
func Collect(ctx context.Context, key, date string, cfg *config.Config, st *store.Store, out io.Writer) error {
	c := fetch.NewClient(30 * time.Second)
	c.Out = out
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

// Online 本机此刻能不能连上外网，见 fetch.Online。调度器用它分辨失败是不是因为断网。
func Online(ctx context.Context) bool { return fetch.Online(ctx) }

// History 生成跨期比对。没有历史不是错误——首期本来就没有。
func History(key, date string, st *store.Store, out io.Writer) error {
	src, ok := source[key]
	if !ok {
		return fmt.Errorf("未知任务 %q", key)
	}
	return history.Build(st, src, date, out)
}

// aiEntries 给 date 那天的 ai 条目按 digest 的顺序编号、判新旧（规则见 history.JudgeAI）。
func aiEntries(st *store.Store, date string) ([]history.Entry, fetch.AIDoc, error) {
	doc, err := fetch.LoadAIDoc(filepath.Join(st.Data("ainews", date), "items.json"))
	if err != nil {
		return nil, doc, err
	}
	items := fetch.AIOrder(doc)
	hs := make([]history.AIItem, len(items))
	for i, it := range items {
		hs[i] = history.AIItem{Source: it.Source, Tier: it.Tier, Title: it.Title, Link: it.Link, Date: it.Date}
	}
	return history.JudgeAI(hs, history.AIFirstSeen(st, date), date), doc, nil
}

// annotateAI 把新旧判定写进 digest.txt（覆盖采集时那份不带标签的）。在模型分析之前调，
// 跳过采集时也要调——digest 可能是旧代码写的。
func annotateAI(st *store.Store, date string, out io.Writer) error {
	es, doc, err := aiEntries(st, date)
	if err != nil {
		return err
	}
	path := filepath.Join(st.Data("ainews", date), "digest.txt")
	text := fetch.AIDigest(doc, func(id int) string { return es[id-1].Tag() })
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	var today, bg, rep, batch int
	batchDay := ""
	for _, e := range es {
		switch {
		case e.Today:
			today++
			if d, ok := strings.CutSuffix(e.Label, " 批次"); ok {
				batch, batchDay = batch+1, d
			}
		case e.Repeat():
			rep++
		default:
			bg++
		}
	}
	fmt.Fprintf(out, "  新旧判定 -> %s：今日 %d 条", st.RelTo(path), today)
	if batch > 0 {
		fmt.Fprintf(out, "（含 arXiv %s 批次 %d 条）", batchDay, batch)
	}
	fmt.Fprintf(out, "，背景 %d 条，前几天已出现 %d 条\n", bg, rep)
	return nil
}

// arrangeAI 把模型写好的 ai 报告排成定稿，原地改写。在模型分析之后、翻译之前调。
//
// 模型一个〔编号〕都没标就原样留着（兜底引擎不认这套写法时会这样）：硬排的话，它
// 所有条目都会堆进「今日增量」，它自己写的「来源」也会被换成空的，比不排更糟。
func arrangeAI(st *store.Store, date, path string, out io.Writer) error {
	es, doc, err := aiEntries(st, date)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	in := history.PrefaceInput{FetchedAt: doc.FetchedAt, WindowHours: doc.WindowHours, Items: len(doc.Items)}
	for _, s := range doc.Sources {
		switch {
		case s.Status != "OK":
			in.Failed = append(in.Failed, s.Source)
		case s.Kept == 0 && fetch.AITier(s.Source) == "一手·厂商公告":
			in.OK++
			in.QuietVendors = append(in.QuietVendors, s.Source)
		default:
			in.OK++
		}
	}
	var hist struct {
		Failed map[string][]string `json:"sources_failed"`
	}
	if b, err := os.ReadFile(filepath.Join(st.Data("ainews", date), "history.json")); err == nil {
		_ = json.Unmarshal(b, &hist)
	}
	in.FailedBefore = hist.Failed

	r := history.ArrangeAI(string(raw), es, date, history.AIPreface(in, es))
	if len(r.Cited) == 0 {
		fmt.Fprintln(out, "  ⚠ 排版跳过：报告里一个〔编号〕都没有，照模型写的原样发布")
		return nil
	}
	if err := writeAtomic(path, r.Text); err != nil {
		return err
	}
	rep := 0
	for id := range r.Cited {
		if es[id-1].Repeat() {
			rep++
		}
	}
	fmt.Fprintf(out, "  排版：今日增量 %d 条、背景 %d 条，引用 %d 条（其中前几天已出现 %d 条）",
		r.Today, r.BG, len(r.Cited), rep)
	if r.Moved > 0 {
		fmt.Fprintf(out, "；模型放错地方的 %d 条已按新旧改放", r.Moved)
	}
	fmt.Fprintln(out)
	if r.Uncited > 0 {
		fmt.Fprintf(out, "  ⚠ %d 条没标〔编号〕，查不了新旧，按模型放的位置放（没分的算今日增量）\n", r.Uncited)
	}
	if len(r.Unknown) > 0 {
		fmt.Fprintf(out, "  ⚠ digest 里没有的编号已删掉：%s\n", r.UnknownList())
	}
	if len(r.Freeform) > 0 {
		fmt.Fprintf(out, "  ⚠ 这几节用了自定的小标题，没按新旧重排：%s\n", strings.Join(r.Freeform, "、"))
	}
	if r.OwnSources {
		fmt.Fprintln(out, "  模型自己写的「来源」已换成按引用生成的")
	}
	return nil
}

// writeAtomic 先写临时文件再换名：排版写到一半出错，原报告还在。临时文件名不匹配
// 任务的报告模式（task.Files），中途被别的步骤扫到也不会当成报告。
func writeAtomic(path, text string) error {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".layout-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
