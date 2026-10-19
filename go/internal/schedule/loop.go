package schedule

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
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
	// Version 这个进程的构建版本，写进 health.json。status 拿它和磁盘上的二进制比：
	// 重新编译之后没重启，调度器会一直跑旧代码，而各处日志照常显示「完成」。
	Version    string
	statePath  string
	healthPath string
	sched      []string // 时间表文案，写进 health.json

	watch cfgWatch
	// 有一档是按旧配置发布的（跑的途中配置文件改过），新配置生效时要重发一次，见 runOne
	republish bool
}

// cfgWatch 配置文件的监视状态。
//
// 比的是内容，不是修改时刻：这台机器的 HFS+ 时间戳只精确到秒，同一秒里的两次
// 保存修改时刻相同，后一次会被当成已经看过而永远不生效。比内容也顺带免掉了
// 「stat 过再去读，两步之间文件又变了」这一类竞态——解析的就是比过的那份字节。
type cfgWatch struct {
	path    string   // 空 = 不监视（测试、一次性命令）
	applied [32]byte // 手上这份配置对应的文件内容摘要；零值 = 对不上号，下一轮当改动处理
	pending [32]byte // 上一轮读到的新内容，连续两轮一样才处理——编辑器可能正写到一半
	bad     [32]byte // 解析失败的那份内容，同一份只告警一次
	readErr string   // 读不了文件时的错误，同一个错误只报一次
}

func NewLoop(root, home string, cfg *config.Config) *Loop {
	st := store.New(root)
	return &Loop{
		Root: root, Home: home, Cfg: cfg,
		Tick:       30 * time.Second,
		statePath:  st.State("schedule.json"),
		healthPath: st.State("health.json"),
	}
}

// WatchConfig 让调度器在配置文件改动后自己重读，不用重启。
//
// 为什么需要：调度器常驻，以前只在启动时读一次配置。09-29 把推送公钥写进
// config.conf、手动 publish 之后线上出现了通知开关；但调度器是半夜启动的，
// 当天中午那一档重建站点时用的还是旧配置，生成的页面没有开关，还把手动发布
// 的那版覆盖掉了。「改了配置」和「生效了」之间差一次重启，而这一步谁都会忘。
//
// 调用方读配置在前、这里读在后。两步之间要是落下一次保存，内容就对不上号，
// 那就不记——下一轮会把它当成改动读进来。只记一个修改时刻的话，那次保存会被
// 当成已经看过，永远不生效。
func (l *Loop) WatchConfig(path string) {
	l.watch = cfgWatch{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		if cfg, err := config.Parse(path, raw); err == nil && cfg.Equal(l.Cfg) {
			l.watch.applied = sha256.Sum256(raw)
		}
	}
}

// reloadConfig 配置文件的内容变了、而且写稳了，就换上新的。
//
// 每轮 tick 都会调，代价是读一个几 KB 的文件。正在跑的那一档不受影响——它拿着
// 开跑时的那份配置跑完，改动从下一轮起生效，不会半途换引擎。
//
// 新内容要连续两轮读到一样才处理。编辑器保存可能是「先清空再写」，正好读在
// 中间就会拿到空的或半截的配置——而空配置是合法的（等于「只在本机看简报」），
// 换上去的话告警通道、发布、英文站会一起悄悄消失。
//
// ⚠️ 读坏了**沿用旧配置**，告警一次，不中断调度：一个编辑到一半的配置文件不该
// 让当天剩下的几档全部停摆。同一份坏内容只告警一次，再改一次就会重新尝试。
// 文件读不了（被删、没权限）也一样：沿用旧配置，同一个错误只报一次。
func (l *Loop) reloadConfig() (changed bool, err error) {
	w := &l.watch
	if w.path == "" {
		return false, nil
	}
	name := filepath.Base(w.path)
	raw, err := os.ReadFile(w.path)
	if err != nil {
		// 从一开始就没有这个文件，是「只在本机看简报」的正常形态，不必声张
		if errors.Is(err, fs.ErrNotExist) && w.applied == ([32]byte{}) {
			return false, nil
		}
		if msg := err.Error(); msg != w.readErr {
			w.readErr = msg
			l.logf("!! 读不了 %s，沿用上一份配置：%v", name, err)
			l.alert("newsdesk 读不了配置文件", fmt.Sprintf(
				"%v\n调度器沿用上一份配置继续运行，文件恢复后会自动重新读取。"+
					"如果是有意删掉、想按「没有配置」运行，要重启调度器。", err))
		}
		return false, err
	}
	if w.readErr != "" {
		w.readErr = ""
		l.logf("%s 又能读了", name)
	}

	sum := sha256.Sum256(raw)
	switch sum {
	case w.applied:
		// 和手上这份一样（没改，或改了又改回来）。按旧配置发布的那一档也就不欠什么了
		w.pending, l.republish = [32]byte{}, false
		return false, nil
	case w.bad:
		w.pending = [32]byte{}
		return false, nil
	case w.pending:
		w.pending = [32]byte{} // 连续两轮一样：写完了，往下处理
	default:
		w.pending = sum // 刚变，等下一轮确认已经写完
		return false, nil
	}

	cfg, err := config.Parse(w.path, raw)
	if err != nil {
		w.bad = sum
		// 报错里只有文件名、行号和键名，不含值（config.Parse 保证），可以发出去
		l.logf("!! %s 改动后读不出来，沿用上一份：%v", name, err)
		l.alert("newsdesk 配置文件有误", fmt.Sprintf(
			"%s 改动后解析失败，调度器沿用上一份配置继续运行：\n%v\n修好后会自动重新读取，不用重启。", name, err))
		return false, err
	}
	w.applied, w.bad = sum, [32]byte{}
	if cfg.Equal(l.Cfg) {
		// 内容变了、值没变：加了注释、调了空白，或者是刚启动时对不上号的那次核对
		l.republish = false
		return false, nil
	}
	before, beforeAudit := engine.PlanLine(l.Cfg), l.auditTime()
	wasDanger := l.Cfg.Danger()
	l.Cfg = cfg
	l.logf("配置已重新读取（%s），下一档起生效", name)
	// 危险模式开或关都要主动说一声。它是个持续状态：只在启动时报一次的话，
	// 热重读打开的这一次会静默生效，而它关掉的是整套防护。
	if now := cfg.Danger(); now != wasDanger {
		if now {
			l.logf("⚠ %s", config.DangerLine)
			l.alert("newsdesk 危险模式已开启", config.DangerLine+
				"\n从下一档起生效。要关掉：把 config.conf 里的 DANGER 去掉或改成 0。")
		} else {
			l.logf("危险模式已关闭，防护恢复（沙箱、凭据拒读、发布前自检）")
			l.alert("newsdesk 危险模式已关闭", "config.conf 里的 DANGER 已去掉或改成 0，防护从下一档起恢复。")
		}
	}
	// 引擎变了要说出来：报告是谁写的必须看得见
	if after := engine.PlanLine(cfg); after != before {
		l.logf("%s", after)
	}
	if after := l.auditTime(); after != beforeAudit {
		l.logf("日终核对：%s", after)
	}
	return true, nil
}

// configMoved 配置文件里现在的值是否已经和手上这份不同。读坏了也算：修好之后要按新的来。
func (l *Loop) configMoved() bool {
	if l.watch.path == "" {
		return false
	}
	raw, err := os.ReadFile(l.watch.path)
	if err != nil {
		return false
	}
	cfg, err := config.Parse(l.watch.path, raw)
	return err != nil || !cfg.Equal(l.Cfg)
}

// republishSite 按现在的配置重建站点、自检、发布，不调模型——和手动 newsdesk publish 一样。
func (l *Loop) republishSite(ctx context.Context) {
	l.logf("按新配置重新发布站点（上一档是按旧配置发布的）")
	start := time.Now()
	err := pipeline.Republish(ctx, l.Root, l.Cfg, false, prefixWriter{prefix: "[重新发布] "})
	switch {
	case ctx.Err() != nil:
		l.logf("重新发布被退出信号中断")
	case err != nil:
		l.logf("!! 重新发布失败：%v", err)
		l.alert("newsdesk 按新配置重新发布失败", fmt.Sprintf(
			"配置改动后重新发布站点失败，线上停在旧配置上，下一档会再发一次：\n%v", err))
	default:
		l.logf("重新发布完成，用时 %s", time.Since(start).Round(time.Second))
	}
}

// auditTime 日终核对时刻 HH:MM。
//
// 核对是按字符串比时刻的，所以要规整成两位小时：「9:30」原样拿去比的话任何时刻
// 都小于它，核对再也不会触发，也不报错。写得认不出来就退回默认值。
func (l *Loop) auditTime() string {
	t, err := time.Parse("15:04", l.Cfg.Get("AUDIT_AT", "23:30"))
	if err != nil {
		return "23:30"
	}
	return t.Format("15:04")
}

func (l *Loop) alert(title, text string) {
	// 告警通道每次从当前配置现取。缓存一份的话，热重读时得记得一起换——漏换不报错，
	// 告警照样发往旧地址
	n := notify.New(l.Cfg.Get("ALERT_WEBHOOK", ""), l.Cfg.Get("ALERT_PAYLOAD", ""))
	if err := n.Send(title, text); err != nil {
		// 告警通道本身坏了是要知道的，但不能因此让调度停下来
		l.logf("!! 告警发送失败：%v", err)
	}
}

func (l *Loop) health(st *State, run *Running) error {
	return WriteHealth(l.healthPath, st, l.sched, run, l.Version, l.Cfg.Danger())
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
	l.logf("调度器启动，pid %d，构建 %s", os.Getpid(), l.Version)
	l.logf("时间表：%s", strings.Join(sched, " | "))
	l.logf("%s", engine.PlanLine(l.Cfg))
	l.logf("日终核对：%s", l.auditTime())
	if l.Cfg.Danger() {
		l.logf("⚠ %s", config.DangerLine)
		l.alert("newsdesk 危险模式已开启", config.DangerLine+
			"\n这个开关下跑出来的报告会照常公开发布。要关掉：把 config.conf 里的 DANGER 去掉或改成 0。")
	}

	st, err := Load(l.statePath)
	if err != nil {
		// 读不出来 ≠ 没有。回空会让今天所有任务被当成没跑过而全部重跑，
		// 正好把用量窗口那套间隔毁掉。所以要告警、要留痕。
		l.logf("!! %v", err)
		l.alert("newsdesk 调度状态异常", err.Error()+"\n已按空状态继续，今天的任务可能重跑。")
	}
	if st.FallbackSince != "" {
		// 重启不该让人以为主引擎好了：这段还没结束，恢复时照样会报
		l.logf("⚠ 主引擎失效中，兜底段自 %s 起（上次告警过，恢复时再报）", clock(st.FallbackSince))
	}
	_ = l.health(st, nil)

	tk := time.NewTicker(l.Tick)
	defer tk.Stop()
	consecutiveErr := 0

	for {
		if err := l.tick(ctx, st); err != nil {
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

func (l *Loop) tick(ctx context.Context, st *State) error {
	// 出错已在里面告警并沿用旧配置，这里不必再管
	if changed, _ := l.reloadConfig(); changed && l.republish {
		l.republish = false
		l.republishSite(ctx)
	}
	now := time.Now()
	for _, t := range task.All {
		// 先跟产出对一次账。手动跑过的档在调度状态里没有记录，被中断过的档
		// 还留着一条失败记录——两种情况都会让调度器把已经有报告的任务再跑
		// 一遍，白烧一次模型调用还覆盖掉好的产出。
		// 日终核对本来就以产出为准，判定这边不看就是两套标准。
		if l.reconcile(t, now, st) {
			_ = l.health(st, nil)
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
		_ = l.health(st, nil)
	}
	l.audit(st, now)
	return l.health(st, nil)
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
	// 只继承**同一天**那次的标记：跨天继承会把昨天无限制跑出来的标签贴到今天的
	// 报告上，那比不标更糟——一份普通跑出来的报告被永久标成「危险模式跑的」。
	danger := prev.Danger && prev.Date == day
	st.Tasks[t.Key] = TaskState{
		Date: day, Status: "ok", Engine: prev.Engine, Attempts: prev.Attempts,
		Danger:   danger,
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
	danger := l.Cfg.Danger()
	if danger {
		// 这一档是在什么模式下跑的，必须在这档自己的日志里出现——报告次日就公开了
		l.logf("[%s] ⚠ %s", t.Key, config.DangerLine)
	}
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
	next.Danger = danger
	if res.FellBack {
		next.Fallback, next.FallbackWhy = true, string(res.PrimaryFailure)
	}
	if res.Usage.Known {
		u := res.Usage
		next.Usage = &u
	}
	if res.Translate.Input+res.Translate.Output > 0 {
		tu := res.Translate
		next.Translate = &tu
	}
	st.Tasks[t.Key] = next
	if e := st.Save(l.statePath); e != nil {
		l.logf("!! 状态保存失败：%v", e)
	}
	if err == nil {
		l.logf("[%s] 完成，用时 %s，%s", t.Key,
			res.Duration.Round(time.Second), engine.Describe(l.Cfg, res.Engine))
		l.noteFallback(st, t, next)
		// 跑的途中配置文件改过：这一档是按开跑时那份配置建站发布的，期间要是按新配置
		// 手动发布过，已经被它覆盖（09-29 丢通知开关就是这个形状）。新配置生效时重发一次。
		if l.configMoved() {
			l.republish = true
			l.logf("[%s] 跑的途中 %s 改过，这一档是按旧配置发布的；新配置生效后会按它重新发布一次",
				t.Key, filepath.Base(l.watch.path))
		}
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

// noteFallback 主引擎失效、兜底顶上的连续段：开始与结束各告警一次。
//
// 兜底成功时流水线看起来一切正常——报告照出、站点照发、status 全绿——只有日志里
// 「完成，用时 …，api / …」那一行能看出来。2026-10-15 到 10-19 连续 17 档跑在兜底上，
// 一条告警都没有，因为以前只有终局失败才告警。每档都推又等于教人忽略告警，所以按
// 连续段：切过去时说一声，回来时再说一声。失败的档不动这个状态——它既没证明主引擎
// 好了，也没证明兜底还在顶着。
func (l *Loop) noteFallback(st *State, t task.Task, next TaskState) {
	primary, _ := engine.Plan(l.Cfg)
	used := engine.Describe(l.Cfg, engine.Kind(next.Engine))
	switch {
	case next.Fallback && st.FallbackSince == "":
		st.FallbackSince = next.Finished
		label, action := engine.Failure(next.FallbackWhy).Hint()
		l.logf("[%s] ⚠ 主引擎 %s 失败（%s），这一档由兜底 %s 跑出；主引擎恢复前每档都会这样，不再重复告警",
			t.Key, engine.Describe(l.Cfg, primary), label, used)
		body := fmt.Sprintf("任务 %s（预定 %s）主引擎 %s 失败（%s），由兜底 %s 跑出并已照常发布。\n"+
			"主引擎恢复前每档都会先试主引擎再落到兜底，这段不再重复告警；恢复时另报一条。",
			t.Key, t.At, engine.Describe(l.Cfg, primary), label, used)
		if action != "" {
			body += "\n→ " + action
		}
		l.alert("newsdesk 主引擎失效，已切兜底", body)
	case !next.Fallback && st.FallbackSince != "":
		since := clock(st.FallbackSince)
		st.FallbackSince = ""
		l.logf("[%s] 主引擎已恢复（兜底段 %s 起）", t.Key, since)
		l.alert("newsdesk 主引擎已恢复",
			fmt.Sprintf("任务 %s（预定 %s）回到主引擎 %s。兜底段从 %s 起，到这一档为止。", t.Key, t.At, used, since))
	default:
		return
	}
	if err := st.Save(l.statePath); err != nil {
		l.logf("!! 状态保存失败：%v", err)
	}
}

// clock 把 RFC3339 缩成「10-15 06:35」，给人看的告警不需要秒和时区。
func clock(rfc string) string {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.Format("01-02 15:04")
	}
	return rfc
}

// audit 日终核对：今天四档里有没有没出报告的。
//
// 这条线只在调度器还活着时才响，覆盖不了「整机不在」——那一层靠站点上的
// status.json 被外部监控轮询。两条线各管一半，都要有。
func (l *Loop) audit(st *State, now time.Time) {
	day := now.Format("2006-01-02")
	if st.Audit == day || now.Format("15:04") < l.auditTime() {
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
	_ = l.health(st, run)
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
				_ = l.health(st, run)
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
