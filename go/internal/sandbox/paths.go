// Package sandbox 生成分析层的隔离配置。
//
// 分析层是唯一接触外部不可信文本的环节——RSS 正文、仓库描述、热榜标题都是别人
// 能写的内容，每天无人值守地喂给模型。所以它读不到任何明文凭据，也出不了网。
package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// DenyDirs 受保护目录的唯一事实来源。seatbelt profile 与引擎权限文件都从这里生成。
//
// 拒绝的是「明文躺在磁盘上、没有第二道门禁」的凭据。
//
// 有意排除、不要加进来的：系统钥匙串 / Secret Service——它们加密且由系统按条目
// 仲裁，而且引擎的登录态存在里面，屏蔽会直接把认证掐断。
func DenyDirs(home string) []string {
	j := func(p ...string) string { return filepath.Join(append([]string{home}, p...)...) }
	return []string{
		// 云 / 基础设施凭据
		j(".oci"), j(".ssh"), j(".aws"), j(".gnupg"), j(".docker"), j(".kube"),
		j(".config", "gh"), j(".config", "gcloud"), j(".config", "rclone"),
		j(".cloudflared"), j(".terraform.d"),
		// 其他 AI/agent 工具的凭据与会话。
		// 这一类最容易漏：装一个新 CLI 就多一份明文密钥。
		// 会话记录尤其要拒——里面常有粘贴进去的密钥和内部信息。
		j(".claude"), j(".codex"), j(".gemini"), j(".cc-switch"),
		j(".pi"), j(".larkin"), j(".cursor"), j(".config", "zed"),
		// 浏览器 Cookie / 数据
		j("Library", "Cookies"), j("Library", "Application Support", "Google"),
		j("Library", "Application Support", "Firefox"), j("Library", "Messages"),
		j(".config", "google-chrome"), j(".config", "chromium"), j(".mozilla"),
		j(".local", "share", "keyrings"), j(".password-store"),
	}
}

// DenyFiles 单个文件。
//
// ⚠️ 按目录拒会漏掉同级的同名文件：~/.claude 拒了，~/.claude.json 还是敞开的
// ——它不在那个目录下。仓库内的凭据文件同理：分析层要能读仓库（data/ 在里面），
// 所以根目录整体放行，凭据文件必须逐个拒。
func DenyFiles(home, root string) []string {
	j := func(p ...string) string { return filepath.Join(append([]string{home}, p...)...) }
	return []string{
		j(".netrc"), j(".npmrc"), j(".pypirc"), j(".git-credentials"),
		j(".claude.json"),
		filepath.Join(root, "config.conf"),
		filepath.Join(root, "sandbox-paths.local"),
		filepath.Join(root, "ts", "wrangler.toml"),
		filepath.Join(root, "ts", ".dev.vars"),
	}
}

// ReadOnlyDirs 分析层不得改写自己的执行脚本、prompt 与 git 历史。
func ReadOnlyDirs(root string) []string {
	return []string{
		filepath.Join(root, "prompts"),
		filepath.Join(root, "go"),
		filepath.Join(root, "ts"),
		filepath.Join(root, ".git"),
	}
}

// LocalExtra 机器特有的敏感路径。denylist 天然会漂移——换机器、装新 CLI，
// 上面那份清单就又落后一次。这个文件不进版本控制，一行一个路径。
func LocalExtra(root string) []string {
	raw, err := os.ReadFile(filepath.Join(root, "sandbox-paths.local"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = l[:i]
		}
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// DenyForEngine 某个引擎实际要拒的目录。
//
// 每个引擎自己的认证路径必须从**它自己的**沙箱里排除，否则认证被掐断：
// codex 整个进程跑在外层沙箱里，读不到 ~/.codex 就登录不了。
// claude 不同——它的沙箱只约束模型执行的命令，自己读配置在沙箱外，
// 所以 ~/.claude 对它照拒不误。
func DenyForEngine(home, root, engine string) []string {
	skip := map[string]string{"codex": filepath.Join(home, ".codex")}[engine]
	var out []string
	for _, d := range append(DenyDirs(home), LocalExtra(root)...) {
		if skip != "" && d == skip {
			continue
		}
		out = append(out, d)
	}
	return out
}
