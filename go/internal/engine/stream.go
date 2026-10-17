package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 一次调用的用量。
//
// 数字来自 claude --output-format stream-json 的 result 事件，那一份是整场会话的
// 累计（每轮请求的输入都会加上去），所以它是「这次调用一共消耗了多少 token」，
// 不是「最后一次请求有多大」。
type Usage struct {
	Input      int     // 每次请求的提示词 token 之和
	Output     int     // 生成 token 之和（含思考）
	CacheRead  int     // 命中缓存的输入
	CacheWrite int     // 写入缓存的输入
	Thinking   int     // output 里的思考部分，能取到才有意义
	CostUSD    float64 // CLI 按自己的价目表估的，第三方网关只能当参考
	Turns      int
	Known      bool // 拿到 result 事件没有
}

// String 一行可读的用量，直接进任务日志。
//
// ⚠️ Known 为假时不能打 0：进程被信号杀掉、输出被截断时都拿不到 result，
// 把「没取到」写成「用了 0」比不写更糟——看日志的人会以为那一档很便宜。
func (u Usage) String() string {
	if !u.Known {
		return "=== 用量：没取到（进程没有给出 result 事件，多半是被中断）==="
	}
	s := fmt.Sprintf("=== 用量：输入 %s · 缓存读 %s · 缓存写 %s · 输出 %s",
		thousands(u.Input), thousands(u.CacheRead), thousands(u.CacheWrite), thousands(u.Output))
	if u.Thinking > 0 {
		s += fmt.Sprintf("（思考 %s）", thousands(u.Thinking))
	}
	s += fmt.Sprintf(" · %d 轮 · 成本 $%.4f（CLI 估算） ===", u.Turns, u.CostUSD)
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
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// renderStream 把 stream-json 还原成人看的日志，并取出这场的用量。
//
// 换成流式 JSON 是为了拿 token 统计，但有三件事必须保住：
//   - 模型的话要原样进日志（以前的文本输出就是这个，事后排查全靠它）；
//   - 失败时错误正文要进日志——stream-json 下 stderr 是空的，配额/凭据/审核的
//     提示只存在于 result 事件的 result 字段里，不打出来 Classify() 就成了瞎子；
//   - 解析不动的时候把原始字节照打，绝不静默丢输出。
func renderStream(r io.Reader, out io.Writer) (Usage, error) {
	var u Usage
	dec := json.NewDecoder(r)
	var lastText string

	for {
		var ev streamEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return u, nil
			}
			// 解析不动：把剩下的字节整段打出去。宁可日志难看，也不能让
			// 「模型说了什么」和「为什么失败」一起消失。
			rest, rerr := io.ReadAll(io.MultiReader(dec.Buffered(), r))
			if rerr == nil && len(rest) > 0 {
				out.Write(rest)
				if !strings.HasSuffix(string(rest), "\n") {
					fmt.Fprintln(out)
				}
			}
			return u, err
		}

		switch ev.Type {
		case "assistant":
			for _, b := range ev.Message.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					fmt.Fprintln(out, b.Text)
					lastText = b.Text
				}
			}
		case "result":
			u = Usage{
				Input: ev.Usage.Input, Output: ev.Usage.Output,
				CacheRead: ev.Usage.CacheRead, CacheWrite: ev.Usage.CacheWrite,
				Thinking: ev.Usage.Details.Thinking,
				CostUSD:  ev.Cost, Turns: ev.NumTurns, Known: true,
			}
			// 失败时错误正文只在这儿。成功时 result 是最后一段话的副本，不重复打。
			if ev.IsError && strings.TrimSpace(ev.Result) != "" && ev.Result != lastText {
				fmt.Fprintln(out, ev.Result)
			}
		}
	}
}
