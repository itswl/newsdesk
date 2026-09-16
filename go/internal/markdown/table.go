package markdown

import (
	"fmt"
	"regexp"
	"strings"
)

var reDivider = regexp.MustCompile(`^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$`)

func isTableRow(s string) bool { return strings.HasPrefix(s, "|") || strings.Contains(s, " | ") }

func isTableDivider(s string) bool {
	t := strings.TrimSpace(s)
	return t != "" && strings.ContainsAny(t, "-") && reDivider.MatchString(t)
}

// table 渲染表格。
//
// 对齐用 class 而不是 style：报告表格很多（trending 那份 11 列），
// 内联 style 既啰嗦又是历史上的注入面——虽然这里的值是自己生成的，
// 但少一处「属性里塞东西」的形状就少一类将来会出错的地方。
func table(b *strings.Builder, lines []string, i int) int {
	head := splitRow(lines[i])
	aligns := parseAlign(lines[i+1], len(head))
	i += 2

	b.WriteString("<table>\n<thead>\n<tr>")
	for c, cell := range head {
		fmt.Fprintf(b, "<th%s>%s</th>", alignClass(aligns, c), inline(cell))
	}
	b.WriteString("</tr>\n</thead>\n<tbody>\n")

	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || !isTableRow(t) {
			break
		}
		b.WriteString("<tr>")
		for c, cell := range splitRow(lines[i]) {
			fmt.Fprintf(b, "<td%s>%s</td>", alignClass(aligns, c), inline(cell))
		}
		b.WriteString("</tr>\n")
		i++
	}
	b.WriteString("</tbody>\n</table>\n")
	return i
}

func splitRow(s string) []string {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseAlign(divider string, n int) []string {
	cells := splitRow(divider)
	out := make([]string, n)
	for i := 0; i < n; i++ {
		if i >= len(cells) {
			continue
		}
		c := cells[i]
		l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case l && r:
			out[i] = "center"
		case r:
			out[i] = "right"
		case l:
			out[i] = "left"
		}
	}
	return out
}

func alignClass(aligns []string, c int) string {
	if c >= len(aligns) || aligns[c] == "" {
		return ""
	}
	return ` class="a-` + aligns[c] + `"`
}
