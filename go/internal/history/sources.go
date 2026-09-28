package history

import (
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

// ainews 按链接判重，让「只报增量」可核验而不是靠模型印象。
func ainews(st *store.Store, date, dd string) (map[string]any, error) {
	var cur aiItems
	loadJSON(filepath.Join(dd, "items.json"), &cur)

	seen := map[string]string{} // 归一化链接 -> 首次见到的日期
	for _, d := range daysBefore(st, "ainews", date, Window) {
		var past aiItems
		loadJSON(filepath.Join(d[1], "items.json"), &past)
		for _, it := range past.Items {
			k := NormLink(it.Link)
			if _, ok := seen[k]; !ok {
				seen[k] = d[0]
			}
		}
	}

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
	return map[string]any{
		"new_count":      newCount,
		"repeat_count":   len(rep),
		"repeat_items":   cut(rep, Cap), // 新条目不列——不在这份名单里的就是新的
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
	for _, d := range daysBefore(st, "momoyu", date, Window) {
		var past momoyuRaw
		loadJSON(filepath.Join(d[1], "raw.json"), &past)
		hist = append(hist, snap{d[0], momoyuTitles(past)})
	}

	persist := []map[string]any{}
	fresh := 0
	for k, v := range today {
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
		rec := map[string]any{"title": v.Title, "src": v.Src, "today_extra": v.Extra,
			"days_on_list": len(ds) + 1, "prev_days": cutStr(ds, 3)}
		if len(ds) > 0 {
			rec["prev_extra"] = firstExtra
			persist = append(persist, rec)
		} else {
			fresh++
		}
	}
	sort.SliceStable(persist, func(i, j int) bool {
		return persist[i]["days_on_list"].(int) > persist[j]["days_on_list"].(int)
	})

	dropped := []map[string]any{}
	if len(hist) > 0 {
		for k, v := range hist[0].m {
			// 掉榜项只留原本有热度值的：没热度值的源本来就分不出冷热，列出来是噪声
			if _, still := today[k]; !still && v.Extra != "" {
				dropped = append(dropped, map[string]any{
					"title": v.Title, "src": v.Src, "last_extra": v.Extra, "last_day": hist[0].day})
			}
		}
		sort.SliceStable(dropped, func(i, j int) bool {
			return dropped[i]["title"].(string) < dropped[j]["title"].(string)
		})
	}
	return map[string]any{
		"history_days": len(hist), "persistent": cut(persist, Cap),
		"persistent_total": len(persist), "new_today_count": fresh,
		"dropped_from_prev": cut(dropped, Cap),
	}, nil
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
		for _, h := range hist {
			for i, r := range h.repos {
				if r == x.Repo {
					if firstDay == "" {
						firstDay = h.day
					}
					if len(appear) < 4 {
						appear = append(appear, map[string]any{"date": h.day, "rank": i + 1})
					}
					break
				}
			}
		}
		prevDay := ""
		if len(hist) > 0 {
			prevDay = hist[0].day
		}
		rows = append(rows, map[string]any{
			"repo": x.Repo, "rank_today": x.Rank, "boards_seen": len(appear) + 1,
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
