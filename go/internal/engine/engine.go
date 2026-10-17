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

// Run 把 prompt 交给引擎，输出直接流到 stdout/stderr（调用方已重定向到日志）。
//
// 危险模式（config 里的 DANGER=1）在这里生效：权限文件换成不设限制的那一份，
// 去掉权限提示与 MCP 裁剪。判定从配置现取，所以同一个进程里跑不同配置的档
// （调度器热重读）不会串。
func (r *Runner) Run(ctx context.Context, kind Kind, prompt string) error {
	danger := r.Cfg.Danger()
	switch kind {
	case Claude:
		settings, err := sandbox.WriteSettings(r.Home, r.Root, danger)
		if err != nil {
			return fmt.Errorf("渲染权限文件: %w", err)
		}
		model := r.Cfg.Get("CLAUDE_MODEL", "claude-sonnet-5")
		if danger {
			// 危险模式：彻底不设限。--dangerously-skip-permissions 绕过一切权限检查，
			// 权限文件里也把 disableBypassPermissionsMode 去掉了；同时**不再传
			// --strict-mcp-config**——交互式会话里挂着的 MCP server 是独立进程、
			// 不在这层沙箱里，默认要挡住的就是它们，而这里正是要放开。
			return r.exec(ctx, nil, "claude", "-p",
				"--model", model, "--settings", settings,
				"--dangerously-skip-permissions", prompt)
		}
		// 不用 --dangerously-skip-permissions：那会绕过所有权限检查。
		//
		// --strict-mcp-config 不是可选的：没有它，分析层会继承交互式会话里的
		// 全局 MCP server。MCP server 是独立进程、不在这层沙箱里，所以
		// network.allowedDomains: [] 对它完全无效——「分析层出不了网」这个前提
		// 会被悄悄推翻，而它每天无人值守地读别人能写的文本。
		return r.exec(ctx, nil, "claude", "-p",
			"--model", model,
			"--settings", settings, "--permission-prompts", "none",
			"--strict-mcp-config", prompt)

	case API:
		if err := r.Cfg.Require("FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"); err != nil {
			return err
		}
		settings, err := sandbox.WriteSettings(r.Home, r.Root, danger)
		if err != nil {
			return err
		}
		// 端点地址归一化：claude 自己会拼 /v1/messages，配置里若带了 /v1
		// 就会变成 /v1/v1/messages（实测 404）。
		base := strings.TrimSuffix(strings.TrimRight(r.Cfg.Get("FALLBACK_BASE_URL", ""), "/"), "/v1")
		env := []string{
			"ANTHROPIC_BASE_URL=" + base,
			"ANTHROPIC_AUTH_TOKEN=" + r.Cfg.Get("FALLBACK_AUTH_TOKEN", ""),
		}
		model := r.Cfg.Get("FALLBACK_MODEL", "")
		if danger {
			return r.exec(ctx, env, "claude", "-p",
				"--model", model, "--settings", settings,
				"--dangerously-skip-permissions", prompt)
		}
		return r.exec(ctx, env, "claude", "-p",
			"--model", model,
			"--settings", settings, "--permission-prompts", "none",
			"--strict-mcp-config", prompt)

	case Codex:
		if danger {
			// 危险模式：不套外层沙箱，整个 codex 进程不受限
			return r.exec(ctx, nil, "codex", "exec",
				"-m", r.Cfg.Get("CODEX_MODEL", "gpt-5.6-luna"), "-C", r.Root,
				"--dangerously-bypass-approvals-and-sandbox", "--color", "never", prompt)
		}
		wrap, err := sandbox.Wrapper(r.Home, r.Root, "codex")
		if err != nil {
			// 没有可用沙箱就拒绝：codex 自带的 workspace-write 不限制读，
			// 无外层沙箱等于无防护
			return err
		}
		argv := append(wrap, "codex", "exec",
			"-m", r.Cfg.Get("CODEX_MODEL", "gpt-5.6-luna"), "-C", r.Root,
			// 自带那层关掉，避免与外层沙箱套娃
			"--dangerously-bypass-approvals-and-sandbox", "--color", "never", prompt)
		return r.exec(ctx, nil, argv[0], argv[1:]...)

	default:
		return fmt.Errorf("未知引擎 %q（可选 claude | codex | api）", kind)
	}
}

func (r *Runner) exec(ctx context.Context, extraEnv []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Root
	cmd.Env = append(os.Environ(), extraEnv...)
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
// 有三类失败**重试帮不上忙**，共同点是「等几分钟再来一次必然还是失败」，
// 但处置提示不一样，所以分开认——余额不足被报成配额耗尽的话，提示会是
// 「等窗口重置」，而那是永远等不到的。
type Failure string

const (
	FailQuota   Failure = "quota"   // 订阅配额：窗口没重置
	FailBalance Failure = "balance" // 按量付费没钱了
	FailAuth    Failure = "auth"    // key 过期/撤销/写错
	FailOther   Failure = "other"
)

var hints = map[Failure][2]string{
	FailQuota:   {"配额耗尽", "主引擎的用量窗口还没重置。等窗口，或把 ENGINE 换成额度更宽的那个。"},
	FailBalance: {"余额不足", "按量付费那边没钱了或触发了计费限制。去供应商后台充值/提额，这个靠等是不会好的。"},
	FailAuth:    {"凭据失效", "key 过期、被撤销或写错了。检查 config.conf 里的 FALLBACK_AUTH_TOKEN，重试帮不上忙。"},
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
