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
// 只有一条新进都没有才进 unchanged。按全部条目算，排第 16 的挤进前 15 不算新进。
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
	if got := f["unchanged"]; !reflect.DeepEqual(got, []string{"豆瓣热话"}) {
		t.Errorf("unchanged = %v，想要 [豆瓣热话]", got)
	}
}
