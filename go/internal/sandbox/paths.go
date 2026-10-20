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

// ScratchBase 分析层临时目录在仓库根下的名字，每档一个子目录（见 pipeline.ScratchDir）。
const ScratchBase = "tmp"

// WritableDirs 分析层该写的只有这两处：报告与临时目录。
func WritableDirs(root string) []string {
	return []string{filepath.Join(root, "reports"), filepath.Join(root, ScratchBase)}
}

// autoLoaded 仓库根下现在可能没有、但一旦被建出来就会被别的程序自动读进去的名字。
//
// ProtectedPaths 按仓库根现有的条目拒写，模型新建的根级文件挡不住；这几个挡不住的代价
// 最大：CLAUDE.local.md、.claude/ 下的设置（含 hooks）与 .mcp.json 会被交互式会话在启动
// 时读进去，而交互式会话的权限往往放得很宽；AGENTS.override.md 与 .codex/ 是 codex 的；
// go 命令会顺着目录往上找 go.work；direnv 一进目录就执行 .envrc。
var autoLoaded = []string{
	".claude", "CLAUDE.local.md", ".mcp.json", "AGENTS.override.md", ".codex", "go.work", ".envrc",
}

// ProtectedPaths 仓库里分析层不得改写的一切：ReadOnlyDirs、仓库根下除 WritableDirs 以外
// 现有的每一项、仓库内的凭据文件（还没建出来的也算）、以及 autoLoaded。
//
// 为什么要列这么全：claude 的沙箱默认放行工作目录（仓库根）的写入，allowWrite 只能往上
// 加、收不回来，能收的只有 denyWrite；而 CLI 生成 seatbelt 时 deny 排在 allow 后面，拒了
// 仓库根就连 reports/ 一起拒了，所以只能逐项拒。以前只拒 ReadOnlyDirs，config.conf、
// state/、bin/、AGENTS.md 都写得进：config.conf 是热重读的，往里写一行 DANGER=1，下一档
// 就脱离沙箱、可联网、凭据全开地跑；state/ 里是它自己的权限文件；bin/ 里是下次重启要
// 执行的二进制。现列仓库根，以后新加的目录和文件自动在内。
//
// 列不出仓库根就报错（fail-closed）；仓库根不存在除外，那时没有东西要护。
func ProtectedPaths(home, root string) ([]string, error) {
	skip := map[string]bool{}
	for _, p := range WritableDirs(root) {
		skip[p] = true
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !skip[p] && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range ReadOnlyDirs(root) {
		add(p)
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		add(filepath.Join(root, e.Name()))
	}
	for _, p := range DenyFiles(home, root) {
		if strings.HasPrefix(p, root+string(filepath.Separator)) {
			add(p)
		}
	}
	for _, n := range autoLoaded {
		add(filepath.Join(root, n))
	}
	return out, nil
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
