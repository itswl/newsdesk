package pipeline

import (
	"context"
	"os/exec"
	"strings"

	"github.com/itswl/newsdesk/internal/config"
)

// githubToken 令牌是可选的。
//
// Trending 采集按约 50 次 API 调用设计，为的是装进未认证的 60 次/小时预算。
// 没令牌照样出报告，只是深度受限并在报告里说明缺口。别让它变回必需。
//
// 配置留空时，若本机装了 gh 且已登录就借用它的令牌；显式填 none 则强制不认证。
func githubToken(ctx context.Context, cfg *config.Config) string {
	t := strings.TrimSpace(cfg.Get("GITHUB_TOKEN", ""))
	switch t {
	case "none":
		return ""
	case "":
		out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return t
}
