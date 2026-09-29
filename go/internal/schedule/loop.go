package schedule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	auditAt    string   // 日终核对时刻 HH:MM
	sched      []string // 时间表文案，写进 health.json

	cfgPath  string    // 空 = 不监视配置文件（测试、一次性命令）
	cfgMtime time.Time // 上次看到的修改时刻；读坏了也记，免得同一份坏文件每 30 秒告警一次
}

func NewLoop(root, home string, cfg *config.Config) *Loop {
	st := store.New(root)
	l := &Loop{
		Root: root, Home: home,
		Tick:       30 * time.Second,
		statePath:  st.State("schedule.json"),
		healthPath: st.State("health.json"),
	}
	l.applyConfig(cfg)
	return l
}

// applyConfig 把配置和所有从它派生出来的东西一起换掉。
//
// 只换 Cfg 不够：告警通道与日终核对时刻是构造时从配置里取出来缓存的，
// 漏了它们的话改了 ALERT_WEBHOOK，告警照样发往旧地址。
func (l *Loop) applyConfig(cfg *config.Config) {
	l.Cfg = cfg
	l.notifier = notify.New(cfg.Get("ALERT_WEBHOOK", ""), cfg.Get("ALERT_PAYLOAD", ""))
	l.auditAt = cfg.Get("AUDIT_AT", "23:30")
}

// WatchConfig 让调度器在配置文件改动后自己重读，不用重启。
//
// 为什么需要：调度器常驻，以前只在启动时读一次配置。09-29 把推送公钥写进
// config.conf、手动 publish 之后线上出现了通知开关；但调度器是半夜启动的，
// 当天中午那一档重建站点时用的还是旧配置，生成的页面没有开关，还把手动发布
// 的那版覆盖掉了。「改了配置」和「生效了」之间差一次重启，而这一步谁都会忘。
func (l *Loop) WatchConfig(path string) {
	l.cfgPath = path
	if fi, err := os.Stat(path); err == nil {
		l.cfgMtime = fi.ModTime()
	}
}

// reloadConfig 配置文件的修改时刻变了就重读。
//
// 每轮 tick 都会调，代价是一次 stat。正在跑的那一档不受影响——它拿着开跑时
// 的那份配置跑完，改动从下一轮起生效，不会半途换引擎。
//
// ⚠️ 读坏了（比如写出重复键）**沿用旧配置**，告警一次，不中断调度：一个编辑到
// 一半的配置文件不该让当天剩下的几档全部停摆。修改时刻照样记下，同一份坏文件
// 不会每 30 秒告警一次；再改一次就会重新尝试。
func (l *Loop) reloadConfig() (changed bool, err error) {
	if l.cfgPath == "" {
		return false, nil
	}
	fi, err := os.Stat(l.cfgPath)
	if err != nil || fi.ModTime().Equal(l.cfgMtime) {
		return false, nil // 暂时读不到就等下一轮，别为一次 stat 失败告警
	}
	l.cfgMtime = fi.ModTime()

	cfg, err := config.Load(l.cfgPath)
	if err != nil {
		l.logf("!! 配置文件改动后读不出来，沿用上一份：%v", err)
		l.alert("newsdesk 配置文件有误", fmt.Sprintf(
			"config.conf 改动后解析失败，调度器沿用上一份配置继续运行：\n%v\n修好后会自动重新读取，不用重启。", err))
		return false, err
	}
	before := engine.PlanLine(l.Cfg)
	l.applyConfig(cfg)
	l.logf("配置已重新读取（config.conf 修改于 %s），下一档起生效", fi.ModTime().Format("15:04:05"))
	// 引擎变了要说出来：报告是谁写的必须看得见
	if after := engine.PlanLine(cfg); after != before {
		l.logf("%s", after)
	}
	return true, nil
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
	l.sched = sched
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
	_ = WriteHealth(l.healthPath, st, sched, nil)

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
	_, _ = l.reloadConfig() // 出错已在里面告警并沿用旧配置，这里不必再管
	now := time.Now()
	for _, t := range task.All {
		// 先跟产出对一次账。手动跑过的档在调度状态里没有记录，被中断过的档
		// 还留着一条失败记录——两种情况都会让调度器把已经有报告的任务再跑
		// 一遍，白烧一次模型调用还覆盖掉好的产出。
		// 日终核对本来就以产出为准，判定这边不看就是两套标准。
		if l.reconcile(t, now, st) {
			_ = WriteHealth(l.healthPath, st, sched, nil)
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
		_ = WriteHealth(l.healthPath, st, sched, nil)
	}
	l.audit(st, now)
	return WriteHealth(l.healthPath, st, sched, nil)
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
	// 任务跑着的时候主循环阻塞在下面这一行，心跳没人写。status 要求「进程活着且
	// 心跳新于 5 分钟」，一档超过 5 分钟就被报成「已停止」——每天四档各误报一次，
	// 恰好落在最可能有人去看的那段时间。旁路心跳顺带写明正在跑哪一档、何时开始。
	stop := l.heartbeatWhile(st, t.Key, time.Minute)
	res, err := pipeline.Run(ctx, pipeline.Options{
		Root: l.Root, Home: l.Home, Cfg: l.Cfg, Task: t, Date: day,
		Log: prefixWriter{prefix: "[" + t.Key + "] "},
	})
	stop() // 必须在改 st 之前停：旁路 goroutine 在读 st.Tasks
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

// heartbeatWhile 任务执行期间每隔 every 写一次心跳，标明正在跑的档。
// 返回的 stop 会等 goroutine 退出后才返回——调用方接着要改 st，而 goroutine 在读它。
func (l *Loop) heartbeatWhile(st *State, key string, every time.Duration) (stop func()) {
	run := &Running{Key: key, Since: time.Now()}
	_ = WriteHealth(l.healthPath, st, l.sched, run)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				_ = WriteHealth(l.healthPath, st, l.sched, run)
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
