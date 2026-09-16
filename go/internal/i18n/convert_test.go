package i18n

import "testing"

// s2twp 值钱的是词汇转换而不是逐字换形。只换字的话「软件」会变成「軟件」
// 而不是「軟體」——那样繁体版就没有存在意义了。
func TestTaiwanVocabularyNotJustGlyphs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"软件", "軟體"}, {"硬件", "硬體"}, {"网络", "網路"},
		{"内存", "記憶體"}, {"鼠标", "滑鼠"}, {"缓存", "快取"},
		{"数据库", "資料庫"}, {"打印机", "印表機"}, {"程序员", "程式設計師"},
		{"人工智能", "人工智慧"}, {"服务器", "伺服器"}, {"默认", "預設"},
		{"视频", "影片"}, {"优化", "最佳化"},
	} {
		if got := ToTraditional(c.in); got != c.want {
			t.Errorf("ToTraditional(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}
}

// 词组表要能消歧。只有字符表的话「皇后」会变成「皇後」。
func TestPhraseDisambiguation(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"皇后", "皇后"}, {"以后", "以後"},
		{"干活", "幹活"}, {"干燥", "乾燥"},
		{"头发", "頭髮"}, {"发现", "發現"},
		{"里面", "裡面"}, {"公里", "公里"},
	} {
		if got := ToTraditional(c.in); got != c.want {
			t.Errorf("ToTraditional(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}
}

func TestLeavesNonChineseAlone(t *testing.T) {
	for _, s := range []string{
		"https://github.com/foo/bar",
		"| a | b |",
		"`npm ci --no-audit`",
		"OpenAI & Anthropic compatible",
		"",
	} {
		if got := ToTraditional(s); got != s {
			t.Errorf("不该改动 %q，得到 %q", s, got)
		}
	}
}

func TestPreservesMarkdownStructure(t *testing.T) {
	src := "| 软件 | 链接 |\n|---|---|\n| `code` | [x](https://a.com/数据) |\n"
	out := ToTraditional(src)
	for _, ch := range []string{"|", "`", "[", "]", "(", ")"} {
		if a, b := count(src, ch), count(out, ch); a != b {
			t.Errorf("结构符号 %q 数量变了: %d -> %d", ch, a, b)
		}
	}
	if !contains(out, "https://a.com/") {
		t.Errorf("URL 被改动了: %q", out)
	}
	if !contains(out, "軟體") {
		t.Errorf("词汇未转换: %q", out)
	}
}

func count(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
func contains(s, sub string) bool { return count(s, sub) > 0 }

// 漏翻英文不会报错，只会在页面上留一句中文，所以靠用例钉住。
func TestEveryKeyHasEnglish(t *testing.T) {
	han := func(s string) bool {
		for _, r := range s {
			if r >= 0x4e00 && r <= 0x9fff {
				return true
			}
		}
		return false
	}
	for _, k := range Keys() {
		en := T(En, k)
		if en == "" {
			t.Errorf("%s 缺英文", k)
		}
		// 平台名保留原文是有意的（Douban (豆瓣)），但这里的表里没有那种情况
		if han(en) {
			t.Errorf("%s 的英文里还有汉字: %q", k, en)
		}
	}
}

// 占位符对不上会在运行时 panic，而且只在某一种语言下触发。
func TestPlaceholdersMatchAcrossLanguages(t *testing.T) {
	countVerbs := func(s string) int {
		n := 0
		for i := 0; i+1 < len(s); i++ {
			if s[i] == '%' {
				if s[i+1] == '%' {
					i++
					continue
				}
				n++
			}
		}
		return n
	}
	for _, k := range Keys() {
		zh, en, tw := T(ZhCN, k), T(En, k), T(ZhTW, k)
		if a, b := countVerbs(zh), countVerbs(en); a != b {
			t.Errorf("%s 占位符数不一致: 简 %d, 英 %d", k, a, b)
		}
		if a, b := countVerbs(zh), countVerbs(tw); a != b {
			t.Errorf("%s 繁体转换把占位符改坏了: %d -> %d", k, a, b)
		}
	}
}

func TestTraditionalIsActuallyDifferent(t *testing.T) {
	if T(ZhTW, "site.name") != "每日簡報" {
		t.Errorf("繁体未转换: %q", T(ZhTW, "site.name"))
	}
	if T(ZhTW, "panel.momoyu") == T(ZhCN, "panel.momoyu") {
		t.Error("繁体与简体相同，转换没生效")
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("未知 key 应 panic——静默退回会让漏翻一直没人发现")
		}
	}()
	T(ZhCN, "no.such.key")
}
