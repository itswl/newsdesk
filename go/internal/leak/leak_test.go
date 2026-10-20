package leak

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 全部是编造的形状样本，不要用任何真实凭据派生的串——仓库是公开的，
// 测试文件本身也会被读到。
func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCatchesCommonShapes(t *testing.T) {
	d := t.TempDir()
	cases := map[string]string{
		"anthropic.md": "key: sk-ant-api03-EXAMPLEEXAMPLEEXAMPLEEXAMPLE",
		"openai.md":    "key: sk-EXAMPLE0123456789EXAMPLE0123456789",
		"github.md":    "ghp_EXAMPLE0123456789EXAMPLE0123456789",
		"cf.md":        "cfut_EXAMPLE0123456789EXAMPLE0123456789",
		"aws.md":       "AKIAIOSFODNN7EXAMPLE", // AWS 文档里的官方示例值
		"feishu.md":    "https://open.larksuite.com/open-apis/bot/v2/hook/00000000-0000-0000-0000-000000000000",
		"pem.md":       "-----BEGIN RSA PRIVATE KEY-----",
	}
	for n, b := range cases {
		write(t, d, n, b)
	}
	found := map[string]bool{}
	for _, h := range Scan([]string{d}, nil) {
		found[filepath.Base(h.File)] = true
	}
	for n := range cases {
		if !found[n] {
			t.Errorf("漏掉 %s", n)
		}
	}
}

func TestOrdinaryReportIsClean(t *testing.T) {
	d := t.TempDir()
	// 报告里天天有链接、代码块、长串英文，不能一惊一乍
	write(t, d, "r.md", "# 日报\n\n- [仓库](https://github.com/foo/bar) ★12.3k\n"+
		"`npm ci --no-audit` 与 `sha256:abcdef0123456789abcdef0123456789`\n"+
		"提交 a3b7179da8d10b5f，版本 v1.2.3-rc4\n")
	if h := Scan([]string{d}, nil); len(h) != 0 {
		t.Errorf("误报: %+v", h)
	}
}

func TestConfigSecretsAndShortValues(t *testing.T) {
	d := t.TempDir()
	write(t, d, "a.md", "token is SUPER-SECRET-VALUE-123456 here")
	// 太短的值不能当凭据，否则 GITHUB_TOKEN=none 会命中一切
	hits := Scan([]string{d}, map[string]string{"X": "none", "TOK": "SUPER-SECRET-VALUE-123456"})
	if len(hits) != 1 || !strings.Contains(hits[0].What, "TOK") {
		t.Errorf("应只命中 TOK 一条，得到 %+v", hits)
	}
}

func TestReportsLocationNotContent(t *testing.T) {
	d := t.TempDir()
	secret := "ghp_EXAMPLE0123456789EXAMPLE0123456789"
	write(t, d, "a.md", "x\ny\n"+secret)
	hits := Scan([]string{d}, nil)
	if len(hits) != 1 || hits[0].Line != 3 {
		t.Fatalf("行号不对: %+v", hits)
	}
	// 把命中的凭据打进日志就自相矛盾了
	if strings.Contains(hits[0].What, secret) {
		t.Error("What 里不该含凭据原文")
	}
}

func TestOnlyScansTextOutputs(t *testing.T) {
	d := t.TempDir()
	write(t, d, "x.png", "ghp_EXAMPLE0123456789EXAMPLE0123456789")
	if h := Scan([]string{d}, nil); len(h) != 0 {
		t.Errorf("不该扫非文本产出: %+v", h)
	}
}

// 结果写进调用方给的 writer（流水线给的是这一档的任务日志）。以前直接打到标准输出，
// 守护进程下只落在 scheduler.log：单档日志里看不出做没做过自检，拒绝发布时也看不到
// 是哪个文件哪一行。
func TestReportWritesToGivenWriter(t *testing.T) {
	var buf bytes.Buffer
	if Report(&buf, "/repo", nil) {
		t.Fatal("没有命中不该报有")
	}
	if !strings.Contains(buf.String(), "未发现凭据") {
		t.Fatalf("没有命中时也要留一行：%q", buf.String())
	}

	buf.Reset()
	hits := []Hit{{File: "/repo/reports/2026-10-20/momoyu.md", Line: 3, What: "EXAMPLE 形状"}}
	if !Report(&buf, "/repo", hits) {
		t.Fatal("有命中应报有")
	}
	if !strings.Contains(buf.String(), "reports/2026-10-20/momoyu.md:3") {
		t.Fatalf("要写出相对路径与行号：%q", buf.String())
	}
}
