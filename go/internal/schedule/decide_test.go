package schedule

import (
	"errors"
	"testing"
	"time"

	"github.com/itswl/newsdesk2/internal/engine"
	"github.com/itswl/newsdesk2/internal/task"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

var trending, _ = task.Find("trending") // 16:40，下一档 21:45

func TestDecideNotYetDue(t *testing.T) {
	d := Decide(trending, at("2026-09-17 16:39"), TaskState{})
	if d.Action != ActWait {
		t.Fatalf("到点前应等待，得到 %s（%s）", d.Action, d.Reason)
	}
}

func TestDecideOnTime(t *testing.T) {
	d := Decide(trending, at("2026-09-17 16:40"), TaskState{})
	if d.Action != ActRun {
		t.Fatalf("到点应运行，得到 %s", d.Action)
	}
}

// 合盖睡过一整档，唤醒后必须补上——这是整个调度器存在的理由。
func TestDecideCatchupAfterSleep(t *testing.T) {
	d := Decide(trending, at("2026-09-17 19:30"), TaskState{})
	if d.Action != ActRun {
		t.Fatalf("睡过头后应补跑，得到 %s（%s）", d.Action, d.Reason)
	}
	if d.Late < 2*time.Hour {
		t.Fatalf("迟到时长算错：%v", d.Late)
	}
}

// 补跑不能越过下一档：21:45 减 30 分钟 = 21:15 之后就不补了，
// 否则两个任务挤进同一个用量窗口。
func TestDecideCatchupDeadline(t *testing.T) {
	if d := Decide(trending, at("2026-09-17 21:14"), TaskState{}); d.Action != ActRun {
		t.Fatalf("截止点前应仍可补跑，得到 %s", d.Action)
	}
	d := Decide(trending, at("2026-09-17 21:20"), TaskState{})
	if d.Action != ActMiss {
		t.Fatalf("越过截止点应判错过，得到 %s（%s）", d.Action, d.Reason)
	}
}

// 当天最后一档的下一档是次日第一档，算错会让它的补跑窗口变成负数、永远立刻超时。
func TestDecideLastSlotWrapsToNextDay(t *testing.T) {
	momoyu, _ := task.Find("momoyu") // 21:45，下一档是次日 06:30
	if d := Decide(momoyu, at("2026-09-17 23:59"), TaskState{}); d.Action != ActRun {
		t.Fatalf("当天最后一档在午夜前应仍可补跑，得到 %s（%s）", d.Action, d.Reason)
	}
}

func TestDecideAlreadyDone(t *testing.T) {
	d := Decide(trending, at("2026-09-17 17:00"), TaskState{Date: "2026-09-17", Status: "ok"})
	if d.Action != ActWait {
		t.Fatalf("今天跑过了就不该再跑，得到 %s", d.Action)
	}
}

func TestDecideYesterdayDoneDoesNotBlockToday(t *testing.T) {
	d := Decide(trending, at("2026-09-17 16:45"), TaskState{Date: "2026-09-16", Status: "ok"})
	if d.Action != ActRun {
		t.Fatalf("昨天的成功记录不该挡住今天，得到 %s", d.Action)
	}
}

func TestDecideRetryBackoff(t *testing.T) {
	last := at("2026-09-17 16:45")
	st := TaskState{Date: "2026-09-17", Status: "failed", Attempts: 1, LastAttempt: last.Unix()}
	if d := Decide(trending, last.Add(time.Minute), st); d.Action != ActWait {
		t.Fatalf("退避期内不该立刻重试，得到 %s", d.Action)
	}
	if d := Decide(trending, last.Add(5*time.Minute), st); d.Action != ActRun {
		t.Fatalf("退避期过后应重试，得到 %s（%s）", d.Action, d.Reason)
	}
}

func TestDecideStopsAfterMaxAttempts(t *testing.T) {
	st := TaskState{Date: "2026-09-17", Status: "failed", Attempts: maxAttempts,
		LastAttempt: at("2026-09-17 16:45").Unix()}
	if d := Decide(trending, at("2026-09-17 18:00"), st); d.Action != ActWait {
		t.Fatalf("到上限后应停手，得到 %s", d.Action)
	}
}

// 配额/余额/凭据这三类重试帮不上忙，第一次失败就该终局。
func TestAfterRunGivesUpOnHardFailures(t *testing.T) {
	now := at("2026-09-17 16:50")
	for _, f := range []engine.Failure{engine.FailQuota, engine.FailBalance, engine.FailAuth} {
		st, final := AfterRun(TaskState{}, "2026-09-17", "claude", f, errors.New("x"), 0, now)
		if !final {
			t.Fatalf("%s 应当立即终局", f)
		}
		if st.Status != "failed" || st.Attempts != 1 {
			t.Fatalf("%s 状态不对：%+v", f, st)
		}
	}
	_, final := AfterRun(TaskState{}, "2026-09-17", "claude", engine.FailOther, errors.New("x"), 0, now)
	if final {
		t.Fatal("一般失败第一次不该终局，应当重试")
	}
}

func TestAfterRunAccumulatesAttempts(t *testing.T) {
	now := at("2026-09-17 16:50")
	prev := TaskState{Date: "2026-09-17", Status: "failed", Attempts: 2}
	st, final := AfterRun(prev, "2026-09-17", "claude", engine.FailOther, errors.New("x"), 0, now)
	if st.Attempts != 3 || !final {
		t.Fatalf("第三次失败应终局：%+v final=%v", st, final)
	}
	// 跨天要清零，不然昨天的失败次数会把今天第一次就顶到上限
	st2, _ := AfterRun(prev, "2026-09-18", "claude", engine.FailOther, errors.New("x"), 0, now)
	if st2.Attempts != 1 {
		t.Fatalf("跨天应从 1 开始，得到 %d", st2.Attempts)
	}
}
