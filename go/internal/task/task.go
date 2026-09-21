// Package task 定义四个任务：时刻、报告文件名、文案 key。
//
// 这里是唯一事实来源：这三样散落多处的话，改时间表要同时改几处，
// 还容易漏改。
package task

import (
	"fmt"
	"sort"
	"time"
)

type Task struct {
	Key   string   // ai / trending / momoyu / douban
	At    string   // HH:MM，本地时间
	Files []string // reports/<日期>/ 下的候选文件名（glob），取最新的那个
}

// ⚠️ 相邻两档间隔 5 小时 5 分是有意的：订阅的用量限额按 5 小时滚动窗口重置，
// 隔满一个窗口才不互抢额度，多出的 5 分钟防止卡边界。
// 约束是**不小于**一个窗口——比 5 小时窄会共用窗口，比它宽是安全的。
// 改时间表后 Validate() 会核对，不自洽就拒绝启动。
//
// 16:40 与 21:45 由历史产出时刻定的，不要轻易改：16:40 单次触发让 Trending
// 的增量窗口天然对齐 24 小时，基线就是昨天同一时刻的快照。
var All = []Task{
	{Key: "ai", At: "06:30", Files: []string{"ai-news.md"}},
	{Key: "douban", At: "11:35", Files: []string{"douban.md"}},
	{Key: "trending", At: "16:40", Files: []string{"github-trending.md", "github-trending_draft*.md"}},
	{Key: "momoyu", At: "21:45", Files: []string{"momoyu.md"}},
}

const (
	UsageWindowHours  = 5.0 // 用量窗口长度。换计费方式就改这里，时间表会跟着被校验
	SlotEdgeMarginMin = 5   // 额外余量，防止卡在窗口边界上
	CatchupMarginMin  = 30  // 补跑截止点 = 下一档开始前这么多分钟
	TaskTimeoutMin    = 30  // 单任务上限
)

func Find(key string) (Task, bool) {
	for _, t := range All {
		if t.Key == key {
			return t, true
		}
	}
	return Task{}, false
}

func Keys() []string {
	out := make([]string, len(All))
	for i, t := range All {
		out[i] = t.Key
	}
	return out
}

// Gaps 相邻两档的间隔（小时），含跨日那一段。
func Gaps() []float64 {
	mins := make([]int, len(All))
	for i, t := range All {
		var h, m int
		fmt.Sscanf(t.At, "%d:%d", &h, &m)
		mins[i] = h*60 + m
	}
	sort.Ints(mins)
	out := make([]float64, len(mins))
	for i, m := range mins {
		next := mins[(i+1)%len(mins)]
		out[i] = float64(((next-m)%(24*60)+24*60)%(24*60)) / 60
	}
	return out
}

// Validate 核对时间表仍然满足用量窗口假设。
//
// 写死的数字会随着改时间表悄悄失效，这个函数让「间隔」和「窗口长度」绑在一起：
// 改了 All 而没想清楚窗口，启动就会被拦下。
func Validate() []string {
	need := UsageWindowHours + float64(SlotEdgeMarginMin)/60
	gaps := Gaps()
	var problems []string
	min := gaps[0]
	for _, g := range gaps {
		if g < min {
			min = g
		}
		if g < need {
			problems = append(problems,
				fmt.Sprintf("有一档之后只隔 %.2fh，不足一个用量窗口（需 ≥ %.2fh）", g, need))
		}
	}
	if float64(CatchupMarginMin)/60 >= min {
		problems = append(problems, fmt.Sprintf(
			"补跑余量 %d 分钟 ≥ 最小间隔 %.2fh，补跑会越进下一档的窗口", CatchupMarginMin, min))
	}
	if float64(TaskTimeoutMin)/60 >= min {
		problems = append(problems, fmt.Sprintf(
			"单任务超时 %d 分钟 ≥ 最小间隔 %.2fh，超时任务会压到下一档", TaskTimeoutMin, min))
	}
	return problems
}

// DueAt 某一天里这一档的触发时刻。
func (t Task) DueAt(day time.Time) time.Time {
	var h, m int
	fmt.Sscanf(t.At, "%d:%d", &h, &m)
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
}

// NextSlotAfter 时间表里紧跟其后的那一档；已是当天最后一档则取次日第一档。
// 算错会让最后一档的补跑窗口变成负数，结果是它永远立刻被判超时跳过。
func NextSlotAfter(at string, day time.Time) time.Time {
	times := make([]string, len(All))
	for i, t := range All {
		times[i] = t.At
	}
	sort.Strings(times)
	for _, s := range times {
		if s > at {
			return Task{At: s}.DueAt(day)
		}
	}
	return Task{At: times[0]}.DueAt(day.AddDate(0, 0, 1))
}
