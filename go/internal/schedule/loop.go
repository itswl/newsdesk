package schedule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/engine"
	"github.com/itswl/newsdesk/internal/notify"
	"github.com/itswl/newsdesk/internal/pipeline"
	"github.com/itswl/newsdesk/internal/store"
	"github.com/itswl/newsdesk/internal/task"
)

type Loop struct {
	Root, Home string
	Cfg        *config.Config
	Tick       time.Duration
	notifier   *notify.Notifier
	statePath  string
	healthPath string
	auditAt    string // 日终核对时刻 HH:MM
}

func NewLoop(root, home string, cfg *config.Config) *Loop {
	st := store.New(root)
	return &Loop{
		Root: root, Home: home, Cfg: cfg,
		Tick:       30 * time.Second,
		notifier:   notify.New(cfg.Get("ALERT_WEBHOOK", ""), cfg.Get("ALERT_PAYLOAD", "")),
		statePath:  st.State("schedule.json"),
		healthPath: st.State("health.json"),
		auditAt:    cfg.Get("AUDIT_AT", "23:30"),
	}
}

func (l *Loop) alert(title, text string) {
	if err := l.notifier.Send(title, text); err != nil {
		// 告警通道本身坏了是要知道的，但不能因此让调度停下来
		l.logf("!! 告警发送失败：%v", err)
	}
}

func (l *Loop) logf(f string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(f, a...))
}

// Run 启动常驻循环。
//
// 不用 launchd / cron：Claude Code 的 Bash 不在用户登录会话里，launchctl bootstrap
// 在 gui/502 和 user/502 都装不上。而且真正要解决的是睡眠补跑，
// cron 的语义（到点没醒就是丢了）恰恰不做这件事。
func (l *Loop) Run(ctx context.Context) error {
	if problems := task.Validate(); len(problems) > 0 {
		return fmt.Errorf("时间表与用量窗口不自洽，拒绝启动：\n  %s",
			strings.Join(problems, "\n  "))
	}
	release, err := l.lock()
	if err != nil {
		return err
	}
	defer release()

	var sched []string
	for _, t := range task.All {
		sched = append(sched, t.At+" "+t.Key)
	}
	l.logf("调度器启动，pid %d", os.Getpid())
	l.logf("时间表：%s", strings.Join(sched, " | "))
	l.logf("%s", engine.PlanLine(l.Cfg))
	l.logf("日终核对：%s", l.auditAt)

	st, err := Load(l.statePath)
	if err != nil {
		// 读不出来 ≠ 没有。回空会让今天所有任务被当成没跑过而全部重跑，
		// 正好把用量窗口那套间隔毁掉。所以要告警、要留痕。
		l.logf("!! %v", err)
		l.alert("newsdesk 调度状态异常", err.Error()+"\n已按空状态继续，今天的任务可能重跑。")
	}
	_ = WriteHealth(l.healthPath, st, sched)

	tk := time.NewTicker(l.Tick)
	defer tk.Stop()
	consecutiveErr := 0

	for {
		if err := l.tick(ctx, st, sched); err != nil {
			consecutiveErr++
			l.logf("!! 循环异常（连续 %d 次）：%v", consecutiveErr, err)
			// 单次异常可能只是磁盘忙。连续失败说明调度本身坏了，
			// 那正是「没有任何报告产出但也没人知道」的形状。
			if consecutiveErr == 3 {
				l.alert("newsdesk 调度循环异常",
					fmt.Sprintf("连续 %d 次 tick 失败，调度可能已停止工作：\n%v", consecutiveErr, err))
			}
		} else {
			consecutiveErr = 0
		}
		select {
		case <-ctx.Done():
			l.logf("收到退出信号，停止调度")
			return nil
		case <-tk.C:
		}
	}
}

func (l *Loop) tick(ctx context.Context, st *State, sched []string) error {
	now := time.Now()
	for _, t := range task.All {
		// 先跟产出对一次账。手动跑过的档在调度状态里没有记录，被中断过的档
		// 还留着一条失败记录——两种情况都会让调度器把已经有报告的任务再跑
		// 一遍，白烧一次模型调用还覆盖掉好的产出。
		// 日终核对本来就以产出为准，判定这边不看就是两套标准。
		if l.reconcile(t, now, st) {
			_ = WriteHealth(l.healthPath, st, sched)
			continue
		}
		d := Decide(t, now, st.Tasks[t.Key])
		switch d.Action {
		case ActWait:
			continue
		case ActMiss:
			l.logf("[%s] %s", t.Key, d.Reason)
			st.Tasks[t.Key] = TaskState{Date: now.Format("2006-01-02"), Status: "skipped",
				Reason: d.Reason, Finished: now.Format(time.RFC3339), LateHours: d.Late.Hours()}
			if err := st.Save(l.statePath); err != nil {
				return err
			}
			l.alert("newsdesk 错过一档", fmt.Sprintf("任务 %s（预定 %s）%s", t.Key, t.At, d.Reason))
		case ActRun:
			l.logf("[%s] %s，开始", t.Key, d.Reason)
			l.runOne(ctx, t, d, st, now)
		}
		_ = WriteHealth(l.healthPath, st, sched)
	}
	l.audit(st, now)
	return WriteHealth(l.healthPath, st, sched)
}

// reconcile 用产出校正调度状态，返回 true 表示这一档已经有今天的报告、不必再跑。
//
// ⚠️ 判据是「报告的生成时刻不早于这一档的预定时刻」，不是「今天有这个文件」。
// Trending 那一档有早间草稿：文件在、但那是上一轮的产物，16:40 的正式那次
// 仍然要跑。只看文件存在会把它整档跳过，而且不报错。
func (l *Loop) reconcile(t task.Task, now time.Time, st *State) bool {
	day := now.Format("2006-01-02")
	if cur := st.Tasks[t.Key]; cur.Date == day && cur.Status == "ok" {
		return true
	}
	p, ok := pipeline.ReportPath(l.Root, day, t)
	if !ok {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil || fi.ModTime().Before(t.DueAt(now)) {
		return false
	}
	prev := st.Tasks[t.Key]
	st.Tasks[t.Key] = TaskState{
		Date: day, Status: "ok", Engine: prev.Engine, Attempts: prev.Attempts,
		Finished: fi.ModTime().Format(time.RFC3339),
		Reason:   "按产出校正（" + reconcileWhy(prev, day) + "）",
	}
	l.logf("[%s] 今天已有报告（%s），按产出校正为已完成，不再重跑",
		t.Key, fi.ModTime().Format("15:04"))
	if err := st.Save(l.statePath); err != nil {
		l.logf("!! 状态保存失败：%v", err)
	}
	return true
}

func reconcileWhy(prev TaskState, day string) string {
	if prev.Date == day && prev.Status == "failed" {
		return "原记录为失败，但产出存在"
	}
	return "手动跑过，调度状态里没有记录"
}

func (l *Loop) runOne(ctx context.Context, t task.Task, d Decision, st *State, now time.Time) {
	day := now.Format("2006-01-02")
	res, err := pipeline.Run(ctx, pipeline.Options{
		Root: l.Root, Home: l.Home, Cfg: l.Cfg, Task: t, Date: day,
		Log: prefixWriter{prefix: "[" + t.Key + "] "},
	})
	end := time.Now()

	// 主动停止不是失败。收到退出信号时模型子进程会被一起杀掉，pipeline 因此
	// 报错——但把它记成 failed 会吃掉一次重试额度，而重试上限只有三次：
	// 在一档的窗口里重启两次调度器，这一档当天就再也不会跑了，还会推一条
	// 「任务失败」告警。状态原样留着，重启后按「今天还没跑」正常补跑。
	if ctx.Err() != nil {
		l.logf("[%s] 被退出信号中断，未记为失败（重启后会重新补跑）", t.Key)
		return
	}

	next, final := AfterRun(st.Tasks[t.Key], day, res.Engine, res.Failure, err, d.Late, end)
	st.Tasks[t.Key] = next
	if e := st.Save(l.statePath); e != nil {
		l.logf("!! 状态保存失败：%v", e)
	}
	if err == nil {
		l.logf("[%s] 完成，用时 %s，%s", t.Key,
			res.Duration.Round(time.Second), engine.Describe(l.Cfg, res.Engine))
		return
	}
	l.logf("[%s] 失败（第 %d 次）：%v", t.Key, next.Attempts, err)
	if !final {
		return
	}
	// 终局失败才告警：中间的重试会自己好，每次都推等于教人忽略告警
	label, action := res.Failure.Hint()
	body := fmt.Sprintf("任务 %s（预定 %s）在第 %d 次尝试后放弃。\n原因：%s\n%v",
		t.Key, t.At, next.Attempts, label, err)
	if action != "" {
		body += "\n→ " + action
	}
	if res.LogPath != "" {
		body += "\n日志：" + res.LogPath
	}
	l.alert("newsdesk 任务失败", body)
}

// audit 日终核对：今天四档里有没有没出报告的。
//
// 这条线只在调度器还活着时才响，覆盖不了「整机不在」——那一层靠站点上的
// status.json 被外部监控轮询。两条线各管一半，都要有。
func (l *Loop) audit(st *State, now time.Time) {
	day := now.Format("2006-01-02")
	if st.Audit == day || now.Format("15:04") < l.auditAt {
		return
	}
	st.Audit = day
	var bad []string
	for _, t := range task.All {
		s := st.Tasks[t.Key]
		if s.Date == day && s.Status == "ok" {
			continue
		}
		if _, ok := pipeline.ReportPath(l.Root, day, t); ok {
			continue // 报告在，状态没跟上，不算缺口
		}
		why := s.Status
		if s.Date != day {
			why = "今天没有记录"
		}
		if s.Reason != "" {
			why += "：" + s.Reason
		}
		bad = append(bad, fmt.Sprintf("  %s（%s）%s", t.Key, t.At, why))
	}
	_ = st.Save(l.statePath)
	if len(bad) == 0 {
		l.logf("日终核对：%s 四档齐全", day)
		return
	}
	l.logf("日终核对：%s 缺 %d 档", day, len(bad))
	l.alert("newsdesk 日终核对有缺口",
		fmt.Sprintf("%s 有 %d 档没有产出：\n%s", day, len(bad), strings.Join(bad, "\n")))
}

// lock 单实例。两个调度器同时跑会让同一档被跑两遍，而模型调用是有成本的。
func (l *Loop) lock() (func(), error) {
	p := filepath.Join(l.Root, "state", "scheduler.pid")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(p); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
			// 信号 0 只探测存在性，不真的发信号
			if err := syscall.Kill(pid, 0); err == nil {
				return nil, fmt.Errorf("已有调度器在运行（pid %d）。要换一个就先停掉它", pid)
			}
			// 进程没了但 pid 文件还在——上次是被强杀的，接管即可
		}
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}

type prefixWriter struct{ prefix string }

func (w prefixWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			fmt.Println(w.prefix + line)
		}
	}
	return len(b), nil
}
