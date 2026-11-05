package history

// ai 简报里能机械判定的部分从 prompt 挪进代码。判定与排版的纯逻辑在这里，读写文件和
// 调用时机在 pipeline（annotateAI / arrangeAI）。
//
// 一条新闻放「今日增量」还是「背景」、出处是哪条、哪天已经出现过、arXiv 是哪天的
// 批次——以前都写在 prompt 里让模型照做，照做得不稳：11-05 那档把标着 11-05 的
// Meta 收购 Conductor 放进了背景，把 11-04 的 arXiv 批次放进今日增量、也没写日期；
// 「X 日已出现」则一直照着一份有错的判重名单写（见 NormLink）。现在的分工：
//
//   - 分析前：按 digest 的顺序给每条编号、判新旧（JudgeAI），写进 digest.txt；
//   - 模型只写正文，每条末尾标〔编号〕；
//   - 分析后：按编号把条目分进「今日增量」「背景」，把编号换成出处链接并补上日期与
//     「X 日已出现」，开头的口径与结尾的「来源」从数据生成（ArrangeAI、AIPreface）。
//
// 判定只在 JudgeAI 一处，两头都用它的结果，必然一致。

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AIItem 判新旧要用到的条目字段（items.json 里的一条）。
type AIItem struct {
	Source, Tier, Title, Link, Date string
}

// Entry 是 digest 里的一条，带程序判好的新旧。
type Entry struct {
	ID int // digest 里的〔编号〕，从 1 起
	AIItem
	Today bool   // 进「今日增量」
	Label string // 出处后面跟的说明：今日的为空，其余如「11-04」「11-04 已出现」「11-04 批次」
}

// Tag 是 digest 里【】的写法。
func (e Entry) Tag() string {
	switch {
	case e.Today && e.Label != "":
		return "今日 · " + e.Label
	case e.Today:
		return "今日"
	}
	return "背景 · " + e.Label
}

// Repeat 近 7 天已经抓到过。
func (e Entry) Repeat() bool { return strings.HasSuffix(e.Label, " 已出现") }

const arxiv = "arXiv"

// JudgeAI 给按 digest 顺序排好的条目编号、判新旧。seen 是 AIFirstSeen 的结果。
//
// 规则（与 prompt 原先要求模型做的一致，只是改由这里判）：
//   - 近 7 天抓到过的链接 → 背景，「X 日已出现」，X 是窗内第一次出现那天；
//   - arXiv 最新一批 → 今日。它每天发一批，06:30 开跑时最新一批总标着前一天，
//     按日期规则会天天进背景；批次日期早于今天就注明「X 日批次」；
//   - 其余按条目自带日期：今天的 → 今日，更早的 → 背景并写日期；
//   - 没有日期的 → 今日，注明「日期不明」。宁可让读者看见，也不替它编一个新旧。
func JudgeAI(items []AIItem, seen map[string]string, date string) []Entry {
	batch := ""
	for _, it := range items {
		if d := dayOf(it.Date); strings.HasPrefix(it.Source, arxiv) && d > batch {
			batch = d
		}
	}
	es := make([]Entry, len(items))
	for i, it := range items {
		e := Entry{ID: i + 1, AIItem: it}
		day := dayOf(it.Date)
		first, rep := seen[NormLink(it.Link)]
		switch {
		case rep:
			e.Label = mmdd(first) + " 已出现"
		case day == "":
			e.Today, e.Label = true, "日期不明"
		case strings.HasPrefix(it.Source, arxiv) && day == batch:
			e.Today = true
			if day < date {
				e.Label = mmdd(day) + " 批次"
			}
		case day < date:
			e.Label = mmdd(day)
		default:
			e.Today = true
		}
		es[i] = e
	}
	return es
}

var reDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// dayOf 条目日期是哪一天。采集层已把日期换成本地（北京）时间，前 10 位就是。
func dayOf(s string) string { return reDay.FindString(s) }

func mmdd(day string) string {
	if len(day) >= 10 {
		return day[5:10]
	}
	return day
}

type Arrangement struct {
	Text       string
	Today, BG  int // 分进两侧的条目数
	Moved      int // 模型自己分了、但分错了侧的
	Uncited    int // 没标编号的条目
	Cited      map[int]bool
	Unknown    []int
	Freeform   []string // 有自定三级标题、没重排的节
	OwnSources bool     // 模型自己写了「来源」
}

// UnknownList 不存在的编号，写日志用。
func (r *Arrangement) UnknownList() string {
	var b strings.Builder
	seen := map[int]bool{}
	for _, n := range r.Unknown {
		if !seen[n] {
			seen[n] = true
			fmt.Fprintf(&b, "〔%d〕", n)
		}
	}
	return b.String()
}

var (
	reAnyItem = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,3}[.)])\s+`)
	reOrdered = regexp.MustCompile(`^( {0,3})\d{1,3}([.)]\s+)`)
	// 一串紧挨着的〔编号〕算一处引用：〔3〕〔17〕、〔3 17〕、〔3、17〕都认。括号里只许有
	// 数字和分隔符，免得把正文里别的〔〕当成引用吞掉。
	reCite = regexp.MustCompile(`(?:〔\s*\d+(?:\s*[,，、;；]?\s*\d+)*\s*〕[ \t]*)+`)
	reNum  = regexp.MustCompile(`\d+`)
)

// ArrangeAI 把模型写好的 ai 报告排成定稿（src 是原文，preface 是 AIPreface 生成的口径）。
func ArrangeAI(src string, es []Entry, date, preface string) Arrangement {
	r := Arrangement{Cited: map[int]bool{}}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	title := "# AI 每日简报 · " + date
	if i < len(lines) && strings.HasPrefix(lines[i], "# ") {
		title = strings.TrimSpace(lines[i])
		i++
	}
	// 标题与第一节之间：模型自己写的口径（引用块）不要，口径由 preface 生成；别的留着
	var pre []string
	for ; i < len(lines) && !strings.HasPrefix(lines[i], "## "); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
			pre = append(pre, lines[i])
		}
	}
	type section struct {
		name  string
		lines []string
	}
	var secs []section
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			secs = append(secs, section{name: strings.TrimSpace(lines[i][3:])})
		} else {
			secs[len(secs)-1].lines = append(secs[len(secs)-1].lines, lines[i])
		}
	}

	var b strings.Builder
	b.WriteString(title + "\n\n" + preface)
	if p := strings.TrimSpace(strings.Join(pre, "\n")); p != "" {
		t, _ := r.cite(p, es)
		b.WriteString("\n\n" + t)
	}
	for _, s := range secs {
		switch {
		case strings.Contains(s.name, "来源"):
			r.OwnSources = true
			continue
		case strings.Contains(s.name, "口径"):
			continue
		case strings.Contains(s.name, "串线索"):
			t, _ := r.cite(strings.Join(s.lines, "\n"), es)
			b.WriteString("\n\n## " + s.name + "\n\n" + strings.TrimSpace(t))
			continue
		}
		b.WriteString("\n\n## " + s.name + "\n\n" + r.theme(s.name, s.lines, es))
	}
	b.WriteString("\n\n## 来源\n\n")
	ids := make([]int, 0, len(r.Cited))
	for id := range r.Cited {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	listed := map[string]bool{} // 同一篇在 HN 首页和 Algolia 里各一条，来源只列一次
	for _, id := range ids {
		e := es[id-1]
		if k := NormLink(e.Link); !listed[k] {
			listed[k] = true
			fmt.Fprintf(&b, "- %s：[%s](%s)\n", display(e), linkText(e.Title), href(e.Link))
		}
	}
	r.Text = b.String()
	return r
}

type block struct {
	lines []string
	item  bool
	side  int // 模型自己把它放在哪：0 没分，1 今日增量，2 背景
}

// theme 排一个主题节：段落留在前面，列表项按引用分进「今日增量」「背景」。
//
// 切块的规则跟站点渲染器（internal/markdown 的 list）一致：比这一节第一项缩进更深的
// 列表项是上一项的子项，中间隔着空行也是；列表项只吃缩进的续行，空行之后不是列表项
// 就另起一段。紧跟在列表项后面、没缩进的那行，渲染器会把它当成另起一段，这里替它补上
// 缩进、并回那一项——模型几乎总是这个意思。
func (r *Arrangement) theme(name string, lines []string, es []Entry) string {
	var blocks []*block
	var cur, top *block // cur 正在接续的块；top 最近的顶层列表项，子项挂在它下面
	side, free, base, blank := 0, false, -1, false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "### ") {
			switch h := strings.TrimSpace(ln[4:]); {
			case strings.Contains(h, "今日"):
				side = 1
			case strings.Contains(h, "背景"):
				side = 2
			default:
				free = true
				blocks = append(blocks, &block{lines: []string{ln}})
			}
			cur, top, blank = nil, nil, false
			continue
		}
		if t == "" {
			blank = true
			continue
		}
		ind, item := indentOf(ln), reAnyItem.MatchString(ln)
		switch {
		case item && top != nil && ind > base:
			if blank {
				top.lines = append(top.lines, "")
			}
			top.lines = append(top.lines, ln)
			cur = top
		case item:
			if base < 0 || ind < base {
				base = ind
			}
			cur = &block{lines: []string{ln}, item: true, side: side}
			top = cur
			blocks = append(blocks, cur)
		case !blank && cur != nil && cur.item:
			if ind == 0 {
				ln = "  " + ln
			}
			cur.lines = append(cur.lines, ln)
		case !blank && cur != nil:
			cur.lines = append(cur.lines, ln)
		default:
			cur, top = &block{lines: []string{ln}}, nil
			blocks = append(blocks, cur)
		}
		blank = false
	}

	if free {
		r.Freeform = append(r.Freeform, name)
		var parts []string
		for _, bl := range blocks {
			t, _ := r.cite(strings.Join(bl.lines, "\n"), es)
			parts = append(parts, t)
		}
		return strings.Join(parts, "\n\n")
	}

	var paras, today, bg []string
	for _, bl := range blocks {
		if !bl.item {
			t, _ := r.cite(strings.Join(bl.lines, "\n"), es)
			paras = append(paras, t)
			continue
		}
		head, kids := splitKids(bl.lines)
		if len(kids) == 0 || reCite.MatchString(strings.Join(head, "\n")) {
			t, ids := r.cite(strings.Join(bl.lines, "\n"), es)
			if r.place(ids, bl.side, es) {
				today = append(today, t)
			} else {
				bg = append(bg, t)
			}
			continue
		}
		// 组名本身没标编号、下面挂着子项：子项各按自己的新旧分开放，两边都带上组名
		h, _ := r.cite(strings.Join(head, "\n"), es)
		var tk, bk []string
		for _, k := range kids {
			t, ids := r.cite(strings.Join(k, "\n"), es)
			if r.place(ids, bl.side, es) {
				tk = append(tk, t)
			} else {
				bk = append(bk, t)
			}
		}
		if len(tk) > 0 {
			today = append(today, h+"\n"+strings.Join(tk, "\n"))
		}
		if len(bk) > 0 {
			bg = append(bg, h+"\n"+strings.Join(bk, "\n"))
		}
	}
	var parts []string
	if len(paras) > 0 {
		parts = append(parts, strings.Join(paras, "\n\n"))
	}
	if len(today) > 0 {
		parts = append(parts, "### 今日增量\n\n"+strings.Join(renumber(today), "\n"))
	}
	if len(bg) > 0 {
		parts = append(parts, "### 背景\n\n"+strings.Join(renumber(bg), "\n"))
	}
	if len(parts) == 0 {
		return "本期无。"
	}
	return strings.Join(parts, "\n\n")
}

// place 判一项进哪边（true = 今日增量），顺带记账。引用的条目里有一条是今日的就算今日。
// 没标编号就查不了新旧：模型自己分了侧就照它的，没分的算今日增量。
func (r *Arrangement) place(ids []int, side int, es []Entry) bool {
	today := side != 2
	if len(ids) == 0 {
		r.Uncited++
	} else {
		today = false
		for _, id := range ids {
			if es[id-1].Today {
				today = true
			}
		}
		if side != 0 && (side == 1) != today {
			r.Moved++
		}
	}
	if today {
		r.Today++
	} else {
		r.BG++
	}
	return today
}

// splitKids 把一个列表项拆成本身（首行与续行）和它下面的子项（每个子项带着自己的续行与更深的子项）。
func splitKids(lines []string) (head []string, kids [][]string) {
	base, kid := indentOf(lines[0]), -1
	for _, ln := range lines {
		ind, item := indentOf(ln), reAnyItem.MatchString(ln)
		switch {
		case item && ind > base && (kid < 0 || ind <= kid):
			kid = ind
			kids = append(kids, []string{ln})
		case len(kids) > 0:
			kids[len(kids)-1] = append(kids[len(kids)-1], ln)
		default:
			head = append(head, ln)
		}
	}
	for i, k := range kids { // 子项之间的空行留在前一个子项末尾没有意义，去掉
		for len(k) > 0 && strings.TrimSpace(k[len(k)-1]) == "" {
			k = k[:len(k)-1]
		}
		kids[i] = k
	}
	return head, kids
}

func indentOf(s string) int {
	n := 0
	for _, c := range s {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// renumber 有序列表项挪动之后重新编号。站点渲染器不认起始号，但 Markdown 原文也给人看。
func renumber(items []string) []string {
	n := 0
	for i, it := range items {
		if m := reOrdered.FindStringSubmatchIndex(it); m != nil {
			n++
			items[i] = it[:m[3]] + strconv.Itoa(n) + it[m[4]:]
		}
	}
	return items
}

// cite 把〔编号〕换成出处，返回这段文字引用的条目。
func (r *Arrangement) cite(s string, es []Entry) (string, []int) {
	var ids []int
	out := reCite.ReplaceAllStringFunc(s, func(m string) string {
		trail := m[len(strings.TrimRight(m, " \t")):]
		var all []int
		for _, n := range reNum.FindAllString(m, -1) {
			id, err := strconv.Atoi(n)
			if err != nil || id < 1 || id > len(es) {
				r.Unknown = append(r.Unknown, id)
				continue
			}
			if !slices.Contains(all, id) {
				all = append(all, id)
			}
		}
		if len(all) == 0 {
			return trail
		}
		// 按编号排：编号是 digest 的顺序，一手源在前、HN Algolia 在后，同一篇两处都有时
		// 留下的是原站那条。显示按链接去重；判新旧（ids）用全部引用。
		sort.Ints(all)
		var grp []int
		shown := map[string]bool{}
		for _, id := range all {
			r.Cited[id] = true
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
			if k := NormLink(es[id-1].Link); !shown[k] {
				shown[k] = true
				grp = append(grp, id)
			}
		}
		return "（" + citeText(grp, es) + "）" + trail
	})
	// 编号前面那个空格：中文里全角括号前不留空
	return strings.ReplaceAll(out, " （[", "（["), ids
}

// citeText 一处引用的写法：同一说明的出处并在一起，说明只写一次。
// 如「[The Verge](…)、[MIT Tech Review](…)；[cnbc.com（经 HN）](…)，11-04」。
func citeText(ids []int, es []Entry) string {
	var parts, run []string
	label := ""
	flush := func() {
		p := strings.Join(run, "、")
		if label != "" {
			p += "，" + label
		}
		parts, run = append(parts, p), nil
	}
	for i, id := range ids {
		e := es[id-1]
		if i > 0 && e.Label != label {
			flush()
		}
		label = e.Label
		run = append(run, "["+display(e)+"]("+href(e.Link)+")")
	}
	flush()
	return strings.Join(parts, "；")
}

// display 出处显示成谁。HN 上转的外链显示原站，不显示「HN」——读者要知道话是谁说的。
func display(e Entry) string {
	switch {
	case e.Source == "Hacker News" || e.Source == "HN Algolia":
		if h := host(e.Link); h != "" && h != "news.ycombinator.com" {
			return h + "（经 HN）"
		}
		return "HN"
	case strings.HasPrefix(e.Source, arxiv):
		return arxiv
	}
	return strings.TrimSuffix(e.Source, " AI")
}

func host(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// href 链接里的空格和圆括号会提前结束 Markdown 链接，转义掉。
func href(link string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29").Replace(strings.TrimSpace(link))
}

// linkText 标题里的方括号会提前结束链接文字，换成全角。
func linkText(t string) string {
	return strings.NewReplacer("[", "［", "]", "］").Replace(strings.Join(strings.Fields(t), " "))
}

// PrefaceInput 是 AIPreface 的输入，都来自 items.json 与 history.json。
type PrefaceInput struct {
	FetchedAt    string // RFC3339
	WindowHours  int
	Items        int
	OK           int                 // 抓取成功的源
	Failed       []string            // 这一档抓取失败的源
	QuietVendors []string            // 抓取成功、窗内一条没有的一手厂商源
	FailedBefore map[string][]string // history.json 的 sources_failed
}

// AIPreface 开头的口径。以前让模型写，prompt 里要交代一长串「失败的源怎么说、安静的源
// 别说成失败」，它照样写出过「Anthropic 官方源 6/7 天采集失败」；这些全是数据里现成的。
func AIPreface(in PrefaceInput, es []Entry) string {
	at := in.FetchedAt
	if t, err := time.Parse(time.RFC3339, in.FetchedAt); err == nil {
		at = t.Format("01-02 15:04")
	}
	rep, batch := 0, ""
	for _, e := range es {
		if e.Repeat() {
			rep++
		}
		if d, cut := strings.CutSuffix(e.Label, " 批次"); cut && e.Today {
			batch = d
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "> **口径**：采集于 %s（北京时间），时间窗 %d 小时，窗内 %d 条，其中 %d 条前几天已出现。",
		at, in.WindowHours, in.Items, rep)
	if len(in.Failed) == 0 {
		fmt.Fprintf(&b, "%d 个源全部抓取成功。", in.OK)
	} else {
		fmt.Fprintf(&b, "%d 个源抓取成功，%s 抓取失败，这部分本期没有覆盖。", in.OK, strings.Join(in.Failed, "、"))
	}
	if len(in.QuietVendors) > 0 {
		fmt.Fprintf(&b, "本期无官方新公告：%s。", strings.Join(in.QuietVendors, "、"))
	}
	if len(in.FailedBefore) > 0 {
		names := make([]string, 0, len(in.FailedBefore))
		for n := range in.FailedBefore {
			names = append(names, n)
		}
		sort.Strings(names)
		var parts []string
		for _, n := range names {
			days := slices.Clone(in.FailedBefore[n])
			sort.Strings(days)
			for i := range days {
				days[i] = mmdd(days[i])
			}
			parts = append(parts, n+"（"+strings.Join(days, "、")+"）")
		}
		fmt.Fprintf(&b, "近 7 天抓取失败过：%s。", strings.Join(parts, "；"))
	}
	b.WriteString("\n>\n> 「今日增量」「背景」、日期与「X 日已出现」由程序按条目日期和近 7 天的抓取记录判定")
	if batch != "" {
		fmt.Fprintf(&b, "；arXiv 每天一批，最新一批（%s）算今日", batch)
	}
	b.WriteString("。HN、Reddit 的分数只代表讨论热度，不代表已核实。")
	return b.String()
}
