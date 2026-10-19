// Package engine 调用分析引擎（claude / codex / 第三方 API 端点）。
//
// 每个引擎恰好一层沙箱，机制不同但边界等价：
//
//	claude —— 用它自带的沙箱（跨平台，权限文件由 internal/sandbox 渲染）
//	codex  —— 自带的只限制写不限制读，改用外层 seatbelt / bubblewrap
//
// ⚠️ 不能给同一个引擎套两层沙箱：两边在 macOS 上都是 seatbelt，嵌套会让 Bash
// 整个失效（sandbox_apply: Operation not permitted，连 echo 都跑不了），
// 而模型会靠 Read/Write 工具绕过去把任务做完，日志上看不出异常。
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/sandbox"
)

type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
	API    Kind = "api" // claude CLI 连第三方 Anthropic 兼容端点
)

type Runner struct {
	Home, Root string
	Cfg        *config.Config
	Out        io.Writer // 引擎输出去向；nil = os.Stdout
}

// toolsAllowed 分析层允许用到的工具，显式列出来把工具面钉死。
//
// 不钉的话工具面是会漂的：交互式会话的全局设置里开着 ENABLE_TOOL_SEARCH（动态
// 工具加载），而网关不是 Anthropic 第一方，初始工具面里文件工具可能根本不出现。
// 实测 10-15 那档豆瓣就是这么交的白卷——模型自述「有联网工具、没有文件工具」，
// 白跑 2m57s 靠重试才补上。
//
// 只给分析真正用得上的：读数据、写报告、必要时跑个脚本核对。
const toolsAllowed = "Read,Write,Edit,Glob,Grep,Bash"

// claudeArgs 拼 claude CLI 的命令行。
//
// 单独拎出来是为了能测：这串参数里每一条都是一个开关，写漏一条不会有任何报错，
// 只会安静地少一层防护或多继承一层配置。
func claudeArgs(kind Kind, cfg *config.Config, settings, prompt string, danger bool) []string {
	def, key := "claude-sonnet-5", "CLAUDE_MODEL"
	if kind == API {
		def, key = "", "FALLBACK_MODEL"
	}
	model := cfg.Get(key, def)
	if danger {
		// 危险模式不设限：不钉工具面、不裁 MCP、也不砍设置来源——交互式会话里的
		// 全局 MCP server 与技能正是「不限制能力」的一部分。
		return []string{"-p", "--model", model, "--settings", settings,
			"--dangerously-skip-permissions",
			"--output-format", "stream-json", "--verbose", prompt}
	}
	return []string{"-p", "--model", model, "--settings", settings,
		"--permission-prompts", "none",
		"--tools", toolsAllowed,
		// 不加载 user/project/local 三处设置。加载的话会连带继承交互式会话的全局
		// env（ENABLE_TOOL_SEARCH 打开动态工具加载、agent teams 多出一批工具）、
		// 11 个技能与已装插件——实测每次调用输入多约 65%（17,431 → 6,171 token）。
		// ⚠️ --settings 指定的是「additional settings」，不受这条影响：下面那份
		// 权限文件（含凭据拒读）照常生效，实测 deny 规则仍是 49 条。
		"--setting-sources", "",
		"--strict-mcp-config",
		// 流式 JSON 是为了拿 token 统计（in/out/cache）。renderStream 负责把它还原
		// 成人看的日志，所以日志形状不变。
		"--output-format", "stream-json", "--verbose", prompt}
}

// Run 把 prompt 交给引擎，输出直接流到 stdout/stderr（调用方已重定向到日志）。
// 返回这次调用的用量（走 claude CLI 的引擎才有；codex 没有，Known 为假）。
//
// 危险模式（config 里的 DANGER=1）在这里生效：权限文件换成不设限制的那一份，
// 去掉权限提示、工具白名单与 MCP 裁剪。判定从配置现取，所以同一个进程里跑不同
// 配置的档（调度器热重读）不会串。
func (r *Runner) Run(ctx context.Context, kind Kind, prompt string) (Usage, error) {
	danger := r.Cfg.Danger()
	switch kind {
	case Claude:
		settings, err := sandbox.WriteSettings(r.Home, r.Root, danger)
		if err != nil {
			return Usage{}, fmt.Errorf("渲染权限文件: %w", err)
		}
		return r.execStream(ctx, nil, claudeArgs(Claude, r.Cfg, settings, prompt, danger)...)

	case API:
		if err := r.Cfg.Require("FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"); err != nil {
			return Usage{}, err
		}
		settings, err := sandbox.WriteSettings(r.Home, r.Root, danger)
		if err != nil {
			return Usage{}, err
		}
		// 端点地址归一化：claude 自己会拼 /v1/messages，配置里若带了 /v1
		// 就会变成 /v1/v1/messages（实测 404）。
		base := strings.TrimSuffix(strings.TrimRight(r.Cfg.Get("FALLBACK_BASE_URL", ""), "/"), "/v1")
		env := []string{
			"ANTHROPIC_BASE_URL=" + base,
			"ANTHROPIC_AUTH_TOKEN=" + r.Cfg.Get("FALLBACK_AUTH_TOKEN", ""),
		}
		return r.execStream(ctx, env, claudeArgs(API, r.Cfg, settings, prompt, danger)...)

	case Codex:
		if danger {
			// 危险模式：不套外层沙箱，整个 codex 进程不受限
			return Usage{}, r.exec(ctx, nil, "codex", "exec",
				"-m", r.Cfg.Get("CODEX_MODEL", "gpt-5.6-luna"), "-C", r.Root,
				"--dangerously-bypass-approvals-and-sandbox", "--color", "never", prompt)
		}
		wrap, err := sandbox.Wrapper(r.Home, r.Root, "codex")
		if err != nil {
			// 没有可用沙箱就拒绝：codex 自带的 workspace-write 不限制读，
			// 无外层沙箱等于无防护
			return Usage{}, err
		}
		argv := append(wrap, "codex", "exec",
			"-m", r.Cfg.Get("CODEX_MODEL", "gpt-5.6-luna"), "-C", r.Root,
			// 自带那层关掉，避免与外层沙箱套娃
			"--dangerously-bypass-approvals-and-sandbox", "--color", "never", prompt)
		return Usage{}, r.exec(ctx, nil, argv[0], argv[1:]...)

	default:
		return Usage{}, fmt.Errorf("未知引擎 %q（可选 claude | codex | api）", kind)
	}
}

// lockedWriter 把写入串起来。
//
// ⚠️ execStream 有两条路往同一个 writer 写：stderr 由 os/exec 自己拷，stdout 由
// renderStream 解析后写。两者并发，而 bytes.Buffer、ring buffer、MultiWriter 都
// 不是并发安全的——实测丢过一整行模型输出（日志里只剩 stderr 和用量行）。
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// childEnv 给引擎子进程的环境：继承自己的，但把 ANTHROPIC_* 与 CLAUDE* 全部剔掉，
// 再加上调用方显式给的那几条。
//
// 调度器继承的是启动它的那个进程的环境，而那可能是一个 Claude Code 会话：2026-10-19 从
// 会话里重启调度器之后，CLAUDE_CODE_EFFORT_LEVEL=max、CLAUDECODE=1、CLAUDE_CODE_CHILD_SESSION=1
// 和会话的消息令牌一起流进了每一档的 claude 调用——次日 06:30 那档思考 token 占输出 80%
// （69k / 86k），跑了 17 分钟，而正常那档是 35%。效果上等于有人偷偷把推理档位拧到最大。
// 反过来也一样：谁的 shell 里要是 export 了 ANTHROPIC_API_KEY 或 ANTHROPIC_BASE_URL，
// 订阅那次就会被它劫走（CLI 明说 API key 优先于登录态），甚至把登录态发往别的端点。
// 所以引擎的环境只认两样：自己注入的（api 的端点与令牌），和与这两家无关的其他变量。
// CLAUDE_CONFIG_DIR 留着：它指的是登录态在哪，拿掉会把认证掐断。
func childEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDE_CONFIG_DIR" {
			env = append(env, kv)
			continue
		}
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// execStream 跑 claude 并把 stream-json 还原成可读日志，末尾补一行用量。
func (r *Runner) execStream(ctx context.Context, extraEnv []string, args ...string) (Usage, error) {
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = r.Root
	cmd.Env = childEnv(extraEnv)
	out := r.Out
	if out == nil {
		out = os.Stdout
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return Usage{}, err
	}
	lw := &lockedWriter{w: out}
	// stderr 直接进日志：模型告警、连接提示都在这儿
	cmd.Stderr = lw
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return Usage{}, err
	}
	u, rerr := renderStream(pipe, lw)
	werr := cmd.Wait()
	// 用量行恒打印：没拿到 result（进程被信号杀掉、输出被截断）就写「没取到」。
	// 不能写 0，也不能不写——「一直是 0」和「从来没有这一行」都不会有人发现。
	fmt.Fprintln(lw, u.String())
	if werr != nil {
		return u, werr
	}
	if rerr != nil {
		// 读输出中途出错但进程正常退出：不因此判失败（报告可能已经写好），但要留痕
		fmt.Fprintf(lw, "!! 读取输出中断（%v），用量可能不准\n", rerr)
	}
	return u, nil
}

func (r *Runner) exec(ctx context.Context, extraEnv []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Root
	cmd.Env = childEnv(extraEnv)
	out := r.Out
	if out == nil {
		out = os.Stdout
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Stdin = nil // 防止引擎等 stdin
	return cmd.Run()
}

// Classify 把失败信息归类。
//
// 有四类失败**重试帮不上忙**，共同点是「等几分钟再来一次必然还是失败」，
// 但处置提示不一样，所以分开认——余额不足被报成配额耗尽的话，提示会是
// 「等窗口重置」，而那是永远等不到的。内容审核同理：同一份输入再发一遍还是被拒。
type Failure string

const (
	FailQuota   Failure = "quota"   // 订阅配额：窗口没重置
	FailBalance Failure = "balance" // 按量付费没钱了
	FailAuth    Failure = "auth"    // key 过期/撤销/写错
	// FailModeration 供应商把这次生成判为敏感。它不是我们的错，也不是重试能解决的：
	// 2026-10-17 21:45 那档 momoyu 就是被智谱这样拒了三次（400 [1301]），
	// 而当时的告警只写了「原因：失败」——因为这一类没被认出来。
	FailModeration Failure = "moderation"
	FailOther      Failure = "other"
)

var hints = map[Failure][2]string{
	FailQuota:   {"配额耗尽", "主引擎的用量窗口还没重置。等窗口，或把 ENGINE 换成额度更宽的那个。"},
	FailBalance: {"余额不足", "按量付费那边没钱了或触发了计费限制。去供应商后台充值/提额，这个靠等是不会好的。"},
	FailAuth:    {"凭据失效", "key 过期、被撤销或写错了。检查 config.conf 里的 FALLBACK_AUTH_TOKEN，重试帮不上忙。"},
	FailModeration: {"内容审核拒绝", "供应商把这次生成判成敏感内容了，同一份输入重试还是会被拒。" +
		"等主引擎的窗口重置，或换一个内容策略不同的兜底。"},
}

func (f Failure) Hint() (label, action string) {
	if h, ok := hints[f]; ok {
		return h[0], h[1]
	}
	return "失败", ""
}

// Classify 顺序有讲究：余额与凭据的报错里常常也带 quota / limit 字样，
// 先认更具体的那个。
func Classify(out string) Failure {
	l := strings.ToLower(out)
	has := func(ss ...string) bool {
		for _, s := range ss {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
	// 内容审核先认：这类报错里常夹着 "400"、"invalid" 之类的词，落到下面任何一类
	// 都会给出错的处置提示（比如让人去等窗口重置）。
	if has("[1301]", "系统检测到输入或生成内容", "敏感内容",
		"content_filter", "content policy", "content_moderation", "content moderation",
		"safety system") {
		return FailModeration
	}
	if has("insufficient balance", "insufficient funds", "insufficient credit",
		"insufficient quota", "payment required", "billing", "arrears",
		"余额不足", "额度不足", "欠费", "请充值") {
		return FailBalance
	}
	if has("invalid api key", "invalid token", "invalid credential", "api key expired",
		"token expired", "unauthorized", "authentication failed", "authentication error",
		"failed to authenticate", "oauth session expired", "session expired", "could not be refreshed",
		"disabled claude subscription", "disabled subscription",
		"密钥无效", "认证失败") {
		return FailAuth
	}
	if has("usage limit", "rate limit", "rate_limit", "quota", "too many requests",
		"429", "5-hour", "limit reached", "weekly limit", "session limit",
		"hit your limit", "hit your weekly limit", "hit your session limit",
		"resets at", "resets in", "resets ",
		"overloaded", "capacity") {
		return FailQuota
	}
	return FailOther
}

// Model 某个引擎实际会用的模型名。
func Model(cfg *config.Config, kind Kind) string {
	switch kind {
	case Claude:
		return cfg.Get("CLAUDE_MODEL", "claude-sonnet-5")
	case Codex:
		return cfg.Get("CODEX_MODEL", "gpt-5.6-luna")
	case API:
		return cfg.Get("FALLBACK_MODEL", "")
	}
	return ""
}

// Describe 一行说清「这次会用谁、用哪个模型」。
//
// 无人值守跑的东西，最难查的问题之一是「报告是哪个引擎写的」——切了兜底而
// 日志里只有一句「完成」，风格突变就只能靠猜。所以启动、每次调用、每次完成
// 都把引擎和模型显式打出来。
//
// api 走的仍是 claude CLI，只是换了端点，必须连端点一起报：同一个 "api"
// 指向不同网关时产出可以完全不同。
func Describe(cfg *config.Config, kind Kind) string {
	m := Model(cfg, kind)
	switch kind {
	case Claude, Codex:
		return fmt.Sprintf("%s / %s", kind, m)
	case API:
		base := cfg.Get("FALLBACK_BASE_URL", "")
		if base == "" {
			return fmt.Sprintf("api / %s（端点未配置）", m)
		}
		return fmt.Sprintf("api / %s @ %s", m, host(base))
	case "":
		return "未配置"
	}
	return string(kind)
}

// host 只取主机名。端点里可能带路径，全量打出来太长；也避免把查询串里
// 可能夹带的东西写进日志。
func host(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i >= 0 { // 万一端点里写了 user:pass@
		s = s[i+1:]
	}
	return s
}

// Plan 配置里定的主引擎与兜底引擎。
func Plan(cfg *config.Config) (primary, fallback Kind) {
	return Kind(cfg.Get("ENGINE", "claude")), Kind(cfg.Get("ENGINE_FALLBACK", ""))
}

// PlanLine 启动时打的那一行。
func PlanLine(cfg *config.Config) string {
	p, f := Plan(cfg)
	s := "引擎：" + Describe(cfg, p)
	if f == "" {
		// 没有兜底是个该知道的事实：主引擎配额耗尽时那一档直接跳过
		return s + "   兜底：未配置（主引擎失败即跳过当档）"
	}
	return s + "   兜底：" + Describe(cfg, f)
}
