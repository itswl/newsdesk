// newsdesk —— 采集、调度、渲染。唯一的本地可执行文件。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/itswl/newsdesk2/internal/config"
	"github.com/itswl/newsdesk2/internal/fetch"
	"github.com/itswl/newsdesk2/internal/history"
	"github.com/itswl/newsdesk2/internal/i18n"
	"github.com/itswl/newsdesk2/internal/site"
	"github.com/itswl/newsdesk2/internal/store"
)

const usage = `用法: newsdesk <命令> [参数]

  fetch <源>      采集。源: douban | momoyu | trending | ai
  history <源>    跨期比对，产出 history.json
  site            渲染静态站（按 SITE_LANGS 生成各语言）
  langs           打印实际会发布哪些语言、被剔除的为什么

全局参数:
  -root DIR       仓库根目录，缺省取可执行文件所在目录的上级
  -date YYYY-MM-DD
`

func main() {
	opts := parseGlobals()
	if len(opts.args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cfg, err := config.Load(filepath.Join(opts.root, "config.conf"))
	if err != nil {
		fatal("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch opts.args[0] {
	case "fetch":
		runFetch(ctx, opts, cfg)
	case "site":
		runSite(opts, cfg)
	case "history":
		if len(opts.args) < 2 {
			fatal("用法: newsdesk history <ainews|momoyu|douban|trending>")
		}
		if err := history.Build(store.New(opts.root), opts.args[1], opts.date); err != nil {
			fatal("%v", err)
		}
	case "langs":
		for _, l := range effectiveLangs(cfg) {
			fmt.Println(l)
		}
	default:
		fatal("未知命令 %q\n%s", opts.args[0], usage)
	}
}

type globals struct {
	root, date string
	args       []string
	tag        string
	n          int
	tier       string
	hours      int
}

func parseGlobals() globals {
	g := globals{date: store.Today(), tag: "热门", n: 10, tier: "t3", hours: 36}
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		k := strings.TrimLeft(args[0], "-")
		if len(args) < 2 {
			fatal("参数 -%s 缺少值", k)
		}
		v := args[1]
		switch k {
		case "root":
			g.root = v
		case "date":
			g.date = v
		case "tag":
			g.tag = v
		case "n":
			fmt.Sscanf(v, "%d", &g.n)
		case "tier":
			g.tier = v
		case "hours":
			fmt.Sscanf(v, "%d", &g.hours)
		default:
			fatal("未知参数 -%s", k)
		}
		args = args[2:]
	}
	g.args = args
	if g.root == "" {
		exe, err := os.Executable()
		if err != nil {
			fatal("取可执行文件路径: %v", err)
		}
		g.root = filepath.Dir(filepath.Dir(exe))
	}
	return g
}

func runFetch(ctx context.Context, g globals, cfg *config.Config) {
	if len(g.args) < 2 {
		fatal("用法: newsdesk fetch <douban|momoyu|trending|ai>")
	}
	st := store.New(g.root)
	c := fetch.NewClient(30 * time.Second)
	var err error
	switch g.args[1] {
	case "douban":
		err = fetch.Douban(ctx, c, st, g.date, fetch.DoubanOpts{Tag: g.tag, N: g.n})
	case "momoyu":
		err = fetch.Momoyu(ctx, c, st)
	case "trending":
		gh := fetch.NewGitHub(c, cfg.Get("GITHUB_TOKEN", ""))
		err = fetch.Trending(ctx, c, gh, st, g.date, g.tier)
	case "ai":
		err = fetch.AINews(ctx, c, st, g.date, fetch.AIOpts{Hours: g.hours})
	default:
		fatal("未知数据源 %q", g.args[1])
	}
	if err != nil {
		fatal("%v", err)
	}
}

// effectiveLangs 「列进配置」不等于「开启」。
// 英文缺 API 配置就翻不出东西，硬构建出来是个所有报告都缺失的空站——
// 那比没有这个语言糟糕得多。
func effectiveLangs(cfg *config.Config) []i18n.Lang {
	want := cfg.List("SITE_LANGS")
	if len(want) == 0 {
		return []i18n.Lang{i18n.ZhCN}
	}
	var keep []i18n.Lang
	seen := map[string]bool{}
	for _, s := range want {
		if !i18n.Valid(s) || seen[s] {
			if !i18n.Valid(s) {
				fmt.Fprintf(os.Stderr, "!! 剔除 %s：不认识这个语言代码\n", s)
			}
			continue
		}
		if s == string(i18n.En) {
			if err := cfg.Require("FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"); err != nil {
				fmt.Fprintf(os.Stderr, "!! 剔除 en：%v——没有这些就翻不出英文，"+
					"构建出来会是一个所有报告都缺失的空站\n", err)
				continue
			}
		}
		seen[s] = true
		keep = append(keep, i18n.Lang(s))
	}
	if len(keep) == 0 {
		return []i18n.Lang{i18n.ZhCN}
	}
	return keep
}

func runSite(g globals, cfg *config.Config) {
	langs := effectiveLangs(cfg)
	days := cfg.Int("PUBLIC_DAYS", 0)
	base := filepath.Join(g.root, "site")
	// 本地全量 + 对外限窗是两份产出。不做成一份然后「只上传一部分」——
	// 那样页面里的日期下拉和归档页仍会链到没上传的日子，点进去 404。
	full := site.Options{Root: g.root, Out: base, Lang: i18n.ZhCN,
		SiteURL: cfg.Get("SITE_URL", ""), Contact: cfg.Get("CONTACT", "")}
	if err := site.Build(full); err != nil {
		fatal("%v", err)
	}
	if days <= 0 {
		return
	}
	for _, l := range langs {
		out := filepath.Join(g.root, "site-public")
		if l != langs[0] {
			out = filepath.Join(out, string(l))
		}
		o := site.Options{Root: g.root, Out: out, Days: days, Lang: l, AllLangs: langs,
			SiteURL: cfg.Get("SITE_URL", ""), Contact: cfg.Get("CONTACT", "")}
		if err := site.Build(o); err != nil {
			fatal("%v", err)
		}
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "!! "+f+"\n", a...)
	os.Exit(1)
}
