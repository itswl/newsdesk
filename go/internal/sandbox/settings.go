package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings 是传给 claude 的权限文件。
//
// 拒读路径从 DenyForEngine/DenyFiles 生成，和 seatbelt profile 同源——
// 两份手写清单会各自漂移：装了新的带凭据的 CLI 只改一头，另一头默默落后。
//
// danger 为 true 时走 DangerSettings：不设任何限制，见那里的说明。
func Settings(home, root string, danger bool) ([]byte, error) {
	if danger {
		return DangerSettings()
	}
	var denyRead []string
	denyRead = append(denyRead, DenyForEngine(home, root, "claude")...)
	denyRead = append(denyRead, DenyFiles(home, root)...)
	// 钥匙串只在这一层拒：它由 claude 自己在沙箱外读，模型执行的命令不该碰。
	// 放进 seatbelt 会把 codex 的登录态一起掐断。
	denyRead = append(denyRead, filepath.Join(home, "Library", "Keychains"))

	readRules := make([]string, 0, len(denyRead))
	for _, p := range denyRead {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			readRules = append(readRules, "Read("+p+"/**)")
		} else {
			readRules = append(readRules, "Read("+p+")")
		}
	}
	deny := append(readRules,
		"Bash(git push:*)", "Bash(gh repo:*)", "Bash(gh auth:*)", "Bash(gh secret:*)",
		"Bash(oci:*)", "Bash(ssh:*)", "Bash(scp:*)", "Bash(security:*)",
		"Bash(sudo:*)", "Bash(launchctl:*)", "Bash(crontab:*)")

	cfg := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"_generated": "由 internal/sandbox 生成，勿手改；" +
			"拒读路径与 seatbelt profile 同源，见 internal/sandbox/paths.go",
		"permissions": map[string]any{
			"defaultMode":                         "acceptEdits",
			"blockReadsOutsideWorkingDirectories": true,
			"disableBypassPermissionsMode":        "disable",
			"deny":                                deny,
		},
		"sandbox": map[string]any{
			"enabled": true,
			// 沙箱起不来必须整个拒绝运行，不静默降级成无防护
			"failIfUnavailable": true,
			// 为 true 时 dangerouslyDisableSandbox 参数可以绕过防护
			"allowUnsandboxedCommands": false,
			"autoAllowBashIfSandboxed": true,
			"network": map[string]any{
				// 所有联网都在采集层。分析层一旦能出网，拒读凭据就没意义了
				"allowedDomains":    []string{},
				"strictAllowlist":   true,
				"allowLocalBinding": false,
			},
			"filesystem": map[string]any{
				"denyRead":   denyRead,
				"allowWrite": []string{root},
				"denyWrite":  ReadOnlyDirs(root),
			},
		},
		"disableAllHooks":        true,
		"autoMemoryEnabled":      false,
		"includeGitInstructions": false,
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// DangerSettings 是危险模式（DANGER=1）的权限文件：**不设任何限制**。
//
// 与默认那份的差别是整套防护一起拿掉：
//   - 没有沙箱：enabled=false，而且不再 failIfUnavailable。沙箱起不来也照跑。
//   - permissions 里一条 deny 都没有：凭据目录（~/.ssh、~/.oci…）、仓库内的凭据
//     文件（config.conf、ts/wrangler.toml…）、以及 git push / ssh / sudo 这类命令
//     全部放开。
//   - blockReadsOutsideWorkingDirectories 关掉，工作目录之外也能读。
//
// ⚠️ 这一层放开的正是「模型读到的文本是别人写的」时的唯一屏障：分析层读 HN 标题、
// 仓库描述、豆瓣简介，而它的产出当天就发到公开站点上。开着它跑，等于把「不受控的
// 输入 + 凭据可读 + 可以联网」三件事同时成立。留痕在调用方：调度器启动、每档开跑、
// status、status.json 都会带上标记，跑出来的那一档也会在 schedule.json 里记一笔。
//
// 文件名与正常那份分开（state/agent-settings-danger.json），好让「这一档是在什么
// 模式下跑的」在磁盘上看得出来。
func DangerSettings() ([]byte, error) {
	cfg := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"_generated": "由 internal/sandbox 生成（DANGER=1）：不经沙箱、可联网、" +
			"凭据拒读与命令拒绝全部关闭。默认那份见 state/agent-settings.json",
		"permissions": map[string]any{
			// 绕过一切权限检查，且不再禁掉 bypass 模式（默认那份里是 disable）
			"defaultMode":                         "bypassPermissions",
			"blockReadsOutsideWorkingDirectories": false,
		},
		"sandbox": map[string]any{
			"enabled": false,
			// 与默认那三条相反：沙箱起不来不拦、允许绕过沙箱
			"failIfUnavailable":        false,
			"allowUnsandboxedCommands": true,
			"autoAllowBashIfSandboxed": true,
		},
		// 与默认那份保持一致：这三项是会话卫生，不是这次要放开的能力
		"disableAllHooks":        true,
		"autoMemoryEnabled":      false,
		"includeGitInstructions": false,
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// WriteSettings 渲染到 state/ 下并返回路径。
//
// 危险模式落到另一个文件名，正常的那些消费方（doctor 的自检、sandbox 子命令）
// 一眼就能看出当前是哪一份。
func WriteSettings(home, root string, danger bool) (string, error) {
	b, err := Settings(home, root, danger)
	if err != nil {
		return "", err
	}
	name := "agent-settings.json"
	if danger {
		name = "agent-settings-danger.json"
	}
	p := filepath.Join(root, "state", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, b, 0o600)
}
