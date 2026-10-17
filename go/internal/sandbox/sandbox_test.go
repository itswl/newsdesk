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
func TestBothLayersCoverTheSamePaths(t *testing.T) {
	m := settings(t)
	rules := strings.Join(toStrings(m["permissions"].(map[string]any)["deny"]), " ")
	fs := m["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	for _, p := range toStrings(fs["denyRead"]) {
		if !strings.Contains(rules, p) {
			t.Errorf("%s 只在沙箱层拒了，Read 工具仍可读", p)
		}
	}
}

func TestCannotRewriteOwnCode(t *testing.T) {
	fs := settings(t)["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	dw := strings.Join(toStrings(fs["denyWrite"]), " ")
	for _, p := range []string{"prompts", "go", ".git"} {
		if !strings.Contains(dw, p) {
			t.Errorf("%s 可被改写", p)
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
