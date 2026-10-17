package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/langs"
	"github.com/itswl/newsdesk/internal/notify"
	"github.com/itswl/newsdesk/internal/oci"
	"github.com/itswl/newsdesk/internal/sandbox"
	"github.com/itswl/newsdesk/internal/task"
)

type doc struct{ fail, warn int }

func (d *doc) ok(f string, a ...any)   { fmt.Printf("  ✓ %s\n", fmt.Sprintf(f, a...)) }
func (d *doc) note(f string, a ...any) { fmt.Printf("    %s\n", fmt.Sprintf(f, a...)) }
func (d *doc) bad(f string, a ...any)  { d.fail++; fmt.Printf("  ✗ %s\n", fmt.Sprintf(f, a...)) }
func (d *doc) soft(f string, a ...any) { d.warn++; fmt.Printf("  ! %s\n", fmt.Sprintf(f, a...)) }

func cmdDoctor(ctx context.Context, args []string) error {
	d := &doc{}
	root, err := findRoot()
	if err != nil {
		return err
	}
	fmt.Println("根目录")
	d.ok("%s", root)

	fmt.Println("\n配置")
	cfg, err := loadConfig(root)
	if err != nil {
		d.bad("%v", err)
		cfg, _ = config.Load("")
	} else if _, err := os.Stat(cfg.Path); err != nil {
		d.soft("没有 %s，全部走默认值（只在本机看简报是够的）", cfg.Path)
	} else {
		d.ok("%s 解析通过，%d 项", cfg.Path, len(cfg.All()))
	}

	fmt.Println("\n时间表")
	if problems := task.Validate(); len(problems) > 0 {
		for _, p := range problems {
			d.bad("%s", p)
		}
	} else {
		var ss []string
		for _, t := range task.All {
			ss = append(ss, t.At+" "+t.Key)
		}
		d.ok("%s", strings.Join(ss, " | "))
		d.note("相邻间隔 %v 小时，用量窗口 %.0fh", roundAll(task.Gaps()), task.UsageWindowHours)
	}

	fmt.Println("\n引擎")
	primary := cfg.Get("ENGINE", "claude")
	fallback := cfg.Get("ENGINE_FALLBACK", "")
	checkEngine(d, primary, cfg, true)
	if fallback == "" {
		d.soft("没配兜底引擎：主引擎配额耗尽时那一档直接跳过")
	} else {
		checkEngine(d, fallback, cfg, false)
	}

	fmt.Println("\n沙箱")
	checkSandbox(ctx, d, root, cfg)

	fmt.Println("\n语言")
	fmt.Print(indent(langs.Explain(cfg, root)))

	fmt.Println("\n告警")
	if cfg.Get("ALERT_WEBHOOK", "") == "" {
		// 不配也能跑，但失败会完全沉默——最安全的失败方式同时也是最沉默的
		d.soft("没配 ALERT_WEBHOOK：任务失败、错过补跑、日终缺口都不会有人知道")
	} else {
		n := notify.New(cfg.Get("ALERT_WEBHOOK", ""), cfg.Get("ALERT_PAYLOAD", ""))
		if len(args) > 0 && args[0] == "--send-test" {
			if err := n.Send("newsdesk 自检", "这是一条测试告警，收到说明通道正常。"); err != nil {
				d.bad("告警发送失败：%v", err)
			} else {
				d.ok("测试告警已发出")
			}
		} else {
			d.ok("已配置（加 --send-test 真发一条）")
		}
	}

	fmt.Println("\n发布")
	backup, public := cfg.Get("BACKUP_BUCKET", ""), cfg.Get("PUBLISH_BUCKET", "")
	if backup == "" && public == "" {
		d.soft("两个桶都没配：只在本机看，site/ 直接用浏览器打开")
	} else {
		prof, err := oci.LoadProfile(cfg.Get("OCI_CONFIG", ""), cfg.Get("OCI_PROFILE", ""))
		if err != nil {
			d.bad("%v", err)
		} else {
			c := oci.New(prof)
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			ns, err := c.Namespace(cctx)
			cancel()
			if err != nil {
				d.bad("连不上对象存储：%v", err)
			} else {
				d.ok("namespace %s / region %s", ns, prof.Region)
				for label, b := range map[string]string{"私有备份桶": backup, "公开站点桶": public} {
					if b == "" {
						continue
					}
					cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
					objs, err := c.List(cctx, b, "")
					cancel()
					if err != nil {
						d.bad("%s %s 不可用：%v", label, b, err)
					} else {
						d.ok("%s %s 可访问，%d 个对象", label, b, len(objs))
					}
				}
			}
		}
		if public != "" && cfg.Get("SITE_URL", "") == "" {
			d.soft("配了公开桶但没配 SITE_URL：不会生成 Atom feed")
		}
	}

	fmt.Println()
	switch {
	case d.fail > 0:
		return fmt.Errorf("%d 项不通过、%d 项提醒", d.fail, d.warn)
	case d.warn > 0:
		fmt.Printf("全部关键项通过，%d 项提醒。\n", d.warn)
	default:
		fmt.Println("全部通过。")
	}
	return nil
}

func checkEngine(d *doc, kind string, cfg *config.Config, isPrimary bool) {
	role := "兜底"
	if isPrimary {
		role = "主"
	}
	switch kind {
	case "claude", "codex":
		p, err := exec.LookPath(kind)
		if err != nil {
			d.bad("%s引擎 %s：命令不在 PATH 里", role, kind)
			return
		}
		model := cfg.Get(map[string]string{"claude": "CLAUDE_MODEL", "codex": "CODEX_MODEL"}[kind], "")
		d.ok("%s引擎 %s（%s）模型 %s", role, kind, p, model)
	case "api":
		if err := cfg.Require("FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"); err != nil {
			d.bad("%s引擎 api：%v", role, err)
			return
		}
		if _, err := exec.LookPath("claude"); err != nil {
			d.bad("%s引擎 api 仍要用 claude CLI，但它不在 PATH 里", role)
			return
		}
		d.ok("%s引擎 api → %s（%s）", role, cfg.Get("FALLBACK_BASE_URL", ""), cfg.Get("FALLBACK_MODEL", ""))
	default:
		d.bad("%s引擎 %q 无法识别（可选 claude | codex | api）", role, kind)
	}
}

// checkSandbox 逐条实测，不看配置写没写。
//
// 「能执行命令」这一项是为套娃失效模式设的：两层 seatbelt 嵌套会让 Bash 整个
// 失效（sandbox_apply: Operation not permitted，连 echo 都跑不了），而模型会靠
// Read/Write 工具绕过去把任务做完，日志上看不出异常。
func checkSandbox(ctx context.Context, d *doc, root string, cfg *config.Config) {
	h := home()
	danger := cfg.Danger()
	if danger {
		d.bad("%s", config.DangerLine)
		b, err := sandbox.Settings(h, root, true)
		if err != nil {
			d.bad("危险模式的权限文件渲染失败：%v", err)
			return
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			d.bad("危险模式的权限文件不是合法 JSON：%v", err)
			return
		}
		sb, _ := m["sandbox"].(map[string]any)
		perms, _ := m["permissions"].(map[string]any)
		if sb["enabled"] != false || sb["failIfUnavailable"] != false {
			d.bad("危险模式下沙箱竟然还是开的（enabled=%v failIfUnavailable=%v）", sb["enabled"], sb["failIfUnavailable"])
		} else if _, ok := perms["deny"]; ok {
			d.bad("危险模式下权限文件里竟然还有 deny 规则")
		} else {
			d.ok("危险模式生效：沙箱关闭、无 deny 规则（这是有意的，DANGER 一去掉就恢复）")
		}
		d.note("要恢复防护：把 config.conf 里的 DANGER 去掉或改成 0，下一档起生效，不用重启")
		return
	}
	deny := append(sandbox.DenyDirs(h), sandbox.DenyFiles(h, root)...)
	if len(deny) == 0 {
		// 清单为空的 profile 什么都不拦，比没有沙箱更糟——它看起来有
		d.bad("拒绝清单是空的，生成出来的 profile 什么都不拦")
		return
	}
	d.ok("拒绝清单 %d 条", len(deny))

	if _, err := sandbox.Settings(h, root, false); err != nil {
		d.bad("claude 权限文件渲染失败：%v", err)
	} else {
		d.ok("claude 权限文件可渲染（fail-closed：沙箱起不来则整档不跑）")
	}

	wrap, err := sandbox.Wrapper(h, root, "codex")
	if err != nil {
		d.soft("codex 外层沙箱不可用：%v", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	argv := append(append([]string{}, wrap...), "/bin/sh", "-c", "echo SANDBOX_OK")
	out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SANDBOX_OK") {
		d.bad("沙箱里连 echo 都跑不了（多半是套了两层）：%s %v", strings.TrimSpace(string(out)), err)
		return
	}
	d.ok("沙箱内能执行命令")

	// 探的是 codex 那层沙箱，用的必须是**它自己的**清单。
	//
	// ~/.codex 是 codex 的认证，从它的沙箱里排掉是有意的——整个进程跑在外层
	// 沙箱里，读不到就登录不了。拿全量清单去探会把这条报成漏洞，然后有人
	// 「修」掉它，引擎第二天起不来。claude 不同：它的沙箱只约束模型执行的命令，
	// 自己读配置在沙箱外，所以 ~/.claude 对它照拒不误。
	engineDeny := append(sandbox.DenyForEngine(h, root, "codex"), sandbox.DenyFiles(h, root)...)
	var exempt []string
	for _, p := range deny {
		if !contains(engineDeny, p) {
			exempt = append(exempt, strings.Replace(p, h, "~", 1))
		}
	}
	if len(exempt) > 0 {
		d.ok("codex 沙箱按设计放行 %s（它自己的认证，拒掉就登录不了）",
			strings.Join(exempt, " "))
	}

	// 布尔探针：只看权限位，不读任何内容
	var leaked []string
	for _, p := range engineDeny {
		p = strings.TrimSuffix(p, "/")
		if _, err := os.Stat(p); err != nil {
			continue // 本机没有这个路径，不算漏
		}
		argv := append(append([]string{}, wrap...), "/bin/sh", "-c",
			fmt.Sprintf("test -r %q && echo READABLE || echo BLOCKED", p))
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out, _ := exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
		cancel()
		if strings.Contains(string(out), "READABLE") {
			leaked = append(leaked, strings.Replace(p, h, "~", 1))
		}
	}
	if len(leaked) > 0 {
		d.bad("这些本该拒掉的路径在沙箱里仍可读：\n      %s", strings.Join(leaked, "\n      "))
	} else {
		d.ok("拒绝清单逐条实测通过（布尔探针，未读取任何内容）")
	}

	// 钥匙串必须仍可读，否则引擎登录态被掐断
	kc := filepath.Join(h, "Library", "Keychains")
	if _, err := os.Stat(kc); err == nil {
		argv := append(append([]string{}, wrap...), "/bin/sh", "-c",
			fmt.Sprintf("test -r %q && echo READABLE || echo BLOCKED", kc))
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out, _ := exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
		cancel()
		if strings.Contains(string(out), "BLOCKED") {
			d.bad("钥匙串被拒了——引擎会直接失去登录态（claude 报 Not logged in）。" +
				"它是加密的、由系统按条目仲裁，和明文凭据文件不是一回事")
		} else {
			d.ok("钥匙串仍可读（有意为之，引擎登录态在里面）")
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func roundAll(fs []float64) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = fmt.Sprintf("%.2f", f)
	}
	return out
}
