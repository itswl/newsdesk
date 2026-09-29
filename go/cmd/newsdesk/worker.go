package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// cmdWorker 在 ts/ 下跑 wrangler，令牌取自 config.conf 的 CLOUDFLARE_API_TOKEN。
//
// 为什么要有：以前部署 Worker 得把令牌贴进对话或命令行，贴出去过的令牌只能作废。
// 放进 config.conf 的话，它和其他凭据一样被分析层的沙箱拒读、被发布前自检收进扫描
// 名单（名字里有 TOKEN）、不进版本控制。令牌只经环境变量交给 wrangler 这一个子进程，
// 不打印，也不进调度器和引擎的环境。
//
//	newsdesk worker deploy
//	newsdesk worker kv key get log:push --binding PUSH --remote
func cmdWorker(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("用法：newsdesk worker <wrangler 参数>，例如 newsdesk worker deploy")
	}
	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "npx", append([]string{"wrangler"}, args...)...)
	cmd.Dir = filepath.Join(root, "ts")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	// 没配就交给 wrangler 自己的登录态（wrangler login）。配了就以它为准：
	// 环境里有同名变量时，exec 取切片里的最后一个
	if tok := cfg.Get("CLOUDFLARE_API_TOKEN", ""); tok != "" {
		cmd.Env = append(cmd.Env, "CLOUDFLARE_API_TOKEN="+tok)
	}
	return cmd.Run()
}
