package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/newsdesk/internal/config"
)

func cfgOf(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassify(t *testing.T) {
	cases := map[string]Failure{
		"Claude usage limit reached":     FailQuota,
		"HTTP 429 Too Many Requests":     FailQuota,
		"rate_limit_error":               FailQuota,
		"Insufficient Balance":           FailBalance,
		"402 Payment Required":           FailBalance,
		"余额不足，请充值":                       FailBalance,
		"your account is in arrears":     FailBalance,
		"invalid api key":                FailAuth,
		"API key expired":                FailAuth,
		"401 Unauthorized":               FailAuth,
		"FileNotFoundError: data/x.json": FailOther,
		"connection reset by peer":       FailOther,
		"":                               FailOther,
	}
	for in, want := range cases {
		if got := Classify(in); got != want {
			t.Errorf("Classify(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 余额的报错里常带 quota/limit 字样。认成配额的话提示会是「等窗口重置」
// ——等到天亮也不会好，该做的事是去充值。
func TestBalanceWinsOverQuota(t *testing.T) {
	if got := Classify("Insufficient quota: please recharge"); got != FailBalance {
		t.Errorf("= %q，应为 balance", got)
	}
}

func TestEveryFailureHasActionableHint(t *testing.T) {
	for _, f := range []Failure{FailQuota, FailBalance, FailAuth} {
		label, action := f.Hint()
		if label == "" || len(action) < 15 {
			t.Errorf("%s 的提示说不清该做什么: %q / %q", f, label, action)
		}
	}
}

// 无人值守跑的东西，最难查的问题之一是「这份报告是哪个引擎写的」。
// 切了兜底而日志只有一句「完成」的话，风格突变就只能靠猜。
func TestDescribeNamesEngineAndModel(t *testing.T) {
	cfg := cfgOf(t, strings.Join([]string{
		"ENGINE=claude",
		"CLAUDE_MODEL=claude-sonnet-5",
		"CODEX_MODEL=gpt-5.6-luna",
		"ENGINE_FALLBACK=api",
		"FALLBACK_BASE_URL=https://gw.example.com/v1",
		"FALLBACK_MODEL=some-model",
		"FALLBACK_AUTH_TOKEN=EXAMPLE0123456789",
	}, "\n"))

	for kind, want := range map[Kind]string{
		Claude: "claude / claude-sonnet-5",
		Codex:  "codex / gpt-5.6-luna",
		// api 走的仍是 claude CLI，只是换了端点——必须连端点一起报，
		// 同一个 "api" 指向不同网关时产出可以完全不同。
		API: "api / some-model @ gw.example.com",
	} {
		if got := Describe(cfg, kind); got != want {
			t.Errorf("%s → %q，期望 %q", kind, got, want)
		}
	}
}

// 端点里带路径、端口或 user:pass@ 时只取主机名：全量打出来太长，
// 也避免把凭据写进日志。
func TestDescribeTrimsEndpointToHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://gw.example.com/v1":            "gw.example.com",
		"https://gw.example.com:8443/api/v1/":  "gw.example.com:8443",
		"https://user:EXAMPLE@gw.example.com/": "gw.example.com",
		"gw.example.com":                       "gw.example.com",
	} {
		if got := host(raw); got != want {
			t.Errorf("%s → %q，期望 %q", raw, got, want)
		}
	}
}

// 没配兜底是个该知道的事实：主引擎配额耗尽时那一档直接跳过，
// 不能只是不显示。
func TestPlanLineSaysWhenNoFallback(t *testing.T) {
	cfg := cfgOf(t, "ENGINE=codex\nCODEX_MODEL=gpt-5.6-luna\n")
	got := PlanLine(cfg)
	if !strings.Contains(got, "codex / gpt-5.6-luna") {
		t.Errorf("主引擎没报全：%s", got)
	}
	if !strings.Contains(got, "未配置") || !strings.Contains(got, "跳过当档") {
		t.Errorf("没配兜底时要说清后果：%s", got)
	}
}

// 端点没配却选了 api，描述里要说出来——否则日志显示 "api / " 后面空着，
// 看起来像正常运行。
func TestDescribeFlagsMissingEndpoint(t *testing.T) {
	cfg := cfgOf(t, "ENGINE_FALLBACK=api\nFALLBACK_MODEL=m\n")
	if got := Describe(cfg, API); !strings.Contains(got, "端点未配置") {
		t.Errorf("缺端点要报出来：%s", got)
	}
}
