// Package markdown 把报告的 Markdown 渲染成 HTML。
//
// 只支持报告实际用到的语法：标题、粗斜体、行内代码、围栏代码、列表、表格、
// 引用、分隔线、链接。够用就好——支持得越多，能出问题的地方越多。
//
// # 为什么不放行原始 HTML
//
// 报告正文里混着别人写的文本：RSS 正文、仓库描述、热榜标题。站点是公开的。
// 通用 Markdown 库默认把源文本里的 HTML 原样透传，所以要在后面再挂一个清洗库
// 把 <script>、onerror= 之类摘掉——那是补丁，而且清洗库的白名单一旦配错就漏。
//
// 这里反过来：渲染器只输出自己生成的标签，源文本里的 < > & " 一律转义。
// 结果是「不安全的 HTML 根本不会被生成」，而不是「生成了再删掉」。
// 代价是报告里不能写内联 HTML——而报告本来就不该写。
package markdown

import (
	"fmt"
	"regexp"
	"strings"
)

// Render 把 Markdown 转成 HTML 片段。
func Render(src string) string {
	var b strings.Builder
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); {
		i = block(&b, lines, i)
	}
	return b.String()
}

// block 处理一个块级结构，返回下一个未处理的行号。
func block(b *strings.Builder, lines []string, i int) int {
	line := lines[i]
	trimmed := strings.TrimSpace(line)

	switch {
	case trimmed == "":
		return i + 1

	case strings.HasPrefix(trimmed, "```"):
		return fence(b, lines, i)

	case reHR.MatchString(trimmed):
		b.WriteString("<hr>\n")
		return i + 1

	case reHeading.MatchString(trimmed):
		m := reHeading.FindStringSubmatch(trimmed)
		n := len(m[1])
		fmt.Fprintf(b, "<h%d>%s</h%d>\n", n, inline(m[2]), n)
		return i + 1

	case strings.HasPrefix(trimmed, ">"):
		return quote(b, lines, i)

	case isTableRow(trimmed) && i+1 < len(lines) && isTableDivider(lines[i+1]):
		return table(b, lines, i)

	case reBullet.MatchString(line) || reOrdered.MatchString(line):
		return list(b, lines, i)

	default:
		return paragraph(b, lines, i)
	}
}

var (
	reHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reHR      = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})$`)
	reBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reOrdered = regexp.MustCompile(`^(\s*)\d+[.)]\s+(.*)$`)
)

func fence(b *strings.Builder, lines []string, i int) int {
	open := strings.TrimSpace(lines[i])
	marker := open[:3]
	lang := strings.TrimSpace(open[3:])
	var body []string
	i++
	for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), marker) {
		body = append(body, lines[i])
		i++
	}
	if i < len(lines) {
		i++ // 吃掉结束围栏
	}
	cls := ""
	if lang != "" {
		// 语言名也要转义：它来自源文本
		cls = fmt.Sprintf(` class="language-%s"`, escape(lang))
	}
	// 代码块同理：命令、配置、日志原文都不是给人翻译的。
	fmt.Fprintf(b, `<pre><code translate="no"%s>%s</code></pre>`+"\n", cls, escape(strings.Join(body, "\n")))
	return i
}

func quote(b *strings.Builder, lines []string, i int) int {
	var body []string
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, ">") {
			break
		}
		body = append(body, strings.TrimPrefix(strings.TrimPrefix(t, ">"), " "))
		i++
	}
	b.WriteString("<blockquote>\n")
	inner := Render(strings.Join(body, "\n"))
	b.WriteString(inner)
	b.WriteString("</blockquote>\n")
	return i
}

func paragraph(b *strings.Builder, lines []string, i int) int {
	var body []string
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || reHeading.MatchString(t) || strings.HasPrefix(t, "```") ||
			strings.HasPrefix(t, ">") || reHR.MatchString(t) ||
			reBullet.MatchString(lines[i]) || reOrdered.MatchString(lines[i]) ||
			(isTableRow(t) && i+1 < len(lines) && isTableDivider(lines[i+1])) {
			break
		}
		body = append(body, t)
		i++
	}
	if len(body) > 0 {
		fmt.Fprintf(b, "<p>%s</p>\n", inline(strings.Join(body, " ")))
	}
	return i
}
