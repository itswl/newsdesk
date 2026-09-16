// Package publish 把产出送到对象存储。
//
// 两个桶职责不同，别混：
//
//	私有桶  data/ + reports/ + site/ 全量备份，是唯一的灾备副本，不设窗口
//	公开桶  只放 site/，且只放最近 PUBLIC_DAYS 天
//
// ⚠️ 公开桶里有豆瓣简介、各平台热榜标题等第三方文本。改动发布范围前先想清楚。
package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/itswl/newsdesk2/internal/config"
	"github.com/itswl/newsdesk2/internal/oci"
)

type Options struct {
	Root      string
	Cfg       *config.Config
	PublicDir string // 窗口截断后的站点目录（发公开桶的就是它）
	DryRun    bool
	Log       func(format string, a ...any)
}

type Result struct {
	Backup, Public oci.SyncStat
	Skipped        []string
}

func Run(ctx context.Context, o Options) (Result, error) {
	var res Result
	logf := o.Log
	if logf == nil {
		logf = func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	}
	backup := o.Cfg.Get("BACKUP_BUCKET", "")
	public := o.Cfg.Get("PUBLISH_BUCKET", "")
	if backup == "" && public == "" {
		res.Skipped = append(res.Skipped, "两个桶都没配，跳过发布（站点仍在本地 site/ 下）")
		logf("发布：未配置桶，跳过")
		return res, nil
	}

	prof, err := oci.LoadProfile(o.Cfg.Get("OCI_CONFIG", ""), o.Cfg.Get("OCI_PROFILE", ""))
	if err != nil {
		return res, err
	}
	if ns := o.Cfg.Get("BACKUP_NAMESPACE", ""); ns != "" {
		prof.Namespace = ns
	}
	c := oci.New(prof)
	ns, err := c.Namespace(ctx)
	if err != nil {
		return res, fmt.Errorf("取 namespace: %w", err)
	}
	logf("发布：namespace=%s region=%s", ns, prof.Region)

	if backup != "" {
		for _, sub := range []string{"data", "reports", "site"} {
			dir := filepath.Join(o.Root, sub)
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			if o.DryRun {
				logf("  [dry-run] %s/ → %s:%s/", sub, backup, sub)
				continue
			}
			st, err := c.Sync(ctx, dir, oci.SyncOpts{
				Bucket: backup, Prefix: sub + "/", Delete: false, Log: logf,
			})
			if err != nil {
				return res, fmt.Errorf("备份 %s: %w", sub, err)
			}
			merge(&res.Backup, st)
			logf("  备份 %s/ → %s: 上传 %d、跳过 %d、失败 %d",
				sub, backup, st.Uploaded, st.Skipped, st.Failed)
		}
	}

	if public != "" {
		src := o.PublicDir
		if src == "" {
			src = filepath.Join(o.Root, "site")
		}
		if _, err := os.Stat(src); err != nil {
			return res, fmt.Errorf("公开站点目录不存在: %s", src)
		}
		if o.DryRun {
			logf("  [dry-run] %s → %s（含删除超窗对象）", src, public)
		} else {
			// Delete=true：窗口外的旧页面必须真删掉。只是不再放链接是不够的，
			// 桶列不出清单但地址是能猜的（日期 + 任务名就是路径）。
			st, err := c.Sync(ctx, src, oci.SyncOpts{
				Bucket: public, Prefix: "", Delete: true, Log: logf,
			})
			if err != nil {
				return res, fmt.Errorf("发布站点: %w", err)
			}
			res.Public = st
			logf("  发布 site → %s: 上传 %d、跳过 %d、删除 %d、失败 %d",
				public, st.Uploaded, st.Skipped, st.Deleted, st.Failed)
		}
	}

	if n := res.Backup.Failed + res.Public.Failed; n > 0 {
		return res, fmt.Errorf("有 %d 个对象没传成功：\n  %s", n,
			strings.Join(append(res.Backup.Errs, res.Public.Errs...), "\n  "))
	}
	return res, nil
}

func merge(dst *oci.SyncStat, s oci.SyncStat) {
	dst.Uploaded += s.Uploaded
	dst.Skipped += s.Skipped
	dst.Deleted += s.Deleted
	dst.Failed += s.Failed
	dst.Bytes += s.Bytes
	dst.Errs = append(dst.Errs, s.Errs...)
}
