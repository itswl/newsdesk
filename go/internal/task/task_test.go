package task

import (
	"testing"
	"time"
)

func TestScheduleSatisfiesUsageWindow(t *testing.T) {
	if p := Validate(); len(p) > 0 {
		t.Errorf("当前时间表不自洽: %v", p)
	}
}

func TestGapsSumToADay(t *testing.T) {
	var sum float64
	for _, g := range Gaps() {
		sum += g
	}
	if sum < 23.99 || sum > 24.01 {
		t.Errorf("间隔之和应为 24 小时，得到 %.4f", sum)
	}
}

func TestNextSlotWraps(t *testing.T) {
	day := time.Date(2026, 9, 15, 0, 0, 0, 0, time.Local)
	if got := NextSlotAfter("06:30", day); got.Hour() != 11 || got.Minute() != 35 {
		t.Errorf("06:30 之后应是 11:35，得到 %v", got)
	}
	// 最后一档的下一档在次日。算错会让它的补跑窗口变成负数。
	got := NextSlotAfter("21:45", day)
	if got.Day() != 16 || got.Hour() != 6 {
		t.Errorf("21:45 之后应是次日 06:30，得到 %v", got)
	}
	// 跨月、跨年
	if g := NextSlotAfter("21:45", time.Date(2026, 9, 30, 0, 0, 0, 0, time.Local)); g.Month() != time.October || g.Day() != 1 {
		t.Errorf("跨月错误: %v", g)
	}
	if g := NextSlotAfter("21:45", time.Date(2026, 12, 31, 0, 0, 0, 0, time.Local)); g.Year() != 2027 || g.Month() != time.January {
		t.Errorf("跨年错误: %v", g)
	}
}

func TestCatchupDeadlineIsBeforeNextSlot(t *testing.T) {
	day := time.Date(2026, 9, 15, 0, 0, 0, 0, time.Local)
	for _, tk := range All {
		next := NextSlotAfter(tk.At, day)
		deadline := next.Add(-CatchupMarginMin * time.Minute)
		if !deadline.Before(next) {
			t.Errorf("%s 的补跑截止点不早于下一档", tk.Key)
		}
		if !deadline.After(tk.DueAt(day)) {
			t.Errorf("%s 的补跑截止点早于触发时刻", tk.Key)
		}
	}
}
