package markdown

import (
	"fmt"
	"regexp"
	"strings"
)

// escape 是这个包的安全基石：所有来自源文本的内容都必须经过它。
// 报告正文含第三方文本（RSS 正文、仓库描述、热榜标题），站点是公开的——
// 只要有一条路径漏掉转义，一个构造过的标题就能在页面上执行脚本。
func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

var (
	reCode   = regexp.MustCompile("`([^`]+)`")
	reLink   = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	reBold   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic = regexp.MustCompile(`(^|[^*])\*([^*]+)\*`)
	reDel    = regexp.MustCompile(`~~([^~]+)~~`)
)

// inline 处理行内语法。
//
// 顺序有讲究：先把行内代码切出来占位，再处理其余标记——否则 `a*b*c` 里的
// 星号会被当成斜体。链接同理，要在强调之前处理，免得 URL 里的下划线被误吃。
func inline(s string) string {
	var codes []string
	s = reCode.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, reCode.FindStringSubmatch(m)[1])
		return fmt.Sprintf("\x00C%d\x00", len(codes)-1)
	})

	var links []string
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		p := reLink.FindStringSubmatch(m)
		text, href := p[1], p[2]
		if !safeURL(href) {
			// 协议不在白名单就退化成纯文本，不产出链接。
			// javascript: 与 data: 是最典型的两个。
			links = append(links, escape(text))
		} else {
			links = append(links, fmt.Sprintf(
				`<a href="%s" rel="noopener noreferrer">%s</a>`, escape(href), inlineEmph(text)))
		}
		return fmt.Sprintf("\x00L%d\x00", len(links)-1)
	})

	s = escape(s)
	s = inlineEmphEscaped(s)

	for i, c := range codes {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00C%d\x00", i), "<code>"+escape(c)+"</code>")
	}
	for i, l := range links {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00L%d\x00", i), l)
	}
	return s
}

// inlineEmph 用于链接文字：先转义再处理强调。
func inlineEmph(s string) string { return inlineEmphEscaped(escape(s)) }

func inlineEmphEscaped(s string) string {
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reItalic.ReplaceAllString(s, "$1<em>$2</em>")
	s = reDel.ReplaceAllString(s, "<del>$1</del>")
	return s
}

// safeURL 只放行 http/https/mailto 和站内相对路径。
func safeURL(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") ||
		strings.HasPrefix(l, "mailto:") {
		return true
	}
	// 相对路径：不含协议分隔符即可
	return !strings.Contains(l, ":")
}
