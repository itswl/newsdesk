// newsdesk —— 每日简报流水线。
//
// 四个任务各自定时触发：采集层联网抓数据落盘，分析层（模型，跑在沙箱里）
// 只读本地文件写报告，再合成静态站发布。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

const usage = `newsdesk —— 每日简报流水线

用法：
  newsdesk <命令> [参数]

日常：
  schedule            启动调度器（常驻，睡眠补跑）
  run <任务>           立刻跑一档完整流程（采集→模型→建站→发布）
  status              看今天四档的状态

分步（排查时用）：
  fetch <任务>         只采集，不调模型
  site                只重建静态站
  translate [日期]     把某天的报告翻成英文（默认今天）
  publish             只发布已生成的站点
  leakcheck           扫产出里有没有混进凭据
  prune               清理过期数据

自检：
  doctor              检查配置、时间表、沙箱、依赖
  langs               看 SITE_LANGS 实际生效哪些语言
  sandbox             打印将要施加的沙箱边界
  version

任务：ai | douban | trending | momoyu
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	// 信号即退出信号：Ctrl-C 与 launchd/systemd 的停止都走这里，
	// ctx 一路传到引擎子进程，不留孤儿。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "schedule":
		err = cmdSchedule(ctx, args)
	case "run":
		err = cmdRun(ctx, args)
	case "fetch":
		err = cmdFetch(ctx, args)
	case "site":
		err = cmdSite(ctx, args)
	case "translate":
		err = cmdTranslate(ctx, args)
	case "publish":
		err = cmdPublish(ctx, args)
	case "leakcheck":
		err = cmdLeakcheck(ctx, args)
	case "prune":
		err = cmdPrune(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "doctor":
		err = cmdDoctor(ctx, args)
	case "langs":
		err = cmdLangs(ctx, args)
	case "sandbox":
		err = cmdSandbox(ctx, args)
	case "version", "--version", "-v":
		fmt.Println("newsdesk " + version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}
