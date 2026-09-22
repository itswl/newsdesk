package markdown

import (
	"regexp"
	"strings"
	"testing"
)

// 这个包不挂清洗库，安全性完全来自「只输出自己生成的标签」。
// 所以检查的不是「输出里有没有 onerror 这串字」——转义之后它只是文本，
// 而是「输出里的每一个真实标签，是不是渲染器自己会生成的那几个」。
// 按字符串查会误报：&lt;img src=x onerror=1&gt; 里确实有 onerror= 三个字，
// 但它已经不是标记了。

var allowedTags = map[string]bool{
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"p": true, "hr": true, "blockquote": true, "ul": true, "ol": true, "li": true,
	"strong": true, "em": true, "del": true, "code": true, "pre": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true,
	"a": true,
}

// 渲染器只会产出这四个属性：链接的 href/rel、表格对齐的 class、代码的 translate
var allowedAttrs = map[string]bool{"href": true, "rel": true, "class": true, "translate": true}

var reTag = regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)((?:\s[^<>]*)?)/?>`)
var reAttr = regexp.MustCompile(`([a-zA-Z-]+)\s*=`)

// assertOnlyOwnMarkup 检查输出里没有渲染器不会产出的标签或属性。
func assertOnlyOwnMarkup(t *testing.T, src, out string) {
	t.Helper()
	for _, m := range reTag.FindAllStringSubmatch(out, -1) {
		tag, attrs := strings.ToLower(m[2]), m[3]
		if !allowedTags[tag] {
			t.Errorf("产出了不该有的标签 <%s>\n  源: %q\n  出: %q", tag, src, out)
		}
		for _, a := range reAttr.FindAllStringSubmatch(attrs, -1) {
			if !allowedAttrs[strings.ToLower(a[1])] {
				t.Errorf("产出了不该有的属性 %s=\n  源: %q\n  出: %q", a[1], src, out)
			}
		}
	}
	// 未转义的裸 < 只可能来自渲染器自己；源文本里的必须是 &lt;
	if strings.Contains(out, "<script") || strings.Contains(out, "<img") {
		t.Errorf("出现了原始 HTML\n  源: %q\n  出: %q", src, out)
	}
}

func TestNoRawHTMLEverEscapes(t *testing.T) {
	cases := []string{
		`<script>alert(1)</script>`,
		`- HN 标题：<img src=x onerror="alert(document.domain)"> 发布`,
		`<svg onload=alert(1)>`,
		`<iframe src="//evil.tld"></iframe>`,
		`<p onclick="steal()">正常文字</p>`,
		`<form action="//evil.tld"><input name=pw type=password>`,
		`<ImG   SrC=x  OnErRoR=alert(1) >`,
		`| a |` + "\n" + `|---|` + "\n" + `| <img src=x onerror=1> |`,
		"# <script>x</script>",
		"> <script>x</script>",
		"```\n<script>x</script>\n```",
		"`<script>x</script>`",
		"**<script>x</script>**",
	}
	for _, src := range cases {
		assertOnlyOwnMarkup(t, src, Render(src))
	}
}

func TestDangerousURLSchemes(t *testing.T) {
	for _, src := range []string{
		`[点我](javascript:alert(1))`,
		`[点我](JavaScript:alert(1))`,
		`[点我](data:text/html;base64,PHNjcmlwdD4=)`,
		`[点我](vbscript:msgbox)`,
	} {
		out := Render(src)
		if strings.Contains(out, "<a ") {
			t.Errorf("危险协议不该产出链接: %q -> %q", src, out)
		}
		assertOnlyOwnMarkup(t, src, out)
	}
}

func TestSafeURLsBecomeLinks(t *testing.T) {
	for _, src := range []string{
		`[x](https://example.com/a?b=1&c=2)`,
		`[x](http://example.com)`,
		`[x](mailto:a@b.com)`,
		`[x](2026-09-16.html)`,
	} {
		out := Render(src)
		if !strings.Contains(out, "<a href=") {
			t.Errorf("安全链接应产出 <a>: %q -> %q", src, out)
		}
		if !strings.Contains(out, `rel="noopener noreferrer"`) {
			t.Errorf("外链应带 rel: %q", out)
		}
	}
	// & 必须转义成 &amp;，否则 HTML 不合法
	if out := Render(`[x](https://e.com/a?b=1&c=2)`); !strings.Contains(out, "&amp;") {
		t.Errorf("URL 里的 & 未转义: %q", out)
	}
}

func TestBlocks(t *testing.T) {
	cases := map[string]string{
		"# 标题":          "<h1>标题</h1>",
		"### 三级":        "<h3>三级</h3>",
		"---":           "<hr>",
		"普通段落":          "<p>普通段落</p>",
		"> 引用":          "<blockquote>",
		"- 一\n- 二":      "<ul>",
		"1. 一\n2. 二":    "<ol>",
		"**粗**":         "<strong>粗</strong>",
		"*斜*":           "<em>斜</em>",
		"~~删~~":         "<del>删</del>",
		"`码`":           `<code translate="no">码</code>`,
		"```\nx\n```":   `<pre><code translate="no">`,
		"```go\nx\n```": `class="language-go"`,
	}
	for src, want := range cases {
		if out := Render(src); !strings.Contains(out, want) {
			t.Errorf("Render(%q) 应含 %q，得到 %q", src, want, out)
		}
	}
}

func TestTable(t *testing.T) {
	out := Render("| 左 | 中 | 右 |\n|:---|:-:|---:|\n| 1 | 2 | 3 |\n")
	for _, want := range []string{"<table>", "<thead>", "<th class=\"a-left\">左</th>",
		"<th class=\"a-center\">中</th>", "<th class=\"a-right\">右</th>",
		"<tbody>", "<td class=\"a-left\">1</td>", "</table>"} {
		if !strings.Contains(out, want) {
			t.Errorf("表格缺 %q:\n%s", want, out)
		}
	}
	// 对齐用 class 不用内联 style——少一处「往属性里塞东西」的形状
	if strings.Contains(out, "style=") {
		t.Errorf("不该产出内联 style: %s", out)
	}
}

func TestInlineOrdering(t *testing.T) {
	// 行内代码要先切出来，否则里面的星号会被当成强调
	if out := Render("`a*b*c`"); strings.Contains(out, "<em>") {
		t.Errorf("代码内的星号被当成强调: %q", out)
	}
	// 链接里的强调应生效
	if out := Render(`[**粗链接**](https://e.com)`); !strings.Contains(out, "<strong>") {
		t.Errorf("链接文字里的强调未生效: %q", out)
	}
}

func TestNestedList(t *testing.T) {
	out := Render("- 一\n  - 一甲\n  - 一乙\n- 二\n")
	if strings.Count(out, "<ul>") != 2 || strings.Count(out, "</ul>") != 2 {
		t.Errorf("嵌套列表应有两层 ul:\n%s", out)
	}
}

func TestEscapeIsTotal(t *testing.T) {
	// 五个字符都必须转义，缺一个就有绕过的余地
	out := escape(`&<>"'`)
	for _, want := range []string{"&amp;", "&lt;", "&gt;", "&quot;", "&#39;"} {
		if !strings.Contains(out, want) {
			t.Errorf("escape 漏了 %s: %q", want, out)
		}
	}
}

// 裸 URL 自动成链：模型不按契约写 [标题](URL) 时兜底（09-22 的来源就是裸地址）。
// 尾部的标点与粘连的中文算文章的，不算地址的，且要归还原文不能吞掉；
// & 在 href 里正确转义；已有的链接语法不能被二次包一层。
func TestBareURLsAreLinked(t *testing.T) {
	p := func(inner string) string { return "<p>" + inner + "</p>\n" }
	cases := map[string]string{
		"看 https://example.com/a。好":    p(`看 <a href="https://example.com/a" rel="noopener noreferrer">https://example.com/a</a>。好`),
		"（https://example.com/x）":      p(`（<a href="https://example.com/x" rel="noopener noreferrer">https://example.com/x</a>）`),
		"<https://example.com>":        p(`&lt;<a href="https://example.com" rel="noopener noreferrer">https://example.com</a>&gt;`),
		"https://example.com/?a=1&b=2": p(`<a href="https://example.com/?a=1&amp;b=2" rel="noopener noreferrer">https://example.com/?a=1&amp;b=2</a>`),
		"[标题](https://example.com)":    p(`<a href="https://example.com" rel="noopener noreferrer">标题</a>`),
		"javascript:alert(1)":          p("javascript:alert(1)"),
		// 连续行没有列表标记会被并成一段，段里每个裸地址要各自成链
		"[1] A — t：https://a.com/1\n[2] B — t：https://b.com/2": p(`[1] A — t：<a href="https://a.com/1" rel="noopener noreferrer">https://a.com/1</a>` + " " + `[2] B — t：<a href="https://b.com/2" rel="noopener noreferrer">https://b.com/2</a>`),
	}
	for src, want := range cases {
		if got := Render(src); got != want {
			t.Errorf("%q →\n%s\n想要:\n%s", src, got, want)
		}
	}
	// 混排：链接与裸地址相邻，各自成链互不吞并
	got := Render("[A](https://a.com) 然后 https://b.com/x。")
	want := p(`<a href="https://a.com" rel="noopener noreferrer">A</a> 然后 <a href="https://b.com/x" rel="noopener noreferrer">https://b.com/x</a>。`)
	if got != want {
		t.Errorf("混排 →\n%s\n想要:\n%s", got, want)
	}
}
