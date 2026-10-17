package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, body string) *Config {
	t.Helper()
	c, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestBasics(t *testing.T) {
	c := load(t, "ENGINE=claude\nPUBLIC_DAYS=7\nSITE_LANGS=zh-CN,zh-TW,en\n")
	if got := c.Get("ENGINE", "x"); got != "claude" {
		t.Errorf("Get = %q", got)
	}
	if got := c.Int("PUBLIC_DAYS", 0); got != 7 {
		t.Errorf("Int = %d", got)
	}
	if got := c.List("SITE_LANGS"); len(got) != 3 || got[0] != "zh-CN" || got[2] != "en" {
		t.Errorf("List = %v", got)
	}
}

func TestCommentsAndQuotes(t *testing.T) {
	c := load(t, `# 整行注释
ENGINE=claude          # 行尾注释
QUOTED="a b"
SINGLE='c'
EMPTY=
`)
	for k, want := range map[string]string{"ENGINE": "claude", "QUOTED": "a b", "SINGLE": "c"} {
		if got := c.Get(k, ""); got != want {
			t.Errorf("%s = %q, 想要 %q", k, got, want)
		}
	}
	// 空值等同于缺项：配置模板里大量留空表示「关掉这个功能」
	if got := c.Get("EMPTY", "fallback"); got != "fallback" {
		t.Errorf("空值应回默认，得到 %q", got)
	}
}

// 这条是这个包存在的理由：解析若静默取值，Go 与 shell source 对重复键的
// 取值方向相反（第一个 vs 最后一个），换网关时两个组件会读到不同的端点，
// 而且完全无声。
func TestDuplicateKeyIsAnError(t *testing.T) {
	_, err := Load(write(t, "FALLBACK_MODEL=new\nFALLBACK_MODEL=old\n"))
	if err == nil {
		t.Fatal("重复键必须报错，不能静默取其一")
	}
	for _, want := range []string{"FALLBACK_MODEL", "重复"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应含 %q: %v", want, err)
		}
	}
}

// 少了 = 的行多半是贴错地方的凭据。这条报错会写进调度器日志、发到告警群里，
// 所以只能报到第几行，不能把这一行带出去。
func TestMalformedLineIsNotEchoed(t *testing.T) {
	const secret = "sk-ant-EXAMPLE0123456789"
	_, err := Load(write(t, "ENGINE=claude\nFALLBACK_AUTH_TOKEN: "+secret+"\n"))
	if err == nil {
		t.Fatal("缺 = 的行应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("报错把这一行的内容带出来了：%v", err)
	}
	if !strings.Contains(err.Error(), ":2 ") {
		t.Errorf("要报出是第几行：%v", err)
	}
}

func TestEqualComparesValuesNotPath(t *testing.T) {
	a := load(t, "ENGINE=claude\n")
	b := load(t, "# 加了一行注释\nENGINE=claude   # 行尾也有\n")
	if !a.Equal(b) {
		t.Error("只差注释与空白，键值相同，应当相等")
	}
	if a.Equal(load(t, "ENGINE=codex\n")) {
		t.Error("值不同不该相等")
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	// 只在本机看简报时整个配置都是可选的，不该因此拒绝启动
	c, err := Load(filepath.Join(t.TempDir(), "nope.conf"))
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if got := c.Get("ENGINE", "claude"); got != "claude" {
		t.Errorf("应回默认值，得到 %q", got)
	}
}

func TestMalformedLine(t *testing.T) {
	if _, err := Load(write(t, "ENGINE=claude\n这行没有等号\n")); err == nil {
		t.Fatal("非 KEY=value 的行应报错并指出行号")
	}
}

func TestRequireReportsAllMissingAtOnce(t *testing.T) {
	c := load(t, "FALLBACK_BASE_URL=https://x\n")
	err := c.Require("FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL")
	if err == nil {
		t.Fatal("应报缺项")
	}
	// 一个个报会让人来回改三遍
	for _, k := range []string{"FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("应一并报出 %s: %v", k, err)
		}
	}
	if strings.Contains(err.Error(), "FALLBACK_BASE_URL") {
		t.Error("已配置的项不该出现在缺项里")
	}
}

func TestListIgnoresBlanks(t *testing.T) {
	c := load(t, "SITE_LANGS= zh-CN , , en ,\n")
	got := c.List("SITE_LANGS")
	if len(got) != 2 || got[0] != "zh-CN" || got[1] != "en" {
		t.Errorf("List = %#v", got)
	}
}

// 危险模式默认必须是关着的：没写这个键就等于开着的话，装一台新机器、复制一份
// 示例配置，防护会全部失效而没有任何迹象。
func TestDangerDefaultsOff(t *testing.T) {
	for _, body := range []string{"", "ENGINE=claude\n", "DANGER=\n", `DANGER=""` + "\n"} {
		if load(t, body).Danger() {
			t.Errorf("DANGER 未明确打开时必须是关着的：%q", body)
		}
	}
	for _, body := range []string{"DANGER=1\n", "DANGER=on\n", "DANGER=YES\n"} {
		if !load(t, body).Danger() {
			t.Errorf("DANGER=%q 应该认出是开着", body)
		}
	}
	for _, body := range []string{"DANGER=0\n", "DANGER=off\n", "DANGER=false\n"} {
		if load(t, body).Danger() {
			t.Errorf("DANGER=%q 应该认出是关着", body)
		}
	}
}

// 认不出来的取值必须报错，不能猜。猜「开着」= 防护全关，猜「关着」= 以为开着
// 其实没开，两种都不报错。
func TestDangerRejectsUnknownValue(t *testing.T) {
	for _, v := range []string{"yes-please", "2", "danger", "tru"} {
		_, err := Load(write(t, "ENGINE=claude\nDANGER="+v+"\n"))
		if err == nil {
			t.Errorf("DANGER=%s 应该报错", v)
			continue
		}
		if !strings.Contains(err.Error(), "DANGER") {
			t.Errorf("报错里应含键名：%v", err)
		}
	}
	// 报错会进日志、会发到告警群，不回显值。
	// ⚠️ 别拿 "2" 这类取值来验这一条：行号本身就是数字，会被误判成回显。
	for _, v := range []string{"please-enable", "danger", "yes-please"} {
		_, err := Load(write(t, "ENGINE=claude\nDANGER="+v+"\n"))
		if err != nil && strings.Contains(err.Error(), v) {
			t.Errorf("报错不该回显值 %q：%v", v, err)
		}
	}
}
