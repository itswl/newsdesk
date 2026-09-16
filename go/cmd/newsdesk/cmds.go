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

	"github.com/itswl/newsdesk2/internal/langs"
	"github.com/itswl/newsdesk2/internal/pipeline"
	"github.com/itswl/newsdesk2/internal/prune"
	"github.com/itswl/newsdesk2/internal/sandbox"
	"github.com/itswl/newsdesk2/internal/schedule"
	"github.com/itswl/newsdesk2/internal/store"
	"github.com/itswl/newsdesk2/internal/task"
	"github.com/itswl/newsdesk2/internal/translate"
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
	return c.Day(ctx, root, date, *force)
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
	publicDir := ""
	if *skipBuild {
		// --no-build 直接发 site/ 会把整个历史都发出去：site/ 是全量的，
		// 按窗口截断的那份在 state/publish 下。配了 PUBLIC_DAYS 却发全量，
		// 等于悄悄把「只展示最近 N 天」这个承诺撤销了，而且不报错。
		if cfg.Int("PUBLIC_DAYS", 0) > 0 {
			publicDir = filepath.Join(root, "state", "publish")
			if _, err := os.Stat(publicDir); err != nil {
				return fmt.Errorf("配了 PUBLIC_DAYS 但 %s 不存在：--no-build 会把全量站点发出去。"+
					"去掉 --no-build 重建一次", publicDir)
			}
		}
	} else if _, publicDir, err = pipeline.Site(root, cfg, os.Stdout); err != nil {
		return err
	}
	// 发布前必须自检。放到发布之后等于没有自检。
	dirs := []string{filepath.Join(root, "site"), filepath.Join(root, "reports")}
	if err := pipeline.LeakCheck(root, cfg, dirs...); err != nil {
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
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	return schedule.NewLoop(root, home(), cfg).Run(ctx)
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
	today := store.Today()
	fmt.Printf("%s\n", today)
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
		// 报告在就是跑过了。手动跑不会写调度状态，只看 schedule.json 会把
		// 已有产出的那一档报成「还没跑」。
		if p, ok := pipeline.ReportPath(root, today, t); ok {
			if mark == "·" {
				mark, note = "✓", "已完成（手动）"
			}
			note += "   " + st.RelTo(p)
		}
		fmt.Printf("  %s %-9s %s  %s\n", mark, t.Key, t.At, note)
	}
	if raw, err := os.ReadFile(st.State("health.json")); err == nil {
		var h struct {
			HeartbeatEpoch int64 `json:"heartbeat_epoch"`
			PID            int   `json:"pid"`
		}
		if json.Unmarshal(raw, &h) == nil && h.HeartbeatEpoch > 0 {
			age := time.Since(time.Unix(h.HeartbeatEpoch, 0))
			// 光看心跳新旧不够：刚停掉的调度器心跳还很新，会被报成运行中。
			// 心跳文件是它留下的痕迹，不是它还活着的证明——要真的探一下进程。
			live := "已停止"
			if h.PID > 0 && syscall.Kill(h.PID, 0) == nil && age < 5*time.Minute {
				live = "运行中"
			}
			fmt.Printf("调度器：%s（pid %d，心跳 %s 前）\n", live, h.PID, age.Round(time.Second))
		}
	} else {
		fmt.Println("调度器：从未运行过")
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
	fmt.Println("拒绝读取的目录：")
	for _, p := range sandbox.DenyDirs(home()) {
		fmt.Println("  " + p)
	}
	fmt.Println("拒绝读取的单个文件（按目录拒会漏掉同级同名文件）：")
	for _, p := range sandbox.DenyFiles(home(), root) {
		fmt.Println("  " + p)
	}
	b, err := sandbox.Settings(home(), root)
	if err != nil {
		return err
	}
	fmt.Println("\nclaude 权限文件：")
	fmt.Println(string(b))
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
