package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		"Claude usage limit reached": FailQuota,
		"HTTP 429 Too Many Requests": FailQuota,
		"rate_limit_error":           FailQuota,
		"You've hit your weekly limit · resets Oct 5 at 9am (Asia/Singapore)": FailQuota,
		"You've hit your session limit · resets 2:20pm (Asia/Singapore)":      FailQuota,
		"Insufficient Balance":       FailBalance,
		"402 Payment Required":       FailBalance,
		"余额不足，请充值":                   FailBalance,
		"your account is in arrears": FailBalance,
		"invalid api key":            FailAuth,
		"API key expired":            FailAuth,
		"401 Unauthorized":           FailAuth,
		"Failed to authenticate: OAuth session expired and could not be refreshed":  FailAuth,
		"Your organization has disabled Claude subscription access for Claude Code": FailAuth,
		// 供应商内容审核：2026-10-17 21:45 那档 momoyu 的真实报错
		`API Error: 400 [1301][系统检测到输入或生成内容可能包含不安全或敏感内容，请您避免输入易产生敏感内容的提示语，感谢您的配合。][20261017220202b27a8e82dd194ed3]`: FailModeration,
		"content_filter triggered by the provider": FailModeration,
		"Generation stopped by our safety system":  FailModeration,
		"FileNotFoundError: data/x.json":           FailOther,
		"connection reset by peer":                 FailOther,
		"":                                         FailOther,
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
	for _, f := range []Failure{FailQuota, FailBalance, FailAuth, FailModeration} {
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
		"CLAUDE_MODEL=claude-sonnet-5-5",
		"CODEX_MODEL=gpt-5.6-luna",
		"ENGINE_FALLBACK=api",
		"FALLBACK_BASE_URL=https://gw.example.com/v1",
		"FALLBACK_MODEL=some-model",
		"FALLBACK_AUTH_TOKEN=EXAMPLE0123456789",
	}, "\n"))

	for kind, want := range map[Kind]string{
		Claude: "claude / claude-sonnet-5-5",
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

// 分析层的工具面必须是钉死的。这串参数里每一条都是一个开关，写漏一条不会有任何
// 报错——要么安静地多继承一层交互式会话的配置，要么安静地少一层防护。
func TestClaudeArgsPinAnalysisLayer(t *testing.T) {
	cfg := cfgOf(t, "CLAUDE_MODEL=claude-sonnet-5-5\nFALLBACK_MODEL=glm-5.3\n")

	in := func(args []string, want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}

	for _, kind := range []Kind{Claude, API} {
		normal := claudeArgs(kind, cfg, "settings.json", "p", false)
		// 钉工具面、不继承全局设置、MCP 照裁、权限提示照关
		for _, want := range []string{
			"--tools", toolsAllowed,
			"--setting-sources",
			"--strict-mcp-config", "--permission-prompts",
		} {
			if !in(normal, want) {
				t.Errorf("%s 常规模式缺参数 %q：%v", kind, want, normal)
			}
		}
		if in(normal, "--dangerously-skip-permissions") {
			t.Errorf("%s 常规模式不该带 --dangerously-skip-permissions", kind)
		}

		// 危险模式反着来：不设限，一个都不留
		danger := claudeArgs(kind, cfg, "settings.json", "p", true)
		if !in(danger, "--dangerously-skip-permissions") {
			t.Errorf("%s 危险模式缺 --dangerously-skip-permissions：%v", kind, danger)
		}
		for _, bad := range []string{"--tools", "--setting-sources", "--strict-mcp-config", "--permission-prompts"} {
			if in(danger, bad) {
				t.Errorf("%s 危险模式不该有 %q（那就不叫不设限了）", kind, bad)
			}
		}
	}

	// 干活的工具一个不能少：少了 Write 就出不了报告，少了 Bash 就没法核对数据
	for _, want := range []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash"} {
		if !strings.Contains(toolsAllowed, want) {
			t.Errorf("工具白名单里没有 %s", want)
		}
	}
}

// 「400 + 敏感内容」必须认成审核，而不是落到 other 去重试三次。
// 这一条也顺带钉住顺序：审核的报错里有 400，先认配额/凭据都会给出错的提示。
func TestModerationWinsOverQuota(t *testing.T) {
	in := `API Error: 400 [1301][系统检测到输入或生成内容可能包含不安全或敏感内容][abc]`
	if got := Classify(in); got != FailModeration {
		t.Fatalf("= %q，应为 moderation", got)
	}
	if got := Classify("rate limit exceeded while generating sensitive content"); got != FailQuota {
		t.Errorf("普通的限流不该被认成审核：%q", got)
	}
}

// 用量行：数字要对，没取到的时候不能假装是 0。
func TestUsageLine(t *testing.T) {
	u := Usage{Input: 17431, Output: 4567, CacheRead: 90123, CacheWrite: 0, Turns: 18, CostUSD: 0.0421, Known: true}
	s := u.String()
	for _, want := range []string{"17,431", "4,567", "90,123", "18 轮", "$0.0421"} {
		if !strings.Contains(s, want) {
			t.Errorf("用量行缺 %q：%s", want, s)
		}
	}
	if !strings.Contains(Usage{}.String(), "没取到") {
		t.Error("拿不到用量时必须说「没取到」，不能打 0")
	}
}

// 流式 JSON 要还原成人看的日志，同时把用量取出来。
// 这里用两组真实的观测数据：一次成功、一次配额失败（失败时 stderr 是空的，
// 错误正文只在 result 里——不打出来 Classify 就成了瞎子）。
//
// 成功那组夹着 claude CLI 2.1.303 实测会出现的几种「不是对象」的 message：
// system/permission_denied 里 message 是字符串、thinking_tokens 事件根本没有 message、
// user 事件的 content 是 tool_result 块。以前按对象声明，碰到字符串整段解析放弃，
// 后面的 result 事件错过，用量行就丢了（2026-10-19 两档）。
func TestRenderStream(t *testing.T) {
	okJSONL := `{"type":"system","subtype":"init","tools":["Read"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"第一段话"}]}}
{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"for f in *.json; do cat $f; done"}}]}}
{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"t1","message":"Permission for this tool use was denied. What required approval: A variable in this command can't be checked before it runs"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"Permission for this tool use was denied.","is_error":true}]}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792389600,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.78,"resetsAt":1792389600},"seven_day":{"utilization":0.13,"resetsAt":1792976400}}}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792389600,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.79,"resetsAt":1792389600},"seven_day":{"utilization":0.13,"resetsAt":1792976400}}}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792389600,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.91,"resetsAt":1792389600},"seven_day":{"utilization":0.13,"resetsAt":1792976400}}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"好的。"}]}}
{"type":"result","is_error":false,"num_turns":3,"total_cost_usd":0.0013,"result":"好的。","usage":{"input_tokens":243,"output_tokens":4,"cache_read_input_tokens":1344,"cache_creation_input_tokens":7,"output_tokens_details":{"thinking_tokens":2}}}
`
	var b strings.Builder
	u, err := renderStream(strings.NewReader(okJSONL), &b)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "第一段话") || !strings.Contains(out, "好的。") {
		t.Errorf("模型的话没有原样落到日志：%s", out)
	}
	if strings.Count(out, "好的。") != 1 {
		t.Errorf("成功时 result 是最后一段的副本，不该重复打：%s", out)
	}
	if !u.Known || u.Input != 243 || u.Output != 4 || u.CacheRead != 1344 || u.CacheWrite != 7 || u.Turns != 3 {
		t.Errorf("用量取错：%+v", u)
	}
	if !strings.Contains(out, "工具调用被拒（Bash）") || !strings.Contains(out, "can't be checked") {
		t.Errorf("权限拒绝要以人看的形式留在日志里：%s", out)
	}
	if strings.Contains(out, `{"type":`) {
		t.Errorf("认得的事件不该以原始 JSON 倾进日志：%s", out)
	}
	// 用量窗口用到几成只有这里能看到；没有窗口数据的那条不打，每次往返爬 1% 的不重复打，
	// 挪了 10 个点以上才再打一行
	if !strings.Contains(out, "配额：5 小时窗 78%") || !strings.Contains(out, "本周 13%") {
		t.Errorf("rate_limit_event 应当打成配额行：%s", out)
	}
	if strings.Count(out, "配额：") != 2 || !strings.Contains(out, "5 小时窗 91%") {
		t.Errorf("配额行应当恰好两条（78%% 和 91%%；无窗口数据的不打、79%% 那条不打）：%s", out)
	}

	failJSONL := `{"type":"rate_limit_event"}
{"type":"assistant","message":{"content":[{"type":"text","text":"You've hit your weekly limit · resets Oct 19 at 9am"}]},"error":"rate_limit"}
{"type":"result","is_error":true,"num_turns":1,"total_cost_usd":0,"result":"You've hit your weekly limit · resets Oct 19 at 9am","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
`
	b.Reset()
	if _, err := renderStream(strings.NewReader(failJSONL), &b); err != nil {
		t.Fatal(err)
	}
	out = b.String()
	if strings.Count(out, "weekly limit") != 1 {
		t.Errorf("错误正文应当恰好出现一次（assistant 和 result 是同一句）：%s", out)
	}
	if got := Classify(out); got != FailQuota {
		t.Errorf("还原出来的日志要能被 Classify 认出来，现在 = %q", got)
	}
}

// 解析不动的行不能吞：照打，而且**继续往下解**——「为什么失败」可能就在那一行，
// 而用量在它后面。以前一行解不动就把剩下的整段倾出去、result 跟着丢。
func TestRenderStreamKeepsGarbageAndGoesOn(t *testing.T) {
	in := "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"开头\"}]}}\n" +
		"这不是 JSON\n" +
		"{\"type\":\"result\",\"is_error\":false,\"num_turns\":2,\"result\":\"开头\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}"
	var b strings.Builder
	u, err := renderStream(strings.NewReader(in), &b)
	if err != nil {
		t.Fatalf("杂音行不该让整个流失败：%v", err)
	}
	if !strings.Contains(b.String(), "这不是 JSON") {
		t.Errorf("非 JSON 的行被吞了：%s", b.String())
	}
	if !u.Known || u.Input != 10 || u.Turns != 2 {
		t.Errorf("杂音之后的 result 应当照常取到（最后一行没有换行符也要算）：%+v", u)
	}
}

// message 字段三种形状都要能过：对象、字符串、content 是字符串的对象。
func TestStreamMessageShapes(t *testing.T) {
	var ev streamEvent
	if err := json.Unmarshal([]byte(`{"type":"system","message":"denied: 原因"}`), &ev); err != nil {
		t.Fatalf("字符串 message 不该报错：%v", err)
	}
	if ev.Message.Text != "denied: 原因" {
		t.Errorf("字符串 message 没留下来：%+v", ev.Message)
	}
	ev = streamEvent{}
	if err := json.Unmarshal([]byte(`{"type":"user","message":{"role":"user","content":"纯文本"}}`), &ev); err != nil || ev.Message.Text != "纯文本" {
		t.Errorf("content 为字符串的 message：err=%v %+v", err, ev.Message)
	}
	ev = streamEvent{}
	if err := json.Unmarshal([]byte(`{"type":"x","message":42}`), &ev); err != nil {
		t.Errorf("认不出的形状当没有，不报错：%v", err)
	}
}

// 引擎子进程的环境不能照单全收：调度器从 Claude Code 会话里启动过一次，
// CLAUDE_CODE_EFFORT_LEVEL=max 就跟着流进了每一档，思考 token 翻了一倍多；
// 反方向，shell 里的 ANTHROPIC_API_KEY 会把订阅那次劫走。只留自己注入的那几条。
func TestChildEnvScrubsAnthropicAndClaudeCode(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-EXAMPLE")
	t.Setenv("ANTHROPIC_BASE_URL", "https://stray.example")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "max")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "EXAMPLE")
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/cfg")
	t.Setenv("NEWSDESK_TEST_KEEP", "1")
	env := childEnv([]string{"ANTHROPIC_BASE_URL=https://gateway.example", "ANTHROPIC_AUTH_TOKEN=EXAMPLE"})
	has := func(prefix string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	for _, gone := range []string{"ANTHROPIC_API_KEY=", "CLAUDE_CODE_EFFORT_LEVEL=", "CLAUDECODE=", "CLAUDE_CODE_MESSAGING_TOKEN=", "ANTHROPIC_BASE_URL=https://stray"} {
		if has(gone) {
			t.Errorf("%s 不该传给引擎", gone)
		}
	}
	for _, keep := range []string{"CLAUDE_CONFIG_DIR=/tmp/cfg", "NEWSDESK_TEST_KEEP=1", "ANTHROPIC_BASE_URL=https://gateway.example", "ANTHROPIC_AUTH_TOKEN=EXAMPLE"} {
		if !has(keep) {
			t.Errorf("%s 应当保留：%v", keep, env)
		}
	}
}

// 用量要落进 state/schedule.json，所以 JSON 往返不能丢字段；Known 不进文件。
func TestUsageJSONRoundTrip(t *testing.T) {
	u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Thinking: 5, CostUSD: 0.5, Turns: 6, Known: true}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Known") || strings.Contains(string(raw), "known") {
		t.Errorf("Known 不该序列化：%s", raw)
	}
	var back Usage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	back.Known = true
	if back != u {
		t.Errorf("往返丢字段：%+v → %+v", u, back)
	}
	if s := u.Brief(false); strings.Contains(s, "$") || !strings.Contains(s, "6 轮") {
		t.Errorf("status 那行不带成本：%s", s)
	}
}

// execStream 的两条路（解析后的 stdout、os/exec 拷来的 stderr）会并发写同一个
// writer。实测丢过一整行输出，所以必须串起来——这里只是把「同时写不丢不串」钉住。
func TestLockedWriterSerializes(t *testing.T) {
	var buf bytes.Buffer
	lw := &lockedWriter{w: &buf}
	var wg sync.WaitGroup
	const n, size = 32, 200
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := lw.Write(bytes.Repeat([]byte("x"), size)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if want := n * 20 * size; buf.Len() != want {
		t.Errorf("写了 %d 字节，应为 %d——并发写丢东西了", buf.Len(), want)
	}
}
