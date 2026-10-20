package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/i18n"
	"github.com/itswl/newsdesk/internal/langs"
	"github.com/itswl/newsdesk/internal/leak"
	"github.com/itswl/newsdesk/internal/publish"
	"github.com/itswl/newsdesk/internal/site"
	"github.com/itswl/newsdesk/internal/translate"
)

// Translate 把当天的报告翻成英文。
//
// 只在英文真的会被发布时才跑：每天约十万 token 的按量付费开销，
// 为一个没人看得到的语言花钱没意义。
func Translate(ctx context.Context, root, date string, cfg *config.Config, out io.Writer) (translate.Usage, error) {
	eff := langs.Effective(cfg, root)
	want := false
	for _, l := range eff {
		if l == i18n.En {
			want = true
		}
	}
	if !want {
		return translate.Usage{}, nil
	}
	c := translate.New(cfg.Get("FALLBACK_BASE_URL", ""),
		cfg.Get("FALLBACK_AUTH_TOKEN", ""), cfg.Get("FALLBACK_MODEL", ""))
	c.Out = out // 翻译花了多少要进这一档自己的日志，不能只落在 scheduler.log 里
	fmt.Fprintf(out, "翻译 %s 的报告 → en\n", date)
	return c.Day(ctx, root, date, false)
}

// Site 生成全部启用语言的站点。
//
// 生成两份：本地 site/ 是全量（自己翻旧账用），发布用的那份按 PUBLIC_DAYS
// 截断。分开而不是「本地也截断」，是因为原始报告必须留着——
// 对外只展示最近 N 天，不代表历史要被删。
func Site(root string, cfg *config.Config, out io.Writer) (localDir, publicDir string, err error) {
	eff := langs.Effective(cfg, root)
	if len(eff) == 0 {
		return "", "", errors.New("没有可用的语言（检查 SITE_LANGS）")
	}
	siteURL := cfg.Get("SITE_URL", "")
	contact := cfg.Get("CONTACT", "")
	pushKey := cfg.Get("PUSH_VAPID_PUBLIC", "")
	localDir = filepath.Join(root, "site")

	build := func(dir string, days int) error {
		for _, l := range eff {
			o := site.Options{
				Root: root, Out: langs.SubDir(dir, l, eff), Days: days,
				Lang: l, AllLangs: eff, SiteURL: siteURL, Contact: contact, PushKey: pushKey, Log: out,
			}
			if err := site.Build(o); err != nil {
				return fmt.Errorf("生成 %s 站点: %w", l, err)
			}
		}
		return nil
	}
	if err := build(localDir, 0); err != nil {
		return "", "", err
	}

	days := cfg.Int("PUBLIC_DAYS", 0)
	if days <= 0 {
		// 不限制窗口时两份是一样的，没必要生成两遍
		return localDir, localDir, nil
	}
	publicDir = filepath.Join(root, "state", "publish")
	// 必须先清空：上一次窗口里的日期页现在超窗了，留在目录里会被原样同步上去。
	if err := os.RemoveAll(publicDir); err != nil {
		return "", "", err
	}
	if err := build(publicDir, days); err != nil {
		return "", "", err
	}
	fmt.Fprintf(out, "对外窗口：最近 %d 天\n", days)
	return localDir, publicDir, nil
}

// secretValues 从配置和环境里收集「真实值」，供产出自检比对。
// 太短的要剔掉——none / 0 / true 这种会命中一切，把自检变成噪音。
func secretValues(cfg *config.Config) map[string]string {
	out := map[string]string{}
	add := func(name, v string) {
		if len(strings.TrimSpace(v)) >= 12 {
			out[name] = strings.TrimSpace(v)
		}
	}
	for k, v := range cfg.All() {
		u := strings.ToUpper(k)
		if strings.Contains(u, "TOKEN") || strings.Contains(u, "KEY") ||
			strings.Contains(u, "SECRET") || strings.Contains(u, "WEBHOOK") ||
			strings.Contains(u, "PASSWORD") {
			add(k, v)
		}
	}
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY",
		"GITHUB_TOKEN", "GH_TOKEN", "CLOUDFLARE_API_TOKEN"} {
		add(k, os.Getenv(k))
	}
	return out
}

// Republish 重建站点、自检、发布，不跑采集与分析，也就不调模型。
//
// newsdesk publish 与调度器（一档跑的途中配置改过，跑完按新配置重发）共用这一份，
// 「先自检、后发布」的顺序只写在一处。
func Republish(ctx context.Context, root string, cfg *config.Config, dry bool, out io.Writer) error {
	localDir, publicDir, err := Site(root, cfg, out)
	if err != nil {
		return err
	}
	// 发布前必须自检。放到发布之后等于没有自检。
	dirs := []string{localDir, filepath.Join(root, "reports")}
	if publicDir != "" && publicDir != localDir {
		dirs = append(dirs, publicDir)
	}
	if err := LeakGate(root, cfg, out, dirs...); err != nil {
		return err
	}
	return Publish(ctx, root, publicDir, cfg, dry, out)
}

// LeakCheck 发布前扫产出。命中就拒绝发布——报告第二天就在公开站点上，
// 沙箱禁网拦不住「写进报告」这条路径。
//
// out 是结果写到哪（任务日志或终端）；nil 时写标准输出。
func LeakCheck(root string, cfg *config.Config, out io.Writer, dirs ...string) error {
	hits := leak.Scan(dirs, secretValues(cfg))
	if leak.Report(out, root, hits) {
		return fmt.Errorf("产出里发现 %d 处疑似凭据，已拒绝发布", len(hits))
	}
	return nil
}

// LeakGate 是发布路径上的自检闸门：发布前调用它，而不是直接调 LeakCheck。
//
// ⚠️ DANGER=1 时**整段跳过**。这是危险模式明确要的一部分——「不限制能力」也包括
// 不拦「把凭据写进公开产出」这一步。跳过必须留一行，不能静默：这是整条链上唯一
// 不可逆的动作（发布之后报告就在公开站点上了）。
//
// 想要无条件自检（不认 DANGER）请直接调 LeakCheck：newsdesk leakcheck 就是这么用的。
func LeakGate(root string, cfg *config.Config, out io.Writer, dirs ...string) error {
	if cfg.Danger() {
		if out != nil {
			fmt.Fprintln(out, "  ⚠ DANGER=1：跳过发布前凭据自检，产出原样发布")
		}
		return nil
	}
	return LeakCheck(root, cfg, out, dirs...)
}

// Publish 同步到对象存储。没配桶就跳过，不是错误。
func Publish(ctx context.Context, root, publicDir string, cfg *config.Config, dry bool, out io.Writer) error {
	_, err := publish.Run(ctx, publish.Options{
		Root: root, Cfg: cfg, PublicDir: publicDir, DryRun: dry,
		Log: func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) },
	})
	return err
}
