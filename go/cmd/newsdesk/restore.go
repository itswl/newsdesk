package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/itswl/newsdesk2/internal/oci"
)

// cmdRestore 从私有桶拉回历史产出。
//
// state/ 也要拉：不带上的话换机器后当天任务会全部重跑一遍，白烧一整个用量窗口。
// 但按本机路径渲染的沙箱 profile 不在备份范围内——换机器必须重新生成，
// 带过去反而是错的。
func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "只列出会拉回什么")
	_ = fs.Parse(args)

	root, err := findRoot()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	bucket := cfg.Get("BACKUP_BUCKET", "")
	if bucket == "" {
		return fmt.Errorf("没配 BACKUP_BUCKET，没有可恢复的备份")
	}
	prof, err := oci.LoadProfile(cfg.Get("OCI_CONFIG", ""), cfg.Get("OCI_PROFILE", ""))
	if err != nil {
		return err
	}
	if ns := cfg.Get("BACKUP_NAMESPACE", ""); ns != "" {
		prof.Namespace = ns
	}
	c := oci.New(prof)

	subs := fs.Args()
	if len(subs) == 0 {
		subs = []string{"reports", "data", "site", "state"}
	}
	for _, sub := range subs {
		if *dry {
			objs, err := c.List(ctx, bucket, sub+"/")
			if err != nil {
				return err
			}
			fmt.Printf("  [dry-run] %s/ ← %s：%d 个对象\n", sub, bucket, len(objs))
			continue
		}
		dst := filepath.Join(root, sub)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		st, err := c.Pull(ctx, bucket, sub+"/", dst)
		if err != nil {
			return fmt.Errorf("恢复 %s: %w", sub, err)
		}
		fmt.Printf("  %s/ ← %s：拉回 %d、跳过 %d、失败 %d\n",
			sub, bucket, st.Files, st.Skipped, st.Failed)
		if len(st.Errs) > 0 {
			fmt.Println("    " + strings.Join(st.Errs, "\n    "))
		}
	}
	fmt.Println("恢复完成。沙箱 profile 按本机路径现渲染，不在备份范围内——跑 newsdesk doctor 确认。")
	return nil
}
