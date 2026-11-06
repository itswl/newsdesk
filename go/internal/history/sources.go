package history

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/itswl/newsdesk/internal/store"
)

type aiItems struct {
	Items []struct {
		Title, Source, Link string
	} `json:"items"`
	Sources [][]any `json:"sources"`
}

// AIFirstSeen 是 ai 判重的唯一实现：归一化链接 -> 窗内最早见到它的日期。
// history.json 的 repeat_items 与报告排版（internal/brief）都用它，不能各写一份。
func AIFirstSeen(st *store.Store, date string) map[string]string {
	seen := map[string]string{}
	for _, d := range daysBefore(st, "ainews", date, Window) {
		var past aiItems
		loadJSON(filepath.Join(d[1], "items.json"), &past)
		for _, it := range past.Items {
			k := NormLink(it.Link)
			// daysBefore 由近到远，先到先得记下的是最近一次，不是 first_seen
			if f, ok := seen[k]; !ok || d[0] < f {
				seen[k] = d[0]
			}
		}
	}
	return seen
}

// ainews 按链接判重，让「只报增量」可核验而不是靠模型印象。
func ainews(st *store.Store, date, dd string) (map[string]any, error) {
	var cur aiItems
	loadJSON(filepath.Join(dd, "items.json"), &cur)

	seen := AIFirstSeen(st, date)

	newCount := 0
	rep := []map[string]any{}
	for _, it := range cur.Items {
		if first, ok := seen[NormLink(it.Link)]; ok {
			rep = append(rep, map[string]any{
				"title": it.Title, "source": it.Source, "first_seen": first})
		} else {
			newCount++
		}
	}

	// 源的两种「没东西」必须分开报：抓取失败是我们这边的问题，要在报告里说明；
	// 抓取成功但窗内没有新条目只是那个源这几天没发东西。以前两者混在一个
	// sources_with_gaps 里，模型写出「Anthropic 官方源 6/7 天采集失败」——
	// 实际那周每天都抓成功了，Anthropic 只是只发了一篇。读者被告知一手信源失效，
	// 而它只是安静。两个键都总是输出（可能为空对象），prompt 按空不空判断。
	failed := map[string][]string{}
	quiet := map[string][]string{}
	for _, d := range daysBefore(st, "ainews", date, Window) {
		var past aiItems
		loadJSON(filepath.Join(d[1], "items.json"), &past)
		for _, s := range past.Sources {
			if len(s) < 4 {
				continue
			}
			name, _ := s[0].(string)
			status, _ := s[1].(string)
			kept, _ := s[3].(float64)
			switch {
			case status != "OK":
				failed[name] = append(failed[name], d[0])
			case kept == 0:
				quiet[name] = append(quiet[name], d[0])
			}
		}
	}
	// repeat_items 全列，不截到 Cap：新条目不列，prompt 按「不在这份名单里的就是新的」
	// 用它，名单就是判定本身。以前和别的名单一样截到 12 条，截掉的重复就成了今日增量：
	// 10-21 到 11-05 有 10 天超过 12 条、漏列 73 条。窗内最多见过 29 条，全列也就几 KB。
	return map[string]any{
		"new_count":      newCount,
		"repeat_count":   len(rep),
		"repeat_items":   rep,
		"sources_failed": failed,
		"sources_quiet":  quiet,
	}, nil
}

type momoyuRaw struct {
	Data []struct {
		Data []struct {
			Name string `json:"name"`
			Data []struct {
				Title, Extra string
			} `json:"data"`
		} `json:"data"`
	} `json:"data"`
}

type mItem struct{ Title, Extra, Src string }

func momoyuTitles(raw momoyuRaw) map[string]mItem {
	out := map[string]mItem{}
	for _, c := range raw.Data {
		for _, s := range c.Data {
			for i, it := range s.Data {
				if i >= 15 {
					break
				}
				out[NormTitle(it.Title)] = mItem{it.Title, it.Extra, s.Name}
			}
		}
	}
	return out
}

// momoyu 话题在榜的连续性，比单日热度更能说明是真热还是一阵风。
func momoyu(st *store.Store, date, dd string) (map[string]any, error) {
	var cur momoyuRaw
	loadJSON(filepath.Join(dd, "raw.json"), &cur)
	today := momoyuTitles(cur)

	type snap struct {
		day string
		m   map[string]mItem
	}
	var hist []snap
	var prevDay string
	var prevRaw momoyuRaw
	for _, d := range daysBefore(st, "momoyu", date, Window) {
		var past momoyuRaw
		if loadJSON(filepath.Join(d[1], "raw.json"), &past) && prevDay == "" && len(past.Data) > 0 {
			prevDay, prevRaw = d[0], past
		}
		hist = append(hist, snap{d[0], momoyuTitles(past)})
	}

	// 持续在榜的分两份。能比冷热的（今昨两头都读得出热度值）全列，冷热由 heatTrend 判好；
	// 比不了的只按源汇总条数和最长天数——它们只能说「还在榜上」。
	//
	// ⚠️ 以前是一份名单按在榜天数排、截前 12 条：挂得最久的是掘金文章和豆瓣话题，大多
	// 没有热度值，真正能看冷热的知乎、微博条目通常只挂两三天，全被截掉了（11-06 那档模型
	// 原话：前 12 条里没有带热度值的知乎或微博条目，判断不了升温还是退烧）。天数相同的
	// 还按 map 的随机顺序排，截下来哪 12 条每次都可能不一样。
	type heatRec struct {
		rec   map[string]any
		ratio float64
	}
	type example struct {
		title string
		days  int
	}
	type quiet struct {
		count, longest int
		ex             []example
	}
	var heat []heatRec
	other := map[string]*quiet{}
	keys := make([]string, 0, len(today))
	for k := range today {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	total, fresh := 0, 0
	for _, k := range keys {
		v := today[k]
		var ds []string
		var firstExtra string
		for _, h := range hist {
			if p, ok := h.m[k]; ok {
				if len(ds) == 0 {
					firstExtra = p.Extra
				}
				ds = append(ds, h.day)
			}
		}
		if len(ds) == 0 {
			fresh++
			continue
		}
		total++
		days := len(ds) + 1
		if change, trend, ratio, ok := heatTrend(v.Src, firstExtra, v.Extra); ok {
			heat = append(heat, heatRec{map[string]any{"title": v.Title, "src": v.Src,
				"days_on_list": days, "prev_days": cutStr(ds, 3), "prev_extra": firstExtra,
				"today_extra": v.Extra, "change": change, "trend": trend}, ratio})
			continue
		}
		q := other[v.Src]
		if q == nil {
			q = &quiet{}
			other[v.Src] = q
		}
		q.count++
		q.longest = max(q.longest, days)
		q.ex = append(q.ex, example{v.Title, days})
	}
	// 知乎、微博在前（判得了升降），各自按变化幅度从涨到跌
	sort.SliceStable(heat, func(i, j int) bool {
		a, b := heat[i], heat[j]
		ia, ib := instantHeat[a.rec["src"].(string)], instantHeat[b.rec["src"].(string)]
		if ia != ib {
			return ia
		}
		if a.rec["src"] != b.rec["src"] {
			return a.rec["src"].(string) < b.rec["src"].(string)
		}
		return a.ratio > b.ratio
	})
	persist := make([]map[string]any, len(heat))
	for i, h := range heat {
		persist[i] = h.rec
	}
	others := map[string]any{}
	for src, q := range other {
		// 举例挑在榜最久的三条，同天数按标题
		sort.Slice(q.ex, func(i, j int) bool {
			if q.ex[i].days != q.ex[j].days {
				return q.ex[i].days > q.ex[j].days
			}
			return q.ex[i].title < q.ex[j].title
		})
		ex := []string{}
		for _, e := range q.ex[:min(3, len(q.ex))] {
			ex = append(ex, fmt.Sprintf("%s（%d 天）", e.title, e.days))
		}
		others[src] = map[string]any{"count": q.count, "longest_days": q.longest, "examples": ex}
	}

	// 掉榜：只收读得出热度值的（以前判的是「额外信息非空」，爱范儿的分类、值得买的商家、
	// 虎嗅的「50分钟前」都算了进去）。各源的热度不在一个量级（知乎「1374 万」、微博「115.3万」），
	// 混在一起按数排的话前 12 条全是知乎，所以每源各取掉榜前最热的几条：知乎、微博各 4 条，
	// 其余读得出数的源各 2 条。
	type drop struct {
		rec map[string]any
		val float64
	}
	bySrc := map[string][]drop{}
	nDrop := 0
	if len(hist) > 0 {
		for k, v := range hist[0].m {
			if _, still := today[k]; still {
				continue
			}
			if val, ok := heatValue(v.Extra); ok {
				bySrc[v.Src] = append(bySrc[v.Src], drop{map[string]any{
					"title": v.Title, "src": v.Src, "last_extra": v.Extra, "last_day": hist[0].day}, val})
				nDrop++
			}
		}
	}
	srcs := make([]string, 0, len(bySrc))
	for src := range bySrc {
		srcs = append(srcs, src)
	}
	sort.Slice(srcs, func(i, j int) bool {
		if instantHeat[srcs[i]] != instantHeat[srcs[j]] {
			return instantHeat[srcs[i]]
		}
		return srcs[i] < srcs[j]
	})
	dropped := []map[string]any{}
	for _, src := range srcs {
		ds := bySrc[src]
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].val != ds[j].val {
				return ds[i].val > ds[j].val
			}
			return ds[i].rec["title"].(string) < ds[j].rec["title"].(string)
		})
		k := 2
		if instantHeat[src] {
			k = 4
		}
		for _, d := range ds[:min(k, len(ds))] {
			dropped = append(dropped, d.rec)
		}
	}
	out := map[string]any{
		"history_days": len(hist), "persistent": persist, "persistent_other": others,
		"persistent_total": total, "new_today_count": fresh,
		"dropped_from_prev": dropped, "dropped_total": nDrop,
	}
	if prevDay != "" {
		out["freshness"] = momoyuFreshness(cur, prevRaw, prevDay)
	}
	return out, nil
}

// momoyuFreshness 各源比上一期新进几条，一条都没有的进 no_new。
//
// 源新不新要按内容判断。接口给的更新时刻靠不住：掘金的 create_time 2026-10-24、25、27
// 三天卡在 10-22，内容照常在换（每天 5–8 条新进）。榜首挂了几天也说明不了什么：掘金热榜
// 榜首从 10-22 起连挂 10 天，同期每天 4–12 条新进。以前 prompt 教模型「条目挂上 7、8 天
// 说明源本身没动」，报告就天天把掘金整源标成过期，新进的条目一起丢了。
//
// no_new 只说明条目没变，不等于内容相同：2026-11-01 豆瓣热话条目和顺序原样，40 条里 17 条
// 热度值在变。这里起初叫 unchanged，报告照字面写成「完全相同」，又引用同一个源的热度变化。
//
// 按全部条目算，不只前 15 条：排第 16 的挤进前 15 不算新进。
func momoyuFreshness(cur, prev momoyuRaw, prevDay string) map[string]any {
	before := momoyuSourceTitles(prev)
	sources := []map[string]any{}
	noNew := []string{}
	for _, c := range cur.Data {
		for _, s := range c.Data {
			now := map[string]bool{}
			for _, it := range s.Data {
				now[NormTitle(it.Title)] = true
			}
			// new 为 nil：上一期没有这个源，无从比较
			rec := map[string]any{"src": s.Name, "total": len(now), "new": nil}
			if p, ok := before[s.Name]; ok {
				n := 0
				for t := range now {
					if !p[t] {
						n++
					}
				}
				rec["new"] = n
				if n == 0 && len(now) > 0 {
					noNew = append(noNew, s.Name)
				}
			}
			sources = append(sources, rec)
		}
	}
	return map[string]any{"prev_day": prevDay, "sources": sources, "no_new": noNew}
}

func momoyuSourceTitles(raw momoyuRaw) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, c := range raw.Data {
		for _, s := range c.Data {
			m := out[s.Name]
			if m == nil {
				m = map[string]bool{}
				out[s.Name] = m
			}
			for _, it := range s.Data {
				m[NormTitle(it.Title)] = true
			}
		}
	}
	return out
}

type doubanSub struct{ ID, Title string }
type doubanDetail struct {
	Title  string `json:"title"`
	Rating struct {
		Value float64 `json:"value"`
		Count int     `json:"count"`
	} `json:"rating"`
}

func doubanDetails(dir string) map[string]doubanDetail {
	out := map[string]doubanDetail{}
	hits, _ := filepath.Glob(filepath.Join(dir, "detail", "*.json"))
	for _, f := range hits {
		var d doubanDetail
		if loadJSON(f, &d) {
			out[filepath.Base(f)[:len(filepath.Base(f))-5]] = d
		}
	}
	return out
}

// douban 评分会漂、**评价人数增速**才是真热度。
//
// ⚠️ 「评价数」取 rating.count，不是 comment_count——后者是短评数，
// 只有前者的三到六成，量级接近极易抄错。
func douban(st *store.Store, date, dd string) (map[string]any, error) {
	var curList []doubanSub
	loadJSON(filepath.Join(dd, "list.json"), &curList)
	curDetail := doubanDetails(dd)
	prev := daysBefore(st, "douban", date, Window)

	prevList := map[string]doubanSub{}
	if len(prev) > 0 {
		var pl []doubanSub
		loadJSON(filepath.Join(prev[0][1], "list.json"), &pl)
		for _, m := range pl {
			prevList[m.ID] = m
		}
	}
	// 由早到晚，记下最早一次的数值
	type firstRec struct {
		day string
		d   doubanDetail
	}
	first := map[string]firstRec{}
	for i := len(prev) - 1; i >= 0; i-- {
		for id, det := range doubanDetails(prev[i][1]) {
			if _, ok := first[id]; !ok {
				first[id] = firstRec{prev[i][0], det}
			}
		}
	}

	rows := []map[string]any{}
	entered := []string{}
	cur := map[string]bool{}
	for _, m := range curList {
		cur[m.ID] = true
		det := curDetail[m.ID]
		title := det.Title
		if title == "" {
			title = m.Title
		}
		row := map[string]any{"id": m.ID, "title": title,
			"rating": det.Rating.Value, "count": det.Rating.Count}
		if f, ok := first[m.ID]; ok {
			row["since"] = f.day
			row["rating_delta"] = round2(det.Rating.Value - f.d.Rating.Value)
			row["count_delta"] = det.Rating.Count - f.d.Rating.Count
		} else {
			entered = append(entered, title)
		}
		rows = append(rows, row)
	}
	left := []string{}
	for id, m := range prevList {
		if !cur[id] {
			left = append(left, m.Title)
		}
	}
	sort.Strings(left)
	return map[string]any{"history_days": len(prev), "movies": rows,
		"entered_since_prev": entered, "left_since_prev": left}, nil
}

type boardRow struct {
	Repo string `json:"repo"`
	Rank int    `json:"rank"`
}

// trending 在榜连续性与名次轨迹，补足单期基线看不到的东西。
func trending(st *store.Store, date, dd string) (map[string]any, error) {
	var board []boardRow
	loadJSON(filepath.Join(dd, "page.json"), &board)

	type snap struct {
		day   string
		repos []string
	}
	var hist []snap
	for _, d := range daysBefore(st, "trending", date, Window) {
		var b []boardRow
		loadJSON(filepath.Join(d[1], "page.json"), &b)
		rs := make([]string, len(b))
		for i, x := range b {
			rs[i] = x.Repo
		}
		hist = append(hist, snap{d[0], rs})
	}

	rows := []map[string]any{}
	onBoard := map[string]bool{}
	for _, x := range board {
		onBoard[x.Repo] = true
		var appear []map[string]any
		var firstDay string
		// 在榜期数与名次轨迹**分开数**。
		//
		// ⚠️ 这两件事曾经共用一个切片：rank_history 截断到 4 条，boards_seen 又拿
		// 截断后的长度算，于是「连续在榜 7 期」被写成 5——静默少报。实测后果是下一
		// 期的模型翻本期文件去核上一期「自 10-10 起连续 6 期 #1」的结论，核不上
		// （文件里最早只到 10-12），就把一条真结论当成「两期文件矛盾、不可复核」
		// 写进了公开报告。hist 本身已被 Window 截过，这里不再另设上限。
		seen := 0
		for _, h := range hist {
			for i, r := range h.repos {
				if r == x.Repo {
					if firstDay == "" {
						firstDay = h.day
					}
					seen++
					appear = append(appear, map[string]any{"date": h.day, "rank": i + 1})
					break
				}
			}
		}
		prevDay := ""
		if len(hist) > 0 {
			prevDay = hist[0].day
		}
		rows = append(rows, map[string]any{
			"repo": x.Repo, "rank_today": x.Rank, "boards_seen": seen + 1,
			"rank_history": appear,
			// returning：以前上过榜但上一期不在——「回锅」比「连续在榜」是不同的信号
			"returning": firstDay != "" && firstDay != prevDay,
		})
	}
	dropped := []string{}
	if len(hist) > 0 {
		for _, r := range hist[0].repos {
			if !onBoard[r] {
				dropped = append(dropped, r)
			}
		}
	}
	return map[string]any{"history_boards": len(hist), "repos": rows,
		"dropped_from_prev": dropped}, nil
}

func cut(s []map[string]any, n int) []map[string]any {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func cutStr(s []string, n int) []string {
	if s == nil {
		return []string{}
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
