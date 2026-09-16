package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Wrapper 返回把命令包进沙箱的前缀 argv。
//
// macOS 用 seatbelt（deny file-read* → 访问返回 EPERM），
// Linux 用 bubblewrap（空 tmpfs / /dev/null 盖住敏感路径 → 看到的是空内容）。
// 两者语义略有差别（拒绝 vs 遮蔽），效果一致：凭据读不出来。
//
// **没有可用沙箱就返回错误，不降级。** 分析层要处理外部不可信文本，
// 无沙箱等于无防护。
func Wrapper(home, root, engine string) ([]string, error) {
	deny := DenyForEngine(home, root, engine)
	files := append(DenyFiles(home, root), LocalExtra(root)...)
	ro := ReadOnlyDirs(root)
	// 清单为空必须拒绝：下面会生成一份 (literal "") 什么都不拦的 profile，
	// 那比没有沙箱更糟，因为它看起来有。
	if len(deny) == 0 || len(ro) == 0 {
		return nil, fmt.Errorf("沙箱路径清单为空，拒绝运行")
	}

	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			return nil, fmt.Errorf("找不到 sandbox-exec（macOS 应自带）")
		}
		var b strings.Builder
		b.WriteString(";; 由 internal/sandbox 生成，勿手改\n(version 1)\n(allow default)\n")
		b.WriteString("(deny file-read*\n")
		for _, p := range deny {
			fmt.Fprintf(&b, "  (subpath %q)\n", p)
		}
		for _, p := range files {
			fmt.Fprintf(&b, "  (literal %q)\n", p)
		}
		b.WriteString("  )\n(deny file-write*\n")
		for _, p := range ro {
			fmt.Fprintf(&b, "  (subpath %q)\n", p)
		}
		b.WriteString("  )\n")
		prof := filepath.Join(root, "state", "sandbox.sb")
		if err := os.MkdirAll(filepath.Dir(prof), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(prof, []byte(b.String()), 0o644); err != nil {
			return nil, err
		}
		return []string{"sandbox-exec", "-f", prof}, nil

	case "linux":
		if _, err := exec.LookPath("bwrap"); err != nil {
			return nil, fmt.Errorf("找不到 bwrap（bubblewrap）。分析层要处理外部不可信" +
				"文本，没有沙箱等于无防护，拒绝运行。请先安装：apt install bubblewrap")
		}
		argv := []string{"bwrap", "--dev-bind", "/", "/", "--die-with-parent"}
		for _, p := range deny {
			if exists(p) {
				argv = append(argv, "--tmpfs", p)
			}
		}
		for _, p := range files {
			if exists(p) {
				argv = append(argv, "--ro-bind", "/dev/null", p)
			}
		}
		for _, p := range ro {
			if exists(p) {
				argv = append(argv, "--ro-bind", p, p)
			}
		}
		return append(argv, "--"), nil

	default:
		return nil, fmt.Errorf("不支持的平台 %s：没有可用沙箱，拒绝运行", runtime.GOOS)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
