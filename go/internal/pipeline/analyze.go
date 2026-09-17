package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/engine"
	"github.com/itswl/newsdesk/internal/task"
)

// ringBuffer 只留最后 N 字节。
//
// 模型输出可以是几十万字，而失败分类只需要末尾那几行。全存进内存等着 OOM，
// 或者只存开头都不对——真正的报错总在最后。
type ringBuffer struct {
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string { return string(r.buf) }

// Prompt 读任务的 prompt 并把日期代入。
//
// prompt 里写的是 `<今天>` 这种占位，由执行器换成真实目录——模型自己算日期
// 会算错（时区、跨零点），而算错的结果是它去读一个不存在的目录然后自己编。
func Prompt(root, key, date string) (string, error) {
	name := map[string]string{
		"ai": "ai-news", "douban": "douban", "trending": "trending", "momoyu": "momoyu",
	}[key]
	if name == "" {
		return "", fmt.Errorf("未知任务 %q", key)
	}
	p := filepath.Join(root, "prompts", name+".md")
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("读 prompt: %w", err)
	}
	body := string(raw)
	for _, ph := range []string{"<今天>", "<今天日期>", "{{DATE}}", "<DATE>"} {
		body = strings.ReplaceAll(body, ph, date)
	}
	return body, nil
}

type AnalyzeResult struct {
	Engine   engine.Kind
	Attempts int
	Failure  engine.Failure
	Output   string // 末尾若干 KB，用于分类与告警
}

// Analyze 把 prompt 交给引擎，失败时按分类决定是否兜底。
//
// 有三类失败重试帮不上忙（配额耗尽、余额不足、凭据失效），共同点是「等几分钟
// 再来一次必然还是失败」。对这三类直接换兜底引擎或放弃，不做无谓重试——
// 重试只会把日志刷满，然后在同一个地方失败。
func Analyze(ctx context.Context, t task.Task, date string, cfg *config.Config,
	root, home string, out io.Writer, override string) (AnalyzeResult, error) {

	prompt, err := Prompt(root, t.Key, date)
	if err != nil {
		return AnalyzeResult{}, err
	}

	primary := engine.Kind(firstNonEmpty(override, cfg.Get("ENGINE", "claude")))
	fallback := engine.Kind(cfg.Get("ENGINE_FALLBACK", ""))
	// 手动指定引擎时不再自动换人：用户要的是「用这个跑」，悄悄换掉会让
	// 「验证某个引擎是否正常」这件事做不了。
	if override != "" {
		fallback = ""
	}

	var res AnalyzeResult
	for _, kind := range []engine.Kind{primary, fallback} {
		if kind == "" {
			continue
		}
		res.Engine, res.Attempts = kind, res.Attempts+1
		ring := &ringBuffer{max: 16 << 10}
		r := &engine.Runner{Home: home, Root: root, Cfg: cfg, Out: io.MultiWriter(out, ring)}

		runCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TaskTimeoutMin)*time.Minute)
		err := r.Run(runCtx, kind, prompt)
		cancel()
		res.Output = ring.String()

		if err == nil {
			// 引擎退出码为 0 不等于报告写出来了：模型可能「完成」了但没落盘。
			// 只认文件，不认退出码。
			if _, ok := ReportPath(root, date, t); ok {
				res.Failure = ""
				return res, nil
			}
			err = errors.New("引擎正常退出，但 reports/" + date + "/ 下没有产出文件")
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("超过 %d 分钟未完成，已终止: %w", task.TaskTimeoutMin, err)
		}
		if ctx.Err() != nil {
			return res, fmt.Errorf("已取消: %w", ctx.Err())
		}
		res.Failure = engine.Classify(res.Output + " " + err.Error())
		label, action := res.Failure.Hint()
		fmt.Fprintf(out, "\n[%s] %s 失败（%s）：%v\n", t.Key, kind, label, err)
		if action != "" {
			fmt.Fprintf(out, "  → %s\n", action)
		}
		if fallback != "" && kind == primary {
			fmt.Fprintf(out, "  → 换兜底引擎 %s 再试一次\n", fallback)
		}
	}
	label, action := res.Failure.Hint()
	msg := fmt.Sprintf("%s 未能产出报告（%s）", t.Key, label)
	if action != "" {
		msg += "\n  " + action
	}
	return res, errors.New(msg)
}

// ReportPath 找这个任务今天的报告文件，多个候选取最新的那份。
func ReportPath(root, date string, t task.Task) (string, bool) {
	dir := filepath.Join(root, "reports", date)
	var best string
	var bestMod time.Time
	for _, pat := range t.Files {
		ms, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, m := range ms {
			fi, err := os.Stat(m)
			if err != nil || fi.Size() == 0 {
				continue
			}
			if best == "" || fi.ModTime().After(bestMod) {
				best, bestMod = m, fi.ModTime()
			}
		}
	}
	return best, best != ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
