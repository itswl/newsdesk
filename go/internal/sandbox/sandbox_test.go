package sandbox

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func settings(t *testing.T) map[string]any {
	t.Helper()
	b, err := Settings("/home/u", "/repo")
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
