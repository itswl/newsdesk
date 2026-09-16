package i18n

import (
	"bufio"
	_ "embed"
	"strings"
	"sync"
)

// 繁体转换词表。内嵌而不是依赖库——这是一张表，不是一个算法。
// 保住 s2twp 真正值钱的部分：**词汇**转换。只换字的话「软件」会变成「軟件」
// 而不是「軟體」，繁体版就失去意义了。来源见 data/SOURCE.md。
var (
	//go:embed data/STPhrases.txt
	stPhrases string
	//go:embed data/STCharacters.txt
	stChars string
	//go:embed data/TWPhrases.txt
	twPhrases string
	//go:embed data/TWVariants.txt
	twVariants string
)

// stage 是转换链上的一段：一张「最长匹配优先」的替换表。
type stage struct {
	m      map[string]string
	maxLen int // 以 rune 计
}

func newStage(sources ...string) *stage {
	s := &stage{m: make(map[string]string, 60000)}
	for _, src := range sources {
		sc := bufio.NewScanner(strings.NewReader(src))
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			k, v, ok := strings.Cut(line, "\t")
			if !ok || k == "" {
				continue
			}
			// 多个候选时取第一个，和 OpenCC 一致
			if i := strings.IndexByte(v, ' '); i >= 0 {
				v = v[:i]
			}
			if v == "" {
				continue
			}
			// 先出现的优先：词组表排在字符表前面，所以词组不会被单字覆盖
			if _, dup := s.m[k]; dup {
				continue
			}
			s.m[k] = v
			if n := len([]rune(k)); n > s.maxLen {
				s.maxLen = n
			}
		}
	}
	return s
}

// convert 最长匹配向前扫描。
func (s *stage) convert(in string) string {
	rs := []rune(in)
	var b strings.Builder
	b.Grow(len(in))
	for i := 0; i < len(rs); {
		n := s.maxLen
		if rem := len(rs) - i; n > rem {
			n = rem
		}
		matched := false
		for ; n > 0; n-- {
			if v, ok := s.m[string(rs[i:i+n])]; ok {
				b.WriteString(v)
				i += n
				matched = true
				break
			}
		}
		if !matched {
			b.WriteRune(rs[i])
			i++
		}
	}
	return b.String()
}

var (
	chainOnce sync.Once
	chain     []*stage
)

// ToTraditional 简体 → 台湾正体。等价于 OpenCC 的 s2twp。
//
// 表 1 MB 左右，解析一次约几十毫秒，所以懒加载：只发简体的站点根本不会碰到它。
func ToTraditional(s string) string {
	if s == "" {
		return s
	}
	chainOnce.Do(func() {
		chain = []*stage{
			newStage(stPhrases, stChars), // 简 → 繁，词组优先
			newStage(twPhrases),          // 台湾用词
			newStage(twVariants),         // 台湾字形
		}
	})
	for _, st := range chain {
		s = st.convert(s)
	}
	return s
}
