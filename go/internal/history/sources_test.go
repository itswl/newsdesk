package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/itswl/newsdesk/internal/store"
)

// 抓取失败与「抓到了但窗内没新条目」必须是两个名单。混在一起的话模型会把
// 一个只是几天没发东西的一手源写成「采集失败」——实测就这么写过。
func TestSourceGapsSplitFailedFromQuiet(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	write := func(day, content string) {
		d := filepath.Join(root, "data", "ainews", day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "items.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-09-20", `{"items":[],"sources":[["A","FAIL",0,0],["B","OK",12,0],["C","OK",10,3]]}`)
	write("2026-09-21", `{"items":[],"sources":[["A","FAIL",0,0],["B","OK",12,0],["C","OK",10,0]]}`)
	write("2026-09-22", `{"items":[],"sources":[]}`)

	out, err := ainews(st, "2026-09-22", filepath.Join(root, "data", "ainews", "2026-09-22"))
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := out["sources_failed"].(map[string][]string)
	quiet, _ := out["sources_quiet"].(map[string][]string)
	// daysBefore 由近到远
	if want := map[string][]string{"A": {"2026-09-21", "2026-09-20"}}; !reflect.DeepEqual(failed, want) {
		t.Errorf("sources_failed = %v，想要 %v", failed, want)
	}
	if want := map[string][]string{"B": {"2026-09-21", "2026-09-20"}, "C": {"2026-09-21"}}; !reflect.DeepEqual(quiet, want) {
		t.Errorf("sources_quiet = %v，想要 %v", quiet, want)
	}
	if _, ok := out["sources_with_gaps"]; ok {
		t.Error("旧的混合名单不该再输出")
	}
}

// 判重的键：同一篇内容要归到一起，不同的内容不能归到一起。
//
// ⚠️ 后一半以前是坏的：`?` 之后整段剥掉，HN 的帖子、YouTube 视频各自塌成一个链接，
// 窗内第一帖之后的新帖全被判成「X 日已出现」。
func TestNormLink(t *testing.T) {
	same := [][2]string{
		{"HTTPS://WWW.Example.com/Post/", "https://example.com/post"},
		{"https://example.com/a?utm_source=rss&utm_medium=feed", "https://example.com/a"},
		{"https://example.com/a?id=3&utm_campaign=x&fbclid=0123", "https://example.com/a?id=3"},
		{"https://example.com/a?b=2&a=1", "https://example.com/a?a=1&b=2"},
		{"https://example.com/a/#comments", "https://example.com/a"},
		{"https://example.com/a?", "https://example.com/a"},
	}
	for _, c := range same {
		if a, b := NormLink(c[0]), NormLink(c[1]); a != b {
			t.Errorf("%q → %q，%q → %q，应当相同", c[0], a, c[1], b)
		}
	}
	differ := [][2]string{
		{"https://news.ycombinator.com/item?id=100", "https://news.ycombinator.com/item?id=200"},
		{"https://www.youtube.com/watch?v=EXAMPLE1", "https://www.youtube.com/watch?v=EXAMPLE2"},
		{"https://example.com/?p=1", "https://example.com/?p=2"},
	}
	for _, c := range differ {
		if a := NormLink(c[0]); a == NormLink(c[1]) {
			t.Errorf("%q 与 %q 都归成了 %q，应当不同", c[0], c[1], a)
		}
	}
}

// ai 的判重名单必须全列，first_seen 必须是窗内第一次出现的那天。
//
// ⚠️ 两条以前都是错的：名单和别的名单一样截到 Cap，而 prompt 按「不在名单里的就是新的」
// 用它，截掉的重复就成了今日增量；first_seen 取的是由近到远第一个碰到的日子，也就是最近一次。
func TestAinewsRepeatsAreCompleteAndDatedByFirstSighting(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	write := func(day string, links ...string) {
		items := make([]map[string]string, len(links))
		for i, l := range links {
			items[i] = map[string]string{"title": l, "source": "S", "link": l}
		}
		raw, err := json.Marshal(map[string]any{"items": items, "sources": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(root, "data", "ainews", day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "items.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const early = "https://example.com/seen-twice"
	var yesterday []string
	for i := 0; i < Cap+3; i++ {
		yesterday = append(yesterday, fmt.Sprintf("https://example.com/post/%d", i))
	}
	write("2026-10-01", early, "https://news.ycombinator.com/item?id=100")
	write("2026-10-02", append(yesterday, early)...)
	today := append(append([]string{}, yesterday...), early,
		"https://news.ycombinator.com/item?id=200",  // 新帖，只是同站
		"https://example.com/post/0?utm_source=rss") // 同一篇，换了个入口
	write("2026-10-03", today...)

	out, err := ainews(st, "2026-10-03", filepath.Join(root, "data", "ainews", "2026-10-03"))
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := out["repeat_items"].([]map[string]any)
	if want := Cap + 5; len(rep) != want || out["repeat_count"] != want {
		t.Fatalf("repeat_items %d 条、repeat_count %v，想要都是 %d", len(rep), out["repeat_count"], want)
	}
	if out["new_count"] != 1 {
		t.Errorf("new_count = %v，想要 1（只有 item?id=200 是新的）", out["new_count"])
	}
	first := map[string]any{}
	for _, r := range rep {
		first[r["title"].(string)] = r["first_seen"]
	}
	if first[early] != "2026-10-01" {
		t.Errorf("%s 的 first_seen = %v，想要窗内最早的 2026-10-01", early, first[early])
	}
	if first["https://example.com/post/0?utm_source=rss"] != "2026-10-02" {
		t.Error("只差跟踪参数的链接应当判成重复")
	}
}

// 在榜期数必须等于窗内实际出现的期数。
//
// ⚠️ 曾经 rank_history 截断到 4 条、boards_seen 用截断后的长度算，于是连续在榜 7 期
// 被写成 5。下一期的模型拿它去核上一期「连续 6 期 #1」的结论核不上，就把一条真结论
// 写成「两期文件矛盾、不可复核」发到了公开报告里。所以这里按「窗内每一天都要算」断言。
func TestTrendingBoardsSeenCountsEveryBoard(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)

	writeBoard := func(day string, repos ...string) {
		d := filepath.Join(root, "data", "trending", day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		board := make([]map[string]any, len(repos))
		for i, r := range repos {
			board[i] = map[string]any{"repo": r, "rank": i + 1}
		}
		raw, err := json.Marshal(board)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "page.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 前 6 天：常驻的那个一直在榜；另有一个只在最早两天出现过、之后掉榜
	days := []string{"2026-10-10", "2026-10-11", "2026-10-12", "2026-10-13", "2026-10-14", "2026-10-15"}
	for i, d := range days {
		if i < 2 {
			writeBoard(d, "morluto/rea", "early/one")
		} else {
			writeBoard(d, "morluto/rea")
		}
	}
	// 今天：常驻的还在，另有一个今天回归（昨天不在）
	writeBoard("2026-10-16", "morluto/rea", "back/again")

	out, err := trending(st, "2026-10-16", filepath.Join(root, "data", "trending", "2026-10-16"))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := out["repos"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("今日榜单 2 个仓库，得到 %d 行：%v", len(rows), rows)
	}

	byRepo := map[string]map[string]any{}
	for _, r := range rows {
		byRepo[r["repo"].(string)] = r
	}

	rea := byRepo["morluto/rea"]
	if rea == nil {
		t.Fatal("没找到 morluto/rea")
	}
	// 6 天前情 + 今天 = 7；名次轨迹一条不能少
	if got := rea["boards_seen"]; got != 7 {
		t.Errorf("boards_seen = %v，想要 7（窗内 6 期 + 今天）", got)
	}
	if hist, _ := rea["rank_history"].([]map[string]any); len(hist) != 6 {
		t.Errorf("rank_history 有 %d 条，想要 6（每一天都要在）", len(hist))
	}

	back := byRepo["back/again"]
	if back == nil {
		t.Fatal("没找到 back/again")
	}
	if got := back["boards_seen"]; got != 1 {
		t.Errorf("boards_seen = %v，想要 1（只有今天）", got)
	}
	if got := back["returning"]; got != false {
		t.Errorf("returning = %v，想要 false（窗内从没上过榜，不是回锅）", got)
	}
}

// 源新不新按「比上一期新进几条」算：榜首连挂不算源没动，换了顺序也不算新进，
// 只有一条新进都没有才进 no_new。按全部条目算，排第 16 的挤进前 15 不算新进。
func TestMomoyuFreshnessCountsNewEntriesPerSource(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	type item struct {
		Title string `json:"title"`
	}
	type source struct {
		Name string `json:"name"`
		Data []item `json:"data"`
	}
	src := func(name string, titles ...string) source {
		s := source{Name: name}
		for _, x := range titles {
			s.Data = append(s.Data, item{x})
		}
		return s
	}
	write := func(day string, srcs ...source) {
		raw, err := json.Marshal(map[string]any{"data": []any{map[string]any{"name": "综合", "data": srcs}}})
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(root, "data", "momoyu", day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "raw.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var long []string
	for i := 1; i <= 16; i++ {
		long = append(long, fmt.Sprintf("话题%d", i))
	}

	// 更早的一期不该拿来比
	write("2026-10-29", src("掘金", "A"), src("豆瓣热话", "X"))
	write("2026-10-30", src("掘金", "A", "B", "C"), src("豆瓣热话", "X", "Y"), src("知乎", long...))
	// 掘金榜首 A 还挂着、进了一条 D；豆瓣只换了顺序；知乎第 16 条挤到第一、进了一条新的；
	// 另有一个上一期没有的源
	today := append([]string{"话题16"}, long[:14]...)
	write("2026-10-31", src("掘金", "A", "B", "D"), src("豆瓣热话", "Y", "X"),
		src("知乎", append(today, "新话题")...), src("新源", "Z"))

	out, err := momoyu(st, "2026-10-31", filepath.Join(root, "data", "momoyu", "2026-10-31"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := out["freshness"].(map[string]any)
	if f["prev_day"] != "2026-10-30" {
		t.Errorf("prev_day = %v，应为最近的一期 2026-10-30", f["prev_day"])
	}
	want := []map[string]any{
		{"src": "掘金", "total": 3, "new": 1},
		{"src": "豆瓣热话", "total": 2, "new": 0},
		{"src": "知乎", "total": 16, "new": 1},
		{"src": "新源", "total": 1, "new": nil},
	}
	if got := f["sources"]; !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %v\n想要 %v", got, want)
	}
	if got := f["no_new"]; !reflect.DeepEqual(got, []string{"豆瓣热话"}) {
		t.Errorf("no_new = %v，想要 [豆瓣热话]", got)
	}
}

// 持续在榜里能比冷热的全列、冷热由程序判；比不了的按源汇总。掉榜按源各取最热的几条，
// 分类、商家这类读不出数的不算。
//
// ⚠️ 以前一份名单按在榜天数排、截前 12 条：挂得久的掘金文章把能比冷热的条目全挤掉了。
// 11-01 到 11-05 有 5 天截掉了这样的条目（截下来的 12 条里可比的 0–4 条，全量是 2–8 条）。
func TestMomoyuPersistentListsEveryComparableItem(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	type item struct{ Title, Extra string }
	type source struct {
		Name string `json:"name"`
		Data []item `json:"data"`
	}
	write := func(day string, srcs ...source) {
		raw, err := json.Marshal(map[string]any{"data": []any{map[string]any{"data": srcs}}})
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(root, "data", "momoyu", day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "raw.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var juejin []item
	for i := 0; i < 15; i++ {
		juejin = append(juejin, item{Title: fmt.Sprintf("掘金长挂文章%02d", i)})
	}
	write("2026-10-01", source{"掘金", juejin})
	write("2026-10-02",
		source{"知乎热榜", []item{{"甲", "100 万"}, {"乙", "300 万"}, {"丙", "200 万"}}},
		source{"微博热搜", []item{{"微一", "50万"}}},
		source{"豆瓣热话", []item{{"豆一", "1.4万"}, {"豆二", ""}}},
		source{"爱范儿", []item{{"分类标签", "产品"}}},
		source{"掘金", juejin})
	write("2026-10-03",
		source{"知乎热榜", []item{{"甲", "150 万"}}},
		source{"豆瓣热话", []item{{"豆一", "2.8万"}, {"豆二", ""}}},
		source{"掘金", juejin})

	out, err := momoyu(st, "2026-10-03", filepath.Join(root, "data", "momoyu", "2026-10-03"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range out["persistent"].([]map[string]any) {
		got = append(got, fmt.Sprint(p["src"], " ", p["title"], " ", p["change"], " ", p["trend"]))
	}
	if want := []string{"知乎热榜 甲 +50% 升温", "豆瓣热话 豆一 +100% 还在涨"}; !reflect.DeepEqual(got, want) {
		t.Errorf("persistent = %q，想要 %q", got, want)
	}
	jj := out["persistent_other"].(map[string]any)["掘金"].(map[string]any)
	if jj["count"] != 15 || jj["longest_days"] != 3 || len(jj["examples"].([]string)) != 3 {
		t.Errorf("掘金汇总 = %v", jj)
	}
	if out["persistent_total"] != 18 {
		t.Errorf("persistent_total = %v，想要 18", out["persistent_total"])
	}
	var drops []string
	for _, d := range out["dropped_from_prev"].([]map[string]any) {
		drops = append(drops, d["title"].(string))
	}
	if want := []string{"微一", "乙", "丙"}; !reflect.DeepEqual(drops, want) || out["dropped_total"] != 3 {
		t.Errorf("dropped = %q（共 %v），想要 %q（分类标签不算）", drops, out["dropped_total"], want)
	}
}
