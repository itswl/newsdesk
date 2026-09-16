package markdown

import (
	"fmt"
	"strings"
)

// list 处理无序/有序列表，支持按缩进嵌套。
func list(b *strings.Builder, lines []string, i int) int {
	ordered := reOrdered.MatchString(lines[i])
	baseIndent := indentOf(lines[i])
	tag := "ul"
	if ordered {
		tag = "ol"
	}
	fmt.Fprintf(b, "<%s>\n", tag)

	for i < len(lines) {
		line := lines[i]
		m := reBullet.FindStringSubmatch(line)
		if m == nil {
			m = reOrdered.FindStringSubmatch(line)
		}
		if m == nil {
			// 空行后面如果还是同级列表项，说明只是松散列表，继续
			if strings.TrimSpace(line) == "" && i+1 < len(lines) &&
				isItem(lines[i+1]) && indentOf(lines[i+1]) >= baseIndent {
				i++
				continue
			}
			break
		}
		ind := len(m[1])
		if ind < baseIndent {
			break
		}
		if ind > baseIndent {
			// 更深的缩进交给递归，它会自己吃掉这一段
			i = list(b, lines, i)
			continue
		}
		// 同一项可能跨多行（续行缩进更深但不是列表项）
		text := []string{m[2]}
		i++
		for i < len(lines) && !isItem(lines[i]) && strings.TrimSpace(lines[i]) != "" &&
			indentOf(lines[i]) > baseIndent {
			text = append(text, strings.TrimSpace(lines[i]))
			i++
		}
		fmt.Fprintf(b, "<li>%s</li>\n", inline(strings.Join(text, " ")))
	}
	fmt.Fprintf(b, "</%s>\n", tag)
	return i
}

func isItem(s string) bool { return reBullet.MatchString(s) || reOrdered.MatchString(s) }

func indentOf(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}
