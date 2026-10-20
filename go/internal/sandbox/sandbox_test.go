package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settings(t *testing.T) map[string]any {
	t.Helper()
	b, err := Settings("/home/u", "/repo", false)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func dangerSettings(t *testing.T) map[string]any {
	t.Helper()
	b, err := Settings("/home/u", "/repo", true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 这几个开关被改弱等于把防护关掉，而且不会有任何报错。
func TestFailClosedSwitches(t *testing.T) {
	sb := settings(t)["sandbox"].(map[string]any)
	for k, want := range map[string]bool{
		"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false,
	} {
		if sb[k] != want {
			t.Errorf("sandbox.%s = %v，必须是 %v", k, sb[k], want)
		}
	}
	net := sb["network"].(map[string]any)
	if d := net["allowedDomains"].([]any); len(d) != 0 {
		t.Errorf("分析层不该出网，allowedDomains = %v", d)
	}
	if net["strictAllowlist"] != true {
		t.Error("strictAllowlist 必须为 true")
	}
}

func TestCredentialPathsDenied(t *testing.T) {
	sb := settings(t)["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	joined := strings.Join(toStrings(sb["denyRead"]), " ")
	for _, p := range []string{".oci", ".ssh", ".aws", ".config/gh", ".gnupg", "Keychains"} {
		if !strings.Contains(joined, p) {
			t.Errorf("%s 未拒读", p)
		}
	}
	// ~/.claude.json 与 ~/.claude 同级、不在其下，按目录拒会漏掉它
	if !strings.Contains(joined, "/.claude.json") {
		t.Error("~/.claude.json 未被覆盖——按目录拒会漏掉同级文件")
	}
	// 仓库内的凭据同理：根目录整体放行，凭据文件必须逐个拒
	if !strings.Contains(joined, "/repo/config.conf") {
		t.Error("仓库内的 config.conf 未拒读")
	}
}

// denyRead（沙箱层）与 Read() 拒绝规则（工具层）必须一一对应，
// 只有一层挡住等于另一层可以绕过去。
//
// 按整条规则比，不按子串比：以前这里查「规则里含这个路径」，而规则写成了 Read(/abs)，
// 那是相对设置文件的路径、什么都匹配不上，测试照样是绿的。
func TestBothLayersCoverTheSamePaths(t *testing.T) {
	m := settings(t)
	rules := map[string]bool{}
	for _, r := range toStrings(m["permissions"].(map[string]any)["deny"]) {
		rules[r] = true
	}
	fs := m["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	for _, p := range toStrings(fs["denyRead"]) {
		if !rules["Read(/"+p+")"] && !rules["Read(/"+p+"/**)"] {
			t.Errorf("%s 只在沙箱层拒了，Read 工具仍可读", p)
		}
	}
}

// 权限规则里 `/x` 是相对设置文件的，绝对路径必须写 `//x`。写错不报错，规则只是落空——
// 2026-10-20 之前的 Read 拒绝就是这样全部落空的（实测 Read(/abs) 照读、Read(//abs) 被拒）。
func TestRulePathsAreAbsolute(t *testing.T) {
	perms := settings(t)["permissions"].(map[string]any)
	all := append(toStrings(perms["deny"]), toStrings(perms["allow"])...)
	n := 0
	for _, r := range all {
		for _, tool := range []string{"Read(", "Edit("} {
			if strings.HasPrefix(r, tool) {
				n++
				if !strings.HasPrefix(r, tool+"//") {
					t.Errorf("%s：路径要写成 //绝对路径，单斜杠是相对设置文件的", r)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("一条 Read/Edit 规则都没有")
	}
}

// 不许改写的，两层都要拒：沙箱的 denyWrite 只管模型跑的命令，Write/Edit 工具是 CLI
// 自己执行的、只认 Edit(...) 规则（实测：denyWrite 里的 go/、prompts/，Write 照样写进）。
func TestCannotRewriteOwnCode(t *testing.T) {
	m := settings(t)
	fs := m["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	dw := map[string]bool{}
	for _, p := range toStrings(fs["denyWrite"]) {
		dw[p] = true
	}
	rules := map[string]bool{}
	for _, r := range toStrings(m["permissions"].(map[string]any)["deny"]) {
		rules[r] = true
	}
	for _, p := range []string{"prompts", "go", "ts", ".git", "config.conf", "CLAUDE.local.md", ".claude"} {
		abs := filepath.Join("/repo", p)
		if !dw[abs] {
			t.Errorf("%s 不在沙箱的 denyWrite 里，Bash 能改写", p)
		}
		if !rules["Edit(/"+abs+"/**)"] || !rules["Edit(/"+abs+")"] {
			t.Errorf("%s 没有 Edit 拒绝规则，Write/Edit 工具能改写", p)
		}
	}
}

// 工具层是白名单：只有 reports/ 与临时目录免审批，别处的编辑要审批、无人值守当场拒。
// acceptEdits 会对整个工作目录（仓库根）放行，config.conf、state/、bin/ 都在里面。
func TestOnlyReportsAndScratchWritable(t *testing.T) {
	m := settings(t)
	perms := m["permissions"].(map[string]any)
	if perms["defaultMode"] != "default" {
		t.Errorf("defaultMode = %v：acceptEdits 会放行仓库里的一切编辑", perms["defaultMode"])
	}
	want := []string{"Edit(//repo/reports/**)", "Edit(//repo/tmp/**)"}
	if got := toStrings(perms["allow"]); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("allow = %v，只该放行 %v", got, want)
	}
	fs := m["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if got := toStrings(fs["allowWrite"]); strings.Join(got, " ") != strings.Join(WritableDirs("/repo"), " ") {
		t.Errorf("allowWrite = %v", got)
	}
}

// claude 的沙箱默认放行工作目录的写入、收不回来，只能把仓库根下现有的每一项逐个拒掉。
func TestProtectedPathsCoverRepoRoot(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"state", "bin", "data", "site", "logs", "reports", "tmp", "scripts"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	for _, f := range []string{"AGENTS.md", "Makefile"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644)
	}
	got, err := ProtectedPaths("/home/u", root)
	if err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, p := range got {
		has[p] = true
	}
	for _, p := range []string{"state", "bin", "data", "site", "logs", "scripts", "AGENTS.md", "Makefile",
		"prompts", "go", "ts", ".git", // ReadOnlyDirs，哪怕还不存在
		"config.conf", "sandbox-paths.local", // 仓库内的凭据文件，还没建出来也要拒
		".claude", "CLAUDE.local.md", ".mcp.json"} { // 建出来就会被交互式会话读进去
		if !has[filepath.Join(root, p)] {
			t.Errorf("%s 不在拒写清单里", p)
		}
	}
	for _, p := range WritableDirs(root) {
		if has[p] {
			t.Errorf("%s 是分析层要写的地方，不该拒", p)
		}
	}
}

// codex 整个进程跑在外层沙箱里，拒掉 ~/.codex 就登录不了。
// claude 不同：它的沙箱只约束模型执行的命令，自己读配置在沙箱外。
func TestEngineSelfAuthExemption(t *testing.T) {
	home := "/home/u"
	codex := strings.Join(DenyForEngine(home, "/repo", "codex"), " ")
	if strings.Contains(codex, filepath.Join(home, ".codex")) {
		t.Error("codex 的沙箱不该拒掉它自己的认证目录")
	}
	cl := strings.Join(DenyForEngine(home, "/repo", "claude"), " ")
	if !strings.Contains(cl, filepath.Join(home, ".codex")) {
		t.Error("claude 那层应照拒 ~/.codex")
	}
	if !strings.Contains(cl, filepath.Join(home, ".claude")) {
		t.Error("claude 那层应拒 ~/.claude（会话 transcript 里常有粘贴的密钥）")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// 危险模式必须真的什么都不拦。
//
// 「开关打开了但没生效」是最坏的一种：以为在无限制地跑（结果还在被拦），或者
// 反过来以为防护还在（其实全关了）。所以这里正向断言每一层都松开了。
func TestDangerDropsEveryGuard(t *testing.T) {
	m := dangerSettings(t)
	sb, ok := m["sandbox"].(map[string]any)
	if !ok {
		t.Fatal("危险模式的权限文件里没有 sandbox 段")
	}
	for k, want := range map[string]bool{
		"enabled": false, "failIfUnavailable": false, "allowUnsandboxedCommands": true,
	} {
		if sb[k] != want {
			t.Errorf("危险模式的 sandbox.%s = %v，必须是 %v", k, sb[k], want)
		}
	}
	perms := m["permissions"].(map[string]any)
	if d, exists := perms["deny"]; exists {
		t.Errorf("危险模式不该有任何 deny 规则，却拿到 %v", d)
	}
	if perms["blockReadsOutsideWorkingDirectories"] != false {
		t.Error("危险模式必须允许读工作目录之外")
	}
	// 留着这个的话，bypassPermissions 会被命令行拒绝，症状是「以为绕过了其实没有」
	if _, exists := perms["disableBypassPermissionsMode"]; exists {
		t.Error("危险模式必须去掉 disableBypassPermissionsMode")
	}
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("defaultMode = %v", perms["defaultMode"])
	}
}

// 两份权限文件不能同名：同名的话切模式不会留下任何痕迹，
// 而且「上一档是不是无限制跑的」事后查不出来。
func TestWriteSettingsKeepsDangerSeparate(t *testing.T) {
	root := t.TempDir()
	safe, err := WriteSettings("/home/u", root, false)
	if err != nil {
		t.Fatal(err)
	}
	danger, err := WriteSettings("/home/u", root, true)
	if err != nil {
		t.Fatal(err)
	}
	if safe == danger {
		t.Fatalf("两份权限文件同名：%s", safe)
	}
	for _, p := range []string{safe, danger} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s 没写出来：%v", p, err)
		}
	}
}
