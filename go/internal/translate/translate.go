// Package translate 把中文报告翻成英文。
//
// 只做英文。繁体不经过这里——它在渲染时由词表现转（24 KB 报告 0.02 秒），
// 存一份反而多一处会过期的副本。
//
// **直接调 API，不走 agentic 循环**：翻译是纯转换，不需要工具也不需要沙箱，
// 一次 messages 调用比让模型带着工具跑一圈便宜也可控得多。
package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxTokens = 16000
	// 超过这个长度就按标题切块。实测 23k 字符的报告一次翻会撞上输出上限被截断，
	// 而截断是静默的：文件看着正常，只是后半截没了。
	chunkChars = 5000
	retries    = 2
	// 分块彼此独立，但供应商也有并发/限流；小并发能缩短长报告耗时，又不把
	// 一次翻译变成一阵突发请求。
	maxConcurrent = 2
	// UA 必须显式设：默认的 Go-http-client 会被 Cloudflare 按客户端签名封掉
	// （实测 Python-urllib 的默认 UA 在某网关返回 403 error code 1010，
	// 而那条错误长得像鉴权失败，很容易一路查到密钥上去）。
	userAgent = "newsdesk/2.0 (+https://github.com/itswl/newsdesk)"
)

const system = `You translate Chinese tech-briefing reports into English.
Rules:
1. Output ONLY the translated Markdown. No preamble, no explanation, no code fence around the whole thing.
2. Preserve the Markdown structure exactly: heading levels, table rows and column counts, list markers, bold/italic, blockquotes, horizontal rules.
3. Do NOT translate: URLs, repository names (owner/repo), package names, code inside backticks, product names, people's names, model identifiers.
4. Keep all numbers, dates and units exactly as they are.
5. Translate table headers and cell text, but keep the alignment row (|---|:-:|) unchanged.
6. Chinese platform names: keep a recognisable English form with the original in parentheses on first use, e.g. Douban (豆瓣), Zhihu (知乎), Hupu (虎扑).
7. Write natural, concise English. This is a professional briefing, not a literal gloss.`

type Client struct {
	BaseURL, Token, Model string
	HTTP                  *http.Client
}

func New(base, token, model string) *Client {
	return &Client{BaseURL: base, Token: token, Model: model,
		HTTP: &http.Client{
			Timeout: 5 * time.Minute,
			// 不跟随重定向：Go 和 urllib 一样会把请求头原样带给新主机，
			// 令牌就跟着送出去了。正常的 API 端点不会重定向。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("API 端点返回了重定向；为避免令牌被转发给其他主机，不跟随")
			},
		}}
}

type Usage struct{ Input, Output int }

// errTruncated 译文被 max_tokens 截断。单独成一个哨兵：这种失败**不重试**——
// 同样的块、同样的参数再发一遍，还是同样的长度。
var errTruncated = errors.New("译文被截断")

type msgResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *Client) call(ctx context.Context, text string) (string, Usage, error) {
	// 端点地址归一化：有的网关文档给 https://host，有的给 https://host/v1。
	// 不处理的话后者会拼成 /v1/v1/messages——实测 404，而且是每次都失败、
	// 只在日志里留一行。
	root := strings.TrimSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1")
	// 明文 HTTP 会让令牌在链路上裸奔。这条不给开关。
	if !strings.HasPrefix(strings.ToLower(root), "https://") {
		return "", Usage{}, fmt.Errorf("端点必须是 https://，当前是 %q", c.BaseURL)
	}
	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "max_tokens": maxTokens, "system": system,
		// 显式关掉推理。翻译是纯转换，不需要想；而 GLM 这类端点默认开着推理，
		// 推理块被下面的解析丢掉，token 却全算在输出里——实测一周出/入比中位
		// 5.3 倍（忠实翻译应为 1–1.6），一句话探针里 88% 的输出是推理。更糟的是
		// 推理会把 max_tokens 的额度吃掉：切块修好之后仍出现的截断全是它。
		// 这是 Anthropic 接口的标准字段，换回官方端点也认。
		"thinking": map[string]string{"type": "disabled"},
		"messages": []map[string]string{{"role": "user", "content": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.Token) // Anthropic 接口的标准头
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("user-agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var r msgResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", Usage{}, fmt.Errorf("解析响应: %w（%s）", err, snip(raw))
	}
	var sb strings.Builder
	for _, c := range r.Content {
		// 只要 text 块。推理块（type=thinking）即便带了 text 字段也不能混进译文；
		// 不带 type 的按 text 处理，兼容只回文本的网关。
		if c.Type == "" || c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	// 截断是静默失败里最难发现的一种：文件看着正常，只是后半截没了
	if r.StopReason == "max_tokens" {
		return "", Usage{}, fmt.Errorf("输出被 max_tokens=%d 截断，译文不完整: %w", maxTokens, errTruncated)
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", Usage{}, fmt.Errorf("返回空内容: %s", snip(raw))
	}
	return sb.String(), Usage{r.Usage.Input, r.Usage.Output}, nil
}

// Shape 结构指纹。翻译把表格列数或标题层级改了，页面会当场崩——但不会报错。
type Shape struct{ Tables, Links, Headings, Fences int }

var (
	reTableRow = regexp.MustCompile(`(?m)^\|`)
	reLink     = regexp.MustCompile(`\]\(https?://`)
	reHeading  = regexp.MustCompile(`(?m)^#{1,6} `)
)

func ShapeOf(md string) Shape {
	return Shape{
		Tables:   len(reTableRow.FindAllString(md, -1)),
		Links:    len(reLink.FindAllString(md, -1)),
		Headings: len(reHeading.FindAllString(md, -1)),
		Fences:   strings.Count(md, "```"),
	}
}

// Chunks 按标题切块，切点在标题之前，所以拼回去是无损的。
//
// 逐级变细：二级标题 → 三级标题 → 空行分段 → 逐行 → 按 rune 硬切。
// 单个二级小节本身就超限时必须继续往下切——只按 ## 切的话那一节会原样送出去
// 照样被截断（实测 9.4k 字符的一节就会）。
//
// ⚠️ **最后一级必须保证切得动。** 这里以前是「三种结构切法都不行就 return
// []string{md}」，整篇当一块——那不是降级，是一道悬崖：只要报告里有**一个**
// 超长段落，整篇就原样送出去，然后撞上 max_tokens 被截断。09-22 的 AI 报告
// 正是如此：来源段 49 行连成一个 6555 字符的段落（没有列表标记也没有空行），
// 空行分段那一级切不动它，于是 12k 字符整篇发出去，译文被砍掉后半截。
// 所以兜底改成逐行切，单行仍超限的再按 rune 边界硬切，返回值恒满足
// maxLen(out) <= chunkChars。
func Chunks(md string) []string {
	if len(md) <= chunkChars {
		return []string{md}
	}
	// Go 的 RE2 不支持前瞻，所以不能用 (?=^## ) 这种「在标题前切」的写法
	// ——那是 Python 的 re 才有的。改成按行首前缀找切点，更直白，
	// 也不用为正则引擎的差异操心。
	for _, prefix := range []string{"## ", "### ", ""} {
		parts := splitAtLinePrefix(md, prefix)
		if len(parts) < 2 {
			continue
		}
		out := merge(parts)
		if maxLen(out) <= chunkChars {
			return out
		}
	}
	// 逐行是最后一个还能保住 Markdown 结构的切法（来源段那种一行一条的形状
	// 正好在这一级被救回来）。再不行才按字节硬切，那会切进段落中间，结构指纹
	// 可能因此报差异——但「结构对不上」有告警，「译文少一半」没有。
	return merge(hardSplit(strings.SplitAfter(md, "\n")))
}

// hardSplit 把仍然超限的块按字节上限强行切开。切点退到 rune 边界上，
// 不切坏 UTF-8——中文一个字三字节，从中间断开会变成乱码。
func hardSplit(parts []string) []string {
	var out []string
	for _, p := range parts {
		for len(p) > chunkChars {
			cut := chunkChars
			for cut > 0 && !utf8.RuneStart(p[cut]) {
				cut--
			}
			if cut == 0 {
				// 理论上到不了：单个 rune 最多 4 字节，远小于 chunkChars。
				// 真到了说明上限被调到极小，按字节切总比死循环强。
				cut = chunkChars
			}
			out = append(out, p[:cut])
			p = p[cut:]
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitAtLinePrefix 在以 prefix 开头的行之前切开，保留全部内容（无损）。
// prefix 为空时按空行分段。
func splitAtLinePrefix(s, prefix string) []string {
	lines := strings.SplitAfter(s, "\n")
	var out []string
	var cur strings.Builder
	blank := false
	for _, ln := range lines {
		cut := false
		if prefix != "" {
			cut = strings.HasPrefix(ln, prefix) && cur.Len() > 0
		} else {
			cut = blank && strings.TrimSpace(ln) != "" && cur.Len() > 0
			blank = strings.TrimSpace(ln) == ""
		}
		if cut {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(ln)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func merge(parts []string) []string {
	var out []string
	buf := ""
	for _, p := range parts {
		if buf != "" && len(buf)+len(p) > chunkChars {
			out = append(out, buf)
			buf = p
			continue
		}
		buf += p
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out
}

func maxLen(ss []string) int {
	m := 0
	for _, s := range ss {
		if len(s) > m {
			m = len(s)
		}
	}
	return m
}

type FileResult struct {
	Skipped bool
	Elapsed time.Duration
	Usage   Usage
	Warn    string
}

// File 翻一份报告。幂等：源文件没变就跳过。
func (c *Client) File(ctx context.Context, src string, force bool) (FileResult, error) {
	dst := strings.TrimSuffix(src, ".md") + ".en.md"
	if !force {
		if ds, err := os.Stat(dst); err == nil {
			if ss, err := os.Stat(src); err == nil && !ds.ModTime().Before(ss.ModTime()) {
				return FileResult{Skipped: true}, nil
			}
		}
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return FileResult{}, err
	}
	text := string(raw)
	start := time.Now()
	pieces := Chunks(text)
	parts := make([]string, len(pieces))
	usages := make([]Usage, len(pieces))
	errs := make([]error, len(pieces))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, p := range pieces {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()

			var last error
			for a := 1; a <= retries; a++ {
				seg, usage, err := c.call(ctx, p)
				if err == nil {
					parts[i] = strings.Trim(seg, "\n")
					usages[i] = usage
					return
				}
				last = err
				if errors.Is(err, errTruncated) {
					break // 同样的块再发一遍还是被截断，重试只会把失败拖长三倍
				}
				if a < retries { // 空返回、瞬时 5xx 多半重试一次就好
					select {
					case <-time.After(time.Duration(a*3) * time.Second):
					case <-ctx.Done():
						errs[i] = ctx.Err()
						return
					}
				}
			}
			errs[i] = fmt.Errorf("第 %d/%d 块翻译失败: %w", i+1, len(pieces), last)
		}(i, p)
	}
	wg.Wait()

	var u Usage
	for i, err := range errs {
		if err != nil {
			return FileResult{}, err
		}
		u.Input += usages[i].Input
		u.Output += usages[i].Output
	}
	out := strings.Join(parts, "\n\n") + "\n"

	a, b := ShapeOf(text), ShapeOf(out)
	var warn []string
	// 链接与代码围栏必须一字不差；表格与标题允许 ±1（译文偶尔会合并一个空行）
	if a.Links != b.Links {
		warn = append(warn, fmt.Sprintf("links %d→%d", a.Links, b.Links))
	}
	if a.Fences != b.Fences {
		warn = append(warn, fmt.Sprintf("fences %d→%d", a.Fences, b.Fences))
	}
	if abs(a.Tables-b.Tables) > 1 {
		warn = append(warn, fmt.Sprintf("tables %d→%d", a.Tables, b.Tables))
	}
	if abs(a.Headings-b.Headings) > 1 {
		warn = append(warn, fmt.Sprintf("headings %d→%d", a.Headings, b.Headings))
	}

	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		return FileResult{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return FileResult{}, err
	}
	return FileResult{Elapsed: time.Since(start), Usage: u, Warn: strings.Join(warn, " ")}, nil
}

// Day 翻某一天的全部报告。
func (c *Client) Day(ctx context.Context, root, date string, force bool) error {
	dir := filepath.Join(root, "reports", date)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil // 那天没有报告，不是错误
	}
	var failed int
	var total Usage
	files := 0
	for _, e := range ents {
		n := e.Name()
		// 只翻正式报告：排除英文译文、草稿文件（_draft*.md）和隐藏文件
		if e.IsDir() || !strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".en.md") ||
			strings.Contains(n, "_draft") || strings.HasPrefix(n, ".") {
			continue
		}
		src := filepath.Join(dir, n)
		r, err := c.File(ctx, src, force)
		rel := filepath.Join("reports", date, n)
		switch {
		case err != nil:
			failed++
			fmt.Printf("  ✗ %s  %v\n", rel, err)
		case r.Skipped:
			fmt.Printf("  · %s  已是最新，跳过\n", rel)
		default:
			files++
			total.Input += r.Usage.Input
			total.Output += r.Usage.Output
			w := ""
			if r.Warn != "" {
				w = "  ⚠ 结构对不上: " + r.Warn
			}
			fmt.Printf("  ✓ %s  %.0fs  %.1fk tokens%s\n", rel, r.Elapsed.Seconds(),
				float64(r.Usage.Input+r.Usage.Output)/1000, w)
		}
	}
	if files > 0 {
		// 用量要看得见：按量付费不吃订阅窗口，但它确实在花钱，
		// 而花了多少原本一点痕迹都没有
		fmt.Printf("  ── 本次 %d 篇，入 %.1fk / 出 %.1fk tokens\n",
			files, float64(total.Input)/1000, float64(total.Output)/1000)
	}
	if failed > 0 {
		return fmt.Errorf("%d 篇翻译失败", failed)
	}
	return nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
func snip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
