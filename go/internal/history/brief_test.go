package history

import (
	"strings"
	"testing"
)

// 新旧判定：这些以前都写在 prompt 里让模型照做。
func TestJudgeAI(t *testing.T) {
	items := []AIItem{
		{Source: "OpenAI", Title: "今天的公告", Link: "https://example.com/today", Date: "2026-10-03T02:00+08:00"},
		{Source: "TechCrunch AI", Title: "旧闻", Link: "https://example.com/old?utm_source=rss", Date: "2026-10-01T20:00+08:00"},
		{Source: "TechCrunch AI", Title: "昨晚的新闻", Link: "https://example.com/last-night", Date: "2026-10-02T22:00+08:00"},
		{Source: "Simon Willison", Title: "没日期", Link: "https://example.com/undated"},
		{Source: "arXiv cs.AI", Title: "最新一批", Link: "https://arxiv.org/abs/1", Date: "2026-10-02T02:00+08:00"},
		{Source: "arXiv cs.AI", Title: "上一批", Link: "https://arxiv.org/abs/2", Date: "2026-10-01T02:00+08:00"},
		{Source: "HN Algolia", Title: "Ask HN: 二", Link: "https://news.ycombinator.com/item?id=2", Date: "2026-10-03T01:00+08:00"},
	}
	// AIFirstSeen 的结果：键是归一化链接
	seen := map[string]string{
		NormLink("https://example.com/old"):                "2026-10-01",
		NormLink("https://news.ycombinator.com/item?id=1"): "2026-10-02",
	}
	want := []string{"今日", "背景 · 10-01 已出现", "背景 · 10-02", "今日 · 日期不明", "今日 · 10-02 批次", "背景 · 10-01", "今日"}
	for i, e := range JudgeAI(items, seen, "2026-10-03") {
		if e.ID != i+1 || e.Tag() != want[i] {
			t.Errorf("%s：编号 %d、%q，想要 %d、%q", e.Title, e.ID, e.Tag(), i+1, want[i])
		}
	}
}

func testEntries() []Entry {
	mk := func(id int, src, title, link string, today bool, label string) Entry {
		return Entry{ID: id, AIItem: AIItem{Source: src, Title: title, Link: link}, Today: today, Label: label}
	}
	return []Entry{
		mk(1, "OpenAI", "今天的公告", "https://openai.example/a", true, ""),
		mk(2, "TechCrunch AI", "收购 [独家]", "https://tc.example/b", true, ""),
		mk(3, "The Verge AI", "昨晚的新闻", "https://verge.example/c", false, "10-02"),
		mk(4, "HN Algolia", "外链", "https://www.cnbc.example/d", false, "10-02 已出现"),
		mk(5, "arXiv cs.AI", "论文甲", "https://arxiv.example/e", true, "10-02 批次"),
		mk(6, "arXiv cs.AI", "论文乙", "https://arxiv.example/f", true, "10-02 批次"),
		mk(7, "Hacker News", "Ask HN: 帖子", "https://news.ycombinator.com/item?id=7", true, ""),
	}
}

// 排版：模型只标编号，分侧、出处、日期、「已出现」、口径、来源都由代码定。
func TestLayoutPutsEntriesWhereTheDataSays(t *testing.T) {
	model := `# AI 每日简报 · 2026-10-03

> **口径**：模型自己写的口径，应当被换掉。

## 模型与产品

### 今日增量

- **今天的公告** —— 正文。〔1〕
没缩进的续行。
- **引用了不存在的编号** —— 正文。〔99〕〔1〕

### 背景

- **收购** —— 模型放进了背景，但它是今天的。〔2〕
- **旧闻** —— 前几天已出现过。〔4 3〕
- 没标编号的一条。

## 研究

1. 论文甲 〔5〕
2. 昨晚的新闻〔3〕
3. 论文乙〔6〕

## 社区关注

### 自定的小标题

- 帖子〔7〕

## 串线索

| 时间 | 事件 |
|---|---|
| 10-03 | 公告〔1〕 |

## 来源

- 模型自己写的来源，应当被换掉
`
	r := ArrangeAI(model, testEntries(), "2026-10-03", "> **口径**：PREFACE")
	out := r.Text

	mustHave := []string{
		"# AI 每日简报 · 2026-10-03\n\n> **口径**：PREFACE",
		// 模型放进背景的今日条目被挪回今日增量，接着的没缩进续行并回那一项
		"### 今日增量\n\n- **今天的公告** —— 正文。（[OpenAI](https://openai.example/a)）\n  没缩进的续行。\n- **引用了不存在的编号** —— 正文。（[OpenAI](https://openai.example/a)）\n- **收购** —— 模型放进了背景，但它是今天的。（[TechCrunch](https://tc.example/b)）\n\n### 背景",
		// 两条不同说明的出处分开写，「已出现」与日期由代码补
		"### 背景\n\n- **旧闻** —— 前几天已出现过。（[The Verge](https://verge.example/c)，10-02；[cnbc.example（经 HN）](https://www.cnbc.example/d)，10-02 已出现）\n- 没标编号的一条。\n\n## 研究",
		// 有序列表挪动后重新编号；arXiv 最新一批进今日并注明批次
		"## 研究\n\n### 今日增量\n\n1. 论文甲（[arXiv](https://arxiv.example/e)，10-02 批次）\n2. 论文乙（[arXiv](https://arxiv.example/f)，10-02 批次）\n\n### 背景\n\n1. 昨晚的新闻（[The Verge](https://verge.example/c)，10-02）",
		// 自定小标题的节原样保留顺序，只换引用
		"## 社区关注\n\n### 自定的小标题\n\n- 帖子（[HN](https://news.ycombinator.com/item?id=7)）",
		"| 10-03 | 公告（[OpenAI](https://openai.example/a)） |",
		// 来源按引用生成、按编号排；标题里的方括号换成全角
		"## 来源\n\n- OpenAI：[今天的公告](https://openai.example/a)\n- TechCrunch：[收购 ［独家］](https://tc.example/b)\n- The Verge：[昨晚的新闻](https://verge.example/c)\n- cnbc.example（经 HN）：[外链](https://www.cnbc.example/d)\n- arXiv：[论文甲](https://arxiv.example/e)\n- arXiv：[论文乙](https://arxiv.example/f)\n- HN：[Ask HN: 帖子](https://news.ycombinator.com/item?id=7)\n",
	}
	for _, s := range mustHave {
		if !strings.Contains(out, s) {
			t.Errorf("排版结果里缺：\n%s\n\n全文：\n%s", s, out)
		}
	}
	for _, s := range []string{"模型自己写的口径", "模型自己写的来源", "〔"} {
		if strings.Contains(out, s) {
			t.Errorf("排版结果里不该还有 %q", s)
		}
	}
	if r.Moved != 1 || r.Uncited != 1 || r.Today != 5 || r.BG != 3 {
		t.Errorf("moved=%d uncited=%d today=%d bg=%d，想要 1 1 5 3", r.Moved, r.Uncited, r.Today, r.BG)
	}
	if r.UnknownList() != "〔99〕" || !r.OwnSources || len(r.Freeform) != 1 {
		t.Errorf("unknown=%s ownSources=%v freeform=%v", r.UnknownList(), r.OwnSources, r.Freeform)
	}
}

// 子项：比第一项缩进深的列表项归上一项，空行隔开也算（站点渲染器同样这么认）。
// 组名本身没标编号时，子项各按新旧分开放，两边都带上组名。
//
// ⚠️ 11-05 试跑时，「开源与本地模型：」下面缩进两格的六条子项被当成了顶层条目，
// 挪进背景的那几条挂到了别的条目下面。
func TestLayoutKeepsKidsWithTheirParent(t *testing.T) {
	es := append(testEntries(), Entry{ID: 8, AIItem: AIItem{Source: "HN Algolia", Title: "今天的公告",
		Link: "https://openai.example/a/?utm_source=hn"}, Today: true})
	model := `# 标题

## 模型与产品

- 开源与本地模型：
  - 今日的一条〔1〕
  - 昨晚的一条〔3〕

  - 空行隔开的子项，仍归这一组〔2〕
- 有自己编号的项〔3〕
  - 它的子项跟着它走〔1〕
- 同一篇两处都有〔8 1〕
`
	r := ArrangeAI(model, es, "2026-10-03", "> P")
	for _, want := range []string{
		"### 今日增量\n\n- 开源与本地模型：\n  - 今日的一条（[OpenAI](https://openai.example/a)）\n  - 空行隔开的子项，仍归这一组（[TechCrunch](https://tc.example/b)）\n- 有自己编号的项（[The Verge](https://verge.example/c)，10-02）\n  - 它的子项跟着它走（[OpenAI](https://openai.example/a)）\n- 同一篇两处都有（[OpenAI](https://openai.example/a)）",
		"### 背景\n\n- 开源与本地模型：\n  - 昨晚的一条（[The Verge](https://verge.example/c)，10-02）",
	} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("排版结果里缺：\n%s\n\n全文：\n%s", want, r.Text)
		}
	}
	if n := strings.Count(r.Text, "](https://openai.example/a"); n != 4 {
		t.Errorf("同一篇在来源与引用里应当各只出现一次，共 4 处，实际 %d 处", n)
	}
	if r.Today != 4 || r.BG != 1 || r.Uncited != 0 {
		t.Errorf("today=%d bg=%d uncited=%d，想要 4 1 0", r.Today, r.BG, r.Uncited)
	}
}

// 一个编号都没标的报告，Cited 为空——调用方据此原样发布（兜底引擎不认这套写法时，
// 硬排比不排更糟：所有条目堆进今日增量，它自己写的「来源」被换成空的）。
func TestArrangeAIFindsNoCitations(t *testing.T) {
	r := ArrangeAI("# 标题\n\n## 模型与产品\n\n- 没有编号的正文\n\n## 来源\n\n- [x](https://example.com)\n",
		testEntries(), "2026-10-03", "> P")
	if len(r.Cited) != 0 {
		t.Errorf("Cited = %v，想要空", r.Cited)
	}
}

// 口径由数据生成：失败的源点名，安静的一手厂商源只说「本期无官方新公告」。
func TestAIPreface(t *testing.T) {
	es := []Entry{{ID: 1, Label: "10-02 已出现"}, {ID: 2, Today: true, Label: "10-02 批次"}}
	p := AIPreface(PrefaceInput{
		FetchedAt: "2026-10-03T06:30:00+08:00", WindowHours: 36, Items: 2, OK: 2,
		Failed: []string{"Ars Technica"}, QuietVendors: []string{"Anthropic"},
		FailedBefore: map[string][]string{"Import AI": {"2026-10-02", "2026-10-01"}},
	}, es)
	for _, s := range []string{
		"采集于 10-03 06:30（北京时间），时间窗 36 小时，窗内 2 条，其中 1 条前几天已出现。",
		"2 个源抓取成功，Ars Technica 抓取失败",
		"本期无官方新公告：Anthropic。",
		"近 7 天抓取失败过：Import AI（10-01、10-02）。",
		"最新一批（10-02）算今日",
	} {
		if !strings.Contains(p, s) {
			t.Errorf("口径里缺 %q：\n%s", s, p)
		}
	}
}
