package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/itswl/newsdesk2/internal/config"
	"github.com/itswl/newsdesk2/internal/engine"
	"github.com/itswl/newsdesk2/internal/store"
	"github.com/itswl/newsdesk2/internal/task"
)

type Options struct {
	Root, Home string
	Cfg        *config.Config
	Task       task.Task
	Date       string
	Engine     string // 覆盖配置里的 ENGINE；空 = 用配置
	SkipFetch  bool
	SkipSite   bool
	DryPublish bool
	Log        io.Writer // 额外的日志去向；总会同时写 logs/<日期>-<任务>.log
}

type Result struct {
	Task     string
	Date     string
	Report   string
	Engine   engine.Kind
	Failure  engine.Failure
	Output   string
	Duration time.Duration
	LogPath  string
}

// Run 跑完一个任务的全流程。
//
// 顺序是有理由的：自检在发布之前，发布在最后。把自检放到发布之后等于没有自检。
func Run(ctx context.Context, o Options) (Result, error) {
	start := time.Now()
	if o.Date == "" {
		o.Date = store.Today()
	}
	res := Result{Task: o.Task.Key, Date: o.Date}

	logPath := filepath.Join(o.Root, "logs", fmt.Sprintf("%s-%s.log", o.Date, o.Task.Key))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return res, err
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return res, err
	}
	defer lf.Close()
	res.LogPath = logPath

	var out io.Writer = lf
	if o.Log != nil {
		out = io.MultiWriter(lf, o.Log)
	}
	step := func(n int, name string) {
		fmt.Fprintf(out, "\n[%s] %d/6 %s  %s\n",
			o.Task.Key, n, name, time.Now().Format("15:04:05"))
	}
	fmt.Fprintf(out, "\n===== %s %s 开始 =====\n", o.Date, o.Task.Key)

	st := store.New(o.Root)

	if !o.SkipFetch {
		step(1, "采集")
		if err := Collect(ctx, o.Task.Key, o.Date, o.Cfg, st); err != nil {
			return res, fmt.Errorf("采集失败: %w", err)
		}
		step(2, "跨期比对")
		if err := History(o.Task.Key, o.Date, st); err != nil {
			// 首期没有历史是正常的，不该因此整档不跑
			fmt.Fprintf(out, "  跨期比对跳过：%v\n", err)
		}
	} else {
		fmt.Fprintf(out, "[%s] 跳过采集，直接用已有数据\n", o.Task.Key)
	}

	step(3, "模型分析")
	ar, err := Analyze(ctx, o.Task, o.Date, o.Cfg, o.Root, o.Home, out, o.Engine)
	res.Engine, res.Failure, res.Output = ar.Engine, ar.Failure, ar.Output
	if err != nil {
		res.Duration = time.Since(start)
		return res, err
	}
	res.Report, _ = ReportPath(o.Root, o.Date, o.Task)
	fmt.Fprintf(out, "  报告: %s\n", st.RelTo(res.Report))

	if o.SkipSite {
		res.Duration = time.Since(start)
		fmt.Fprintf(out, "===== %s %s 完成（未建站）=====\n", o.Date, o.Task.Key)
		return res, nil
	}

	step(4, "翻译")
	if err := Translate(ctx, o.Root, o.Date, o.Cfg, out); err != nil {
		// 翻译失败不该让今天的简体站也发不出去
		fmt.Fprintf(out, "  翻译失败，英文版本将缺这一天：%v\n", err)
	}

	step(5, "建站 + 自检")
	localDir, publicDir, err := Site(o.Root, o.Cfg, out)
	if err != nil {
		res.Duration = time.Since(start)
		return res, err
	}
	dirs := []string{localDir, filepath.Join(o.Root, "reports")}
	if publicDir != "" && publicDir != localDir {
		dirs = append(dirs, publicDir)
	}
	if err := LeakCheck(o.Root, o.Cfg, dirs...); err != nil {
		res.Duration = time.Since(start)
		return res, err
	}

	step(6, "发布")
	if err := Publish(ctx, o.Root, publicDir, o.Cfg, o.DryPublish, out); err != nil {
		res.Duration = time.Since(start)
		return res, fmt.Errorf("发布失败: %w", err)
	}

	res.Duration = time.Since(start)
	fmt.Fprintf(out, "===== %s %s 完成，用时 %s =====\n",
		o.Date, o.Task.Key, res.Duration.Round(time.Second))
	return res, nil
}
