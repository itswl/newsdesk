package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// 一次调用的用量。
//
// 数字来自 claude --output-format stream-json 的 result 事件，那一份是整场会话的
// 累计（每轮请求的输入都会加上去），所以它是「这次调用一共消耗了多少 token」，
// 不是「最后一次请求有多大」。
//
// 带 json 标签是因为它要落进 state/schedule.json：日志会滚，事后要能回答
// 「这一档花了多少」只能靠状态文件，newsdesk status 也从那里读。
type Usage struct {
	Input      int     `json:"input"`              // 每次请求的提示词 token 之和
	Output     int     `json:"output"`             // 生成 token 之和（含思考）
	CacheRead  int     `json:"cache_read"`         // 命中缓存的输入
	CacheWrite int     `json:"cache_write"`        // 写入缓存的输入
	Thinking   int     `json:"thinking,omitempty"` // output 里的思考部分，能取到才有意义
	CostUSD    float64 `json:"cost_usd"`           // CLI 按自己的价目表估的，第三方网关只能当参考
	Turns      int     `json:"turns"`
	Known      bool    `json:"-"` // 拿到 result 事件没有。只有 Known 的才落盘，所以不序列化
}

// String 一行可读的用量，直接进任务日志。
//
// ⚠️ Known 为假时不能打 0：进程被信号杀掉、输出被截断时都拿不到 result，
// 把「没取到」写成「用了 0」比不写更糟——看日志的人会以为那一档很便宜。
func (u Usage) String() string {
	if !u.Known {
		return "=== 用量：没取到（进程没有给出 result 事件，多半是被中断）==="
	}
	return "=== 用量：" + u.Brief(true) + " ==="
}

// Brief 不带框的一行，给 status 用。
//
// withCost 为假时不打成本：成本是 claude CLI 按 Anthropic 价目估的，走第三方网关
// （api 引擎）时模型名它都不认识，那个数字对不上任何账单，打出来只会误导。
func (u Usage) Brief(withCost bool) string {
	s := fmt.Sprintf("输入 %s · 缓存读 %s · 缓存写 %s · 输出 %s",
		thousands(u.Input), thousands(u.CacheRead), thousands(u.CacheWrite), thousands(u.Output))
	if u.Thinking > 0 {
		s += fmt.Sprintf("（思考 %s）", thousands(u.Thinking))
	}
	s += fmt.Sprintf(" · %d 轮", u.Turns)
	if withCost {
		s += fmt.Sprintf(" · 成本 $%.4f（CLI 估算）", u.CostUSD)
	}
	return s
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// streamEvent 是 stream-json 的一行。只声明用得上的字段——CLI 加字段不该让解析失败。
type streamEvent struct {
	Type     string  `json:"type"`
	Subtype  string  `json:"subtype"`
	ToolName string  `json:"tool_name"`
	IsError  bool    `json:"is_error"`
	Result   string  `json:"result"`
	NumTurns int     `json:"num_turns"`
	Cost     float64 `json:"total_cost_usd"`
	Usage    struct {
		Input      int `json:"input_tokens"`
		Output     int `json:"output_tokens"`
		CacheRead  int `json:"cache_read_input_tokens"`
		CacheWrite int `json:"cache_creation_input_tokens"`
		Details    struct {
			Thinking int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Message streamMessage `json:"message"`
	// rate_limit_event：订阅的两个用量窗口当前用到几成。只有 claude 引擎有，
	// api 走第三方网关一般没有。
	RateLimit struct {
		Status  string `json:"status"`
		Windows map[string]struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    int64   `json:"resetsAt"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

// streamMessage 事件里的 message 字段。
//
// ⚠️ 它不总是对象。assistant / user 事件里是 {"content":[…]}，但 claude CLI 2.1.303 起
// system/permission_denied 事件里它是一个字符串（拒绝的原因）。以前按对象声明，
// json 一碰到字符串就报错——2026-10-19 两档的用量行就是这么丢的。两种形状都收下：
// 对象取 content 里的文本块，字符串原样留在 Text。其他形状当没有，不让整行失败。
type streamMessage struct {
	Text    string
	Content []streamBlock
}

type streamBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (m *streamMessage) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	switch b[0] {
	case '"':
		return json.Unmarshal(b, &m.Text)
	case '{':
		var obj struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(b, &obj); err != nil || len(obj.Content) == 0 {
			return nil
		}
		switch c := bytes.TrimSpace(obj.Content); {
		case len(c) > 0 && c[0] == '[':
			_ = json.Unmarshal(c, &m.Content)
		case len(c) > 0 && c[0] == '"':
			// 接口允许 content 直接是字符串
			_ = json.Unmarshal(c, &m.Text)
		}
	}
	return nil
}

// renderStream 把 stream-json 还原成人看的日志，并取出这场的用量。
//
// 按行解析：stream-json 一行一个事件。认不出的行（CLI 升级加的新事件、形状变了的
// 字段、非 JSON 的杂音）原样照打、继续往下。以前用 json.Decoder 串着解，一行解不动
// 就把剩下的整段倾进日志、result 事件随之错过，用量就没了——而 CLI 是自动升级的，
// 事件形状会在无人值守时变。
//
// 换成流式 JSON 是为了拿 token 统计，但有三件事必须保住：
//   - 模型的话要原样进日志（以前的文本输出就是这个，事后排查全靠它）；
//   - 失败时错误正文要进日志——stream-json 下 stderr 是空的，配额/凭据/审核的
//     提示只存在于 result 事件的 result 字段里，不打出来 Classify() 就成了瞎子；
//   - 绝不静默丢输出。
func renderStream(r io.Reader, out io.Writer) (Usage, error) {
	var u Usage
	br := bufio.NewReader(r)
	st := &renderState{}
	for {
		line, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev streamEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				// 解析不动的行照打。宁可日志难看，也不能让「模型说了什么」
				// 和「为什么失败」一起消失。
				out.Write(line)
				if !bytes.HasSuffix(line, []byte("\n")) {
					fmt.Fprintln(out)
				}
			} else {
				renderEvent(ev, st, &u, out)
			}
		}
		if rerr == io.EOF {
			return u, nil
		}
		if rerr != nil {
			return u, rerr
		}
	}
}

// renderState 跨事件要记住的东西：最后一段话（result 成功时是它的副本，不重复打）
// 和上一次打过的配额（同一场里只在明显变化时再打）。
type renderState struct {
	lastText string
	rate     *rateSeen
}

// rateSeen 上一次打出来的配额。每次 API 往返都会来一条 rate_limit_event，利用率逐次
// 爬 1%，逐条打的话一档就是二十来行——首条照打，之后只在状态变了或某个窗口挪了
// 10 个点以上才再打。
type rateSeen struct {
	status     string
	five, week float64
}

// renderEvent 处理一个事件。
func renderEvent(ev streamEvent, st *renderState, u *Usage, out io.Writer) {
	switch ev.Type {
	case "assistant":
		for _, b := range ev.Message.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				fmt.Fprintln(out, b.Text)
				st.lastText = b.Text
			}
		}
	case "rate_limit_event":
		// 订阅的用量窗口用到几成。它只在这里能看到：切兜底的那一档，日志里
		// 「hit your weekly limit」之前的这一行能说明是窗口被谁用光的。
		if line, seen := rateLine(ev); line != "" && st.rate.moved(seen) {
			fmt.Fprintln(out, line)
			st.rate = &seen
		}
	case "system":
		// 工具调用被权限层自动拒绝（无人值守没有审批面）。模型会自己绕过去，
		// 但这件事要留在日志里：以前只能从原样倾出来的 JSON 里看出来。
		if ev.Subtype == "permission_denied" {
			fmt.Fprintf(out, "⚠ 工具调用被拒（%s）：%s\n", ev.ToolName, strings.TrimSpace(ev.Message.Text))
		}
	case "result":
		*u = Usage{
			Input: ev.Usage.Input, Output: ev.Usage.Output,
			CacheRead: ev.Usage.CacheRead, CacheWrite: ev.Usage.CacheWrite,
			Thinking: ev.Usage.Details.Thinking,
			CostUSD:  ev.Cost, Turns: ev.NumTurns, Known: true,
		}
		// 失败时错误正文只在这儿。成功时 result 是最后一段话的副本，不重复打。
		if ev.IsError && strings.TrimSpace(ev.Result) != "" && ev.Result != st.lastText {
			fmt.Fprintln(out, ev.Result)
		}
	}
}

// moved 这次的配额值得再打一行吗。
func (r *rateSeen) moved(now rateSeen) bool {
	if r == nil {
		return true
	}
	step := func(a, b float64) bool { return math.Abs(a-b) >= 0.10 }
	return r.status != now.status || step(r.five, now.five) || step(r.week, now.week)
}

// rateLine 把 rate_limit_event 压成一行：「⏱ 配额：5 小时窗 78%（14:00 重置）· 本周 13%（10-26 09:00 重置）」。
// 没有窗口数据就不打（有些事件只报 status）。
func rateLine(ev streamEvent) (string, rateSeen) {
	seen := rateSeen{status: ev.RateLimit.Status}
	var parts []string
	for _, w := range []struct {
		key, label, layout string
		into               *float64
	}{
		{"five_hour", "5 小时窗", "15:04", &seen.five},
		{"seven_day", "本周", "01-02 15:04", &seen.week},
	} {
		win, ok := ev.RateLimit.Windows[w.key]
		if !ok {
			continue
		}
		*w.into = win.Utilization
		s := fmt.Sprintf("%s %.0f%%", w.label, win.Utilization*100)
		if win.ResetsAt > 0 {
			s += fmt.Sprintf("（%s 重置）", time.Unix(win.ResetsAt, 0).Format(w.layout))
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "", seen
	}
	line := "⏱ 配额：" + strings.Join(parts, " · ")
	if ev.RateLimit.Status != "" && ev.RateLimit.Status != "allowed" {
		line += "（" + ev.RateLimit.Status + "）"
	}
	return line, seen
}
