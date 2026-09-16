package schedule

import (
	"fmt"
	"time"

	"github.com/itswl/newsdesk2/internal/engine"
	"github.com/itswl/newsdesk2/internal/task"
)

type Action string

const (
	ActWait Action = "wait" // 还没到点，或今天已经处理完了
	ActRun  Action = "run"  // 该跑了（含补跑）
	ActMiss Action = "miss" // 错过了补跑窗口，标记跳过并告警
)

type Decision struct {
	Action Action
	Reason string
	Late   time.Duration // 比预定时刻晚了多久
}

const maxAttempts = 3

// retryDelay 失败后隔多久再试。
//
// 只对「可能是一次性」的失败退避重试。配额/余额/凭据那三类等多久都一样，
// 由 giveUp 直接终结，不进这里。
func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 3 * time.Minute
	case 2:
		return 10 * time.Minute
	}
	return 30 * time.Minute
}

func giveUp(f engine.Failure) bool {
	return f == engine.FailQuota || f == engine.FailBalance || f == engine.FailAuth
}

// Decide 判断某一档现在该不该跑。
//
// 这里是**睡眠补跑**的核心：判据不是「此刻是否等于预定时刻」，而是
// 「今天这一档该跑了吗、跑过了吗」。合盖睡过 16:40，18:00 唤醒后照样补上——
// 按时刻触发的调度器在这种场景下会静默丢一整档。
//
// 补跑有截止点：下一档开始前 CatchupMarginMin 分钟。越过去就不补了，
// 否则一个睡过头的任务会跟下一档挤进同一个用量窗口——正是那套间隔要避免的事。
func Decide(t task.Task, now time.Time, st TaskState) Decision {
	today := now.Format("2006-01-02")
	due := t.DueAt(now)
	late := now.Sub(due)

	if st.Date == today {
		switch st.Status {
		case "ok", "skipped":
			return Decision{Action: ActWait, Reason: "今天已处理"}
		case "failed":
			if st.Attempts >= maxAttempts {
				return Decision{Action: ActWait, Reason: fmt.Sprintf("今天已失败 %d 次，不再重试", st.Attempts)}
			}
			if st.LastAttempt > 0 {
				next := time.Unix(st.LastAttempt, 0).Add(retryDelay(st.Attempts))
				if now.Before(next) {
					return Decision{Action: ActWait,
						Reason: fmt.Sprintf("第 %d 次重试要等到 %s", st.Attempts+1, next.Format("15:04"))}
				}
			}
		}
	}

	if now.Before(due) {
		return Decision{Action: ActWait, Reason: "未到时刻"}
	}

	deadline := task.NextSlotAfter(t.At, now).Add(-time.Duration(task.CatchupMarginMin) * time.Minute)
	if now.After(deadline) {
		if st.Date == today && st.Status == "failed" {
			// 已经试过了、也已经告过警，这里只是不再重试
			return Decision{Action: ActWait, Reason: "已过补跑窗口", Late: late}
		}
		return Decision{Action: ActMiss, Late: late,
			Reason: fmt.Sprintf("预定 %s，现在已晚 %s，越过补跑截止点 %s，本档跳过",
				t.At, round(late), deadline.Format("15:04"))}
	}

	why := "到点"
	if late > 2*time.Minute {
		why = fmt.Sprintf("补跑（晚 %s）", round(late))
	}
	if st.Date == today && st.Status == "failed" {
		why = fmt.Sprintf("第 %d 次重试", st.Attempts+1)
	}
	return Decision{Action: ActRun, Reason: why, Late: late}
}

// AfterRun 把一次运行的结果折成新的任务状态。
func AfterRun(prev TaskState, day string, kind engine.Kind, f engine.Failure,
	err error, late time.Duration, now time.Time) (TaskState, bool) {

	st := TaskState{Date: day, Engine: string(kind), LastAttempt: now.Unix(),
		Finished: now.Format(time.RFC3339), LateHours: late.Hours()}
	if prev.Date == day {
		st.Attempts = prev.Attempts
	}
	st.Attempts++
	if err == nil {
		st.Status = "ok"
		return st, false
	}
	st.Status = "failed"
	st.Reason = trunc(err.Error(), 400)
	final := giveUp(f) || st.Attempts >= maxAttempts
	return st, final
}

func round(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f 小时", d.Hours())
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
