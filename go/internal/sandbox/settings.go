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
func Settings(home, root string) ([]byte, error) {
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

// WriteSettings 渲染到 state/agent-settings.json 并返回路径。
func WriteSettings(home, root string) (string, error) {
	b, err := Settings(home, root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "state", "agent-settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, b, 0o600)
}
