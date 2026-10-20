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
	// 临时目录同理由执行器代入：路径只在 ScratchDir 定义一次，prompt 里写死的话
	// 改了这边、prompt 还指着旧地方，模型写进去的东西就没人清。
	body = strings.ReplaceAll(body, "<临时目录>", ScratchDir(key))
	return body, nil
}

// ScratchDir 是这一档给模型放辅助脚本与中间文件的目录（相对仓库根）。
//
// 为什么在仓库里：无人值守没有审批面，要审批的调用当场被拒。claude 的 Write 写工作
// 目录之外要审批；在 Bash 里用 heredoc / python3 -c 内联带引号花括号的代码（字典、
// f-string）会被 CLI 的命令解析器判成「展开混淆」，也要审批。2026-10-19 写进 prompt
// 的「放 /tmp，或直接在 Bash 里内联」两条路，10-20 那档 trending 都撞了一遍，最后
// 把脚本写在了仓库根上。仓库里是两层都放行的地方：Write 在工作目录内自动通过，沙箱
// 对仓库根放行写入。
func ScratchDir(key string) string { return "tmp/" + key }

// resetScratch 清空 <root>/tmp/<key>；keep 为真时再建一个空的（开跑前），为假时只删（收尾）。
//
// tmp 本身必须是真目录。分析层能在仓库里建符号链接，而删东西的是沙箱外的这个进程：
// 跟着一条指向仓库外的 tmp 链接去删 tmp/<key>，删掉的就是仓库外的目录。所以 tmp 不是
// 目录就先删它本身（删的是链接，不是目标）；最后一级是链接时 RemoveAll 也只摘链接。
func resetScratch(root, key string, keep bool) error {
	base := filepath.Join(root, "tmp")
	if fi, err := os.Lstat(base); err == nil && !fi.IsDir() {
		if err := os.Remove(base); err != nil {
			return err
		}
	}
	dir := filepath.Join(base, key)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if !keep {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

type AnalyzeResult struct {
	Engine   engine.Kind
	Attempts int
	Failure  engine.Failure
	Output   string // 末尾若干 KB，用于分类与告警
	// Usage 最后一次尝试的用量（成功时就是写出报告的那次）。
	Usage engine.Usage
	// FellBack 主引擎失败、这份报告由兜底跑出；PrimaryFailure 是主引擎当时的失败类别。
	// 兜底成功时流水线看起来一切正常，只有这两项能把「切过引擎」这件事带给调度器。
	FellBack       bool
	PrimaryFailure engine.Failure
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
	// 不进版本控制的目录里落了什么，git status 看不见，不删就一直堆着。
	defer func() {
		if err := resetScratch(root, t.Key, false); err != nil {
			fmt.Fprintf(out, "  !! 清理临时目录 %s 失败：%v\n", ScratchDir(t.Key), err)
		}
	}()
	for _, kind := range []engine.Kind{primary, fallback} {
		if kind == "" {
			continue
		}
		res.Engine, res.Attempts = kind, res.Attempts+1
		// 每次尝试都从空目录开始：兜底不该接着用主引擎留下的半截脚本。
		// 建不出来不拦着跑——Write 会自己建上级目录，只是少了「开跑前是空的」这一条。
		if err := resetScratch(root, t.Key, true); err != nil {
			fmt.Fprintf(out, "  !! 准备临时目录 %s 失败：%v\n", ScratchDir(t.Key), err)
		}
		// 报告是哪个引擎、哪个模型写的，必须留在这一档自己的日志里。
		// 切了兜底而日志只有一句「完成」的话，风格突变就只能靠猜。
		fmt.Fprintf(out, "  引擎: %s\n", engine.Describe(cfg, kind))
		ring := &ringBuffer{max: 16 << 10}
		r := &engine.Runner{Home: home, Root: root, Cfg: cfg, Out: io.MultiWriter(out, ring)}

		runCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TaskTimeoutMin)*time.Minute)
		u, err := r.Run(runCtx, kind, prompt)
		cancel()
		res.Output, res.Usage = ring.String(), u

		if err == nil {
			// 引擎退出码为 0 不等于报告写出来了：模型可能「完成」了但没落盘。
			// 只认文件，不认退出码。
			if _, ok := ReportPath(root, date, t); ok {
				res.Failure = ""
				res.FellBack = kind != primary
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
			res.PrimaryFailure = res.Failure
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
