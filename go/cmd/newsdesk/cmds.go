package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/engine"
	"github.com/itswl/newsdesk/internal/langs"
	"github.com/itswl/newsdesk/internal/pipeline"
	"github.com/itswl/newsdesk/internal/prune"
	"github.com/itswl/newsdesk/internal/sandbox"
	"github.com/itswl/newsdesk/internal/schedule"
	"github.com/itswl/newsdesk/internal/store"
	"github.com/itswl/newsdesk/internal/task"
	"github.com/itswl/newsdesk/internal/translate"
)

func pickTask(args []string) (task.Task, []string, error) {
	if len(args) == 0 {
		return task.Task{}, nil, fmt.Errorf("要指定任务：%s", strings.Join(task.Keys(), " | "))
	}
	t, ok := task.Find(args[0])
	if !ok {
		return task.Task{}, nil, fmt.Errorf("未知任务 %q（可选 %s）", args[0], strings.Join(task.Keys(), " | "))
	}
	return t, args[1:], nil
}

func cmdRun(ctx context.Context, args []string) error {
	t, rest, err := pickTask(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	date := fs.String("date", store.Today(), "日期 YYYY-MM-DD")
	eng := fs.String("engine", "", "覆盖配置里的引擎（claude | codex | api）")
	skipFetch := fs.Bool("skip-fetch", false, "跳过采集，用已有数据")
	skipSite := fs.Bool("skip-site", false, "只出报告，不建站不发布")
	dry := fs.Bool("dry-publish", false, "发布阶段只打印要做什么")
	_ = fs.Parse(rest)

	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	res, err := pipeline.Run(ctx, pipeline.Options{
		Root: root, Home: home(), Cfg: cfg, Task: t, Date: *date, Engine: *eng,
		SkipFetch: *skipFetch, SkipSite: *skipSite, DryPublish: *dry, Log: os.Stdout,
	})
	if err != nil {
		if res.LogPath != "" {
			fmt.Fprintf(os.Stderr, "完整日志：%s\n", res.LogPath)
		}
		return err
	}
	fmt.Printf("✓ %s %s 完成，用时 %s\n", res.Date, res.Task, res.Duration.Round(time.Second))
	return nil
}

func cmdFetch(ctx context.Context, args []string) error {
	t, rest, err := pickTask(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	date := fs.String("date", store.Today(), "日期 YYYY-MM-DD")
	_ = fs.Parse(rest)

	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	st := store.New(root)
	if err := pipeline.Collect(ctx, t.Key, *date, cfg, st); err != nil {
		return err
	}
	if err := pipeline.History(t.Key, *date, st); err != nil {
		fmt.Printf("跨期比对跳过：%v\n", err)
	}
	return nil
}

func cmdSite(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	_, _, err = pipeline.Site(root, cfg, os.Stdout)
	return err
}

func cmdTranslate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("translate", flag.ExitOnError)
	force := fs.Bool("force", false, "已有译文也重译")
	_ = fs.Parse(args)
	date := store.Today()
	if fs.NArg() > 0 {
		date = fs.Arg(0)
	}
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if err := cfg.Require("FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"); err != nil {
		return err
	}
	c := translate.New(cfg.Get("FALLBACK_BASE_URL", ""),
		cfg.Get("FALLBACK_AUTH_TOKEN", ""), cfg.Get("FALLBACK_MODEL", ""))
	_, err = c.Day(ctx, root, date, *force)
	return err
}

func cmdPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "只打印要做什么")
	skipBuild := fs.Bool("no-build", false, "不重建站点，直接发已有的")
	_ = fs.Parse(args)

	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if !*skipBuild {
		return pipeline.Republish(ctx, root, cfg, *dry, os.Stdout)
	}
	// --no-build 直接发 site/ 会把整个历史都发出去：site/ 是全量的，
	// 按窗口截断的那份在 state/publish 下。配了 PUBLIC_DAYS 却发全量，
	// 等于悄悄把「只展示最近 N 天」这个承诺撤销了，而且不报错。
	publicDir := ""
	if cfg.Int("PUBLIC_DAYS", 0) > 0 {
		publicDir = filepath.Join(root, "state", "publish")
		if _, err := os.Stat(publicDir); err != nil {
			return fmt.Errorf("配了 PUBLIC_DAYS 但 %s 不存在：--no-build 会把全量站点发出去。"+
				"去掉 --no-build 重建一次", publicDir)
		}
	}
	// 发布前必须自检。放到发布之后等于没有自检。
	dirs := []string{filepath.Join(root, "site"), filepath.Join(root, "reports")}
	if err := pipeline.LeakGate(root, cfg, os.Stdout, dirs...); err != nil {
		return err
	}
	return pipeline.Publish(ctx, root, publicDir, cfg, *dry, os.Stdout)
}

func cmdLeakcheck(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	dirs := args
	if len(dirs) == 0 {
		dirs = []string{filepath.Join(root, "site"), filepath.Join(root, "reports")}
	}
	return pipeline.LeakCheck(root, cfg, dirs...)
}

func cmdPrune(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	res := prune.Run(root, args)
	res.Print(root)
	return nil
}

func cmdSchedule(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("schedule", flag.ExitOnError)
	bg := fs.Bool("d", false, "转入后台运行，输出写进 logs/scheduler.log")
	fs.BoolVar(bg, "daemon", false, "同 -d")
	_ = fs.Parse(args)

	root, err := findRoot()
	if err != nil {
		return err
	}
	if *bg && os.Getenv("NEWSDESK_DAEMON") == "" {
		return daemonize(root)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	l := schedule.NewLoop(root, home(), cfg)
	l.Version = version
	l.WatchConfig(cfg.Path)
	return l.Run(ctx)
}

func cmdStatus(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	st := store.New(root)
	s, err := schedule.Load(st.State("schedule.json"))
	if err != nil {
		fmt.Printf("!! %v\n", err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	// 先读心跳：任务跑着的时候要把「正在跑」标在那一档上，而不是报成「还没跑」。
	var h struct {
		HeartbeatEpoch int64  `json:"heartbeat_epoch"`
		PID            int    `json:"pid"`
		Running        string `json:"running"`
		RunningSince   string `json:"running_since"`
		Version        string `json:"version"`
	}
	haveHealth := false
	if raw, err := os.ReadFile(st.State("health.json")); err == nil && json.Unmarshal(raw, &h) == nil && h.HeartbeatEpoch > 0 {
		haveHealth = true
	}
	age := time.Since(time.Unix(h.HeartbeatEpoch, 0))
	// 光看心跳新旧不够：刚停掉的调度器心跳还很新，会被报成运行中。
	// 心跳文件是它留下的痕迹，不是它还活着的证明——要真的探一下进程。
	alive := haveHealth && h.PID > 0 && syscall.Kill(h.PID, 0) == nil && age < 5*time.Minute

	today := store.Today()
	fmt.Printf("%s   %s\n", today, engine.PlanLine(cfg))
	// 危险模式是「整套防护关着」，必须出现在每天第一眼看的地方，
	// 而且这个文件里其他行说的是「跑得怎么样」，只有这一行说的是「怎么跑的」
	if cfg.Danger() {
		fmt.Printf("  ⚠ %s\n", config.DangerLine)
	}
	for _, t := range task.All {
		ts := s.Tasks[t.Key]
		mark, note := "·", "今天还没跑"
		if ts.Date == today {
			switch ts.Status {
			case "ok":
				mark, note = "✓", "已完成"
			case "failed":
				mark, note = "✗", fmt.Sprintf("失败 %d 次：%s", ts.Attempts, ts.Reason)
			case "skipped":
				mark, note = "–", "跳过："+ts.Reason
			default:
				note = ts.Status
			}
		}
		running := alive && h.Running == t.Key
		if running {
			mark, note = "▶", "正在跑"
			if since, err := time.Parse(time.RFC3339, h.RunningSince); err == nil {
				note = fmt.Sprintf("正在跑（%s 开始，已 %s）", since.Format("15:04"), time.Since(since).Round(time.Minute))
			}
		}
		// 报告路径单独攒着，最后再拼：下面那个 switch 会整句覆盖 note，
		// 直接往里加的话，在「产出在、调度状态未同步」那一支上会被冲掉
		trail := ""
		// 报告在就是跑过了。手动跑不写调度状态，被中断过的档还留着失败记录——
		// 只看 schedule.json 会把已有产出的那一档报成「还没跑」或「失败」。
		// 判据跟调度器的 reconcile 一致：产出时刻不早于预定时刻才算这一档的。
		if p, ok := pipeline.ReportPath(root, today, t); ok {
			fresh := false
			if fi, err := os.Stat(p); err == nil && !fi.ModTime().Before(t.DueAt(time.Now())) {
				fresh = true
			}
			switch {
			case running:
				// 跑到一半的档可能已经写出报告（翻译、发布还没完），别报成已完成
			case fresh && ts.Status != "ok":
				mark, note = "✓", "已完成（产出在，调度状态未同步）"
			case mark == "·":
				mark, note = "·", note+"（有上一轮的产出）"
			}
			trail = "   " + st.RelTo(p)
		}
		// 这一档是不是无限制跑出来的——报告已经公开了，事后要能一眼看出来。
		// 放在报告路径之前，否则会读成「路径的限定语」
		if ts.Danger && ts.Date == today {
			note += "（危险模式跑的）"
		}
		// 主引擎失败、兜底顶上的档也要一眼看出来：兜底成功时其他地方全是绿的
		if ts.Fallback && ts.Date == today {
			note += fmt.Sprintf("（主引擎失败，兜底 %s 跑的）", ts.Engine)
		}
		fmt.Printf("  %s %-9s %s  %s%s\n", mark, t.Key, t.At, note, trail)
		// 这一档花了多少。日志会滚，状态文件里这份才能随时回答「这档多贵」。
		// 成本只在 claude 引擎下打：那是 CLI 按 Anthropic 价目估的，第三方网关对不上任何账单。
		// 翻译单列：它每天都走兜底账号，和分析用的引擎无关。
		if ts.Date == today && (ts.Usage != nil || ts.Translate != nil) {
			var parts []string
			if ts.Usage != nil {
				parts = append(parts, ts.Usage.Brief(ts.Engine == string(engine.Claude)))
			}
			if ts.Translate != nil {
				parts = append(parts, "翻译 入 "+kilo(ts.Translate.Input)+" / 出 "+kilo(ts.Translate.Output))
			}
			fmt.Printf("    用量：%s\n", strings.Join(parts, " ｜ "))
		}
	}
	// 兜底账号今天一共用了多少：落到兜底的分析档 + 翻译（翻译每天都走它）。
	// 订阅那边有自己的窗口和周限额，按量付费这边没人替你算，这一行就是账单的预告。
	if _, fb := engine.Plan(cfg); fb != "" {
		var an engine.Usage
		var tr translate.Usage
		nAn, nTr := 0, 0
		for _, t := range task.All {
			ts := s.Tasks[t.Key]
			if ts.Date != today {
				continue
			}
			if ts.Usage != nil && ts.Engine == string(fb) {
				nAn++
				an.Input += ts.Usage.Input
				an.CacheRead += ts.Usage.CacheRead
				an.CacheWrite += ts.Usage.CacheWrite
				an.Output += ts.Usage.Output
				an.Turns += ts.Usage.Turns
			}
			if ts.Translate != nil {
				nTr++
				tr.Input += ts.Translate.Input
				tr.Output += ts.Translate.Output
			}
		}
		var parts []string
		if nAn > 0 {
			parts = append(parts, fmt.Sprintf("分析 %d 档 %s", nAn, an.Brief(false)))
		}
		if nTr > 0 {
			parts = append(parts, fmt.Sprintf("翻译 %d 档 入 %s / 出 %s", nTr, kilo(tr.Input), kilo(tr.Output)))
		}
		if len(parts) > 0 {
			fmt.Printf("兜底账号 %s 今日：%s\n", engine.Describe(cfg, fb), strings.Join(parts, "；"))
		}
	}
	switch {
	case !haveHealth:
		fmt.Println("调度器：从未运行过")
	case alive:
		fmt.Printf("调度器：运行中（pid %d，心跳 %s 前）\n", h.PID, age.Round(time.Second))
		// 重新编译之后不重启，调度器会一直跑旧代码，各处日志照常显示「完成」——
		// 合并到上线之间最容易漏的一步。旧调度器不报版本（空），那就无从比起
		if h.Version != "" && version != "dev" && h.Version != version {
			fmt.Printf("  ⚠ 调度器跑的是 %s，磁盘上的二进制是 %s：重启才生效"+
				"（newsdesk stop && newsdesk schedule -d）\n", h.Version, version)
		}
	default:
		fmt.Printf("调度器：已停止（pid %d，心跳 %s 前）\n", h.PID, age.Round(time.Second))
	}
	return nil
}

func cmdLangs(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	fmt.Print(langs.Explain(cfg, root))
	return nil
}

func cmdSandbox(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if cfg.Danger() {
		fmt.Printf("⚠ %s\n", config.DangerLine)
		fmt.Println("  下面这份拒绝清单**不生效**。")
		fmt.Println()
	}
	fmt.Println("拒绝读取的目录：")
	for _, p := range sandbox.DenyDirs(home()) {
		fmt.Println("  " + p)
	}
	fmt.Println("拒绝读取的单个文件（按目录拒会漏掉同级同名文件）：")
	for _, p := range sandbox.DenyFiles(home(), root) {
		fmt.Println("  " + p)
	}
	b, err := sandbox.Settings(home(), root, cfg.Danger())
	if err != nil {
		return err
	}
	mode := "claude 权限文件："
	if cfg.Danger() {
		mode = "claude 权限文件（危险模式，写到 state/agent-settings-danger.json）："
	}
	fmt.Println("\n" + mode)
	fmt.Println(string(b))
	if cfg.Danger() {
		fmt.Println("codex 外层沙箱：危险模式下不套，跳过")
		return nil
	}
	if w, err := sandbox.Wrapper(home(), root, "codex"); err == nil {
		fmt.Printf("codex 外层沙箱：%s …\n", strings.Join(w[:min(3, len(w))], " "))
	} else {
		fmt.Printf("codex 外层沙箱不可用：%v\n", err)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// kilo 把 token 数缩成「8.9k」：翻译那行日志就是这么打的，status 跟它一致。
func kilo(n int) string { return fmt.Sprintf("%.1fk", float64(n)/1000) }
