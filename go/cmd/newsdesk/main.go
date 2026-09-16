// newsdesk —— 采集与调度的入口。渲染那半在 ts/。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/itswl/newsdesk2/internal/config"
	"github.com/itswl/newsdesk2/internal/fetch"
	"github.com/itswl/newsdesk2/internal/store"
)

func main() {
	root := flag.String("root", "", "仓库根目录，缺省取可执行文件所在目录的上级")
	date := flag.String("date", store.Today(), "数据日期 YYYY-MM-DD")
	tag := flag.String("tag", "热门", "豆瓣标签")
	n := flag.Int("n", 10, "取多少部")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: newsdesk [参数] fetch douban\n\n参数:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 || args[0] != "fetch" {
		flag.Usage()
		os.Exit(2)
	}

	r := *root
	if r == "" {
		exe, err := os.Executable()
		if err != nil {
			fatal("取可执行文件路径: %v", err)
		}
		r = filepath.Dir(filepath.Dir(exe))
	}
	// 配置现在只是校验能读通；后面的采集器会用到里面的项
	if _, err := config.Load(filepath.Join(r, "bin", "config.conf")); err != nil {
		fatal("%v", err)
	}

	// 收到信号要能干净退出：采集中途被杀会留下半个目录，
	// 而下一次运行会把它当成「已经采过」。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := store.New(r)
	c := fetch.NewClient(30 * time.Second)

	switch args[1] {
	case "douban":
		if err := fetch.Douban(ctx, c, st, *date, fetch.DoubanOpts{Tag: *tag, N: *n}); err != nil {
			fatal("%v", err)
		}
	default:
		fatal("未知数据源: %s（目前支持 douban）", args[1])
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "!! "+f+"\n", a...)
	os.Exit(1)
}
