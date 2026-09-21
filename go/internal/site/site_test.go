package site

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/itswl/newsdesk/internal/i18n"
	"github.com/itswl/newsdesk/internal/task"
)

// fixture 造一个最小仓库：若干天的报告。
func fixture(t *testing.T, days ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range days {
		dir := filepath.Join(root, "reports", d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "ai-news.md"), "# AI 简报 "+d+"\n\n正文一段。\n")
		write(t, filepath.Join(dir, "douban.md"), "# 豆瓣 "+d+"\n\n| 片名 | 评分 |\n|---|---|\n| 甲 | 8.1 |\n")
	}
	return root
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func build(t *testing.T, o Options) string {
	t.Helper()
	if o.Out == "" {
		o.Out = filepath.Join(t.TempDir(), "site")
	}
	if o.Lang == "" {
		o.Lang = i18n.ZhCN
	}
	if err := Build(o); err != nil {
		t.Fatal(err)
	}
	return o.Out
}

func TestBuildProducesCorePages(t *testing.T) {
	root := fixture(t, "2026-09-15", "2026-09-16", "2026-09-17")
	out := build(t, Options{Root: root, SiteURL: "https://news.example.com"})
	for _, f := range []string{"index.html", "archive.html", "status.json",
		"feed.xml", "2026-09-17.html", "2026-09-15.html"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("缺 %s", f)
		}
	}
}

// 全站重建不应把没有变化的历史页标成当前时刻；页脚跟随该页最新报告。
func TestPageGenerationTimeFollowsContent(t *testing.T) {
	root := fixture(t, "2026-09-15", "2026-09-16")
	old := time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)
	newer := time.Date(2026, 9, 16, 11, 0, 0, 0, time.Local)
	for _, name := range []string{"ai-news.md", "douban.md"} {
		p := filepath.Join(root, "reports", "2026-09-15", name)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		p = filepath.Join(root, "reports", "2026-09-16", name)
		if err := os.Chtimes(p, newer, newer); err != nil {
			t.Fatal(err)
		}
	}
	out := build(t, Options{Root: root})
	for _, tc := range []struct {
		page string
		want string
	}{
		{"2026-09-15.html", old.Format("2006-01-02 15:04")},
		{"2026-09-16.html", newer.Format("2006-01-02 15:04")},
	} {
		body := read(t, filepath.Join(out, tc.page))
		if !strings.Contains(body, "本栏内容更新于 "+tc.want) {
			t.Errorf("%s 应使用页面内容更新时间 %s", tc.page, tc.want)
		}
	}
}

func TestEachPaneUsesItsOwnReportTime(t *testing.T) {
	root := fixture(t, "2026-09-18")
	aiAt := time.Date(2026, 9, 18, 6, 40, 0, 0, time.Local)
	doubanAt := time.Date(2026, 9, 18, 11, 40, 0, 0, time.Local)
	ai := filepath.Join(root, "reports", "2026-09-18", "ai-news.md")
	douban := filepath.Join(root, "reports", "2026-09-18", "douban.md")
	if err := os.Chtimes(ai, aiAt, aiAt); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(douban, doubanAt, doubanAt); err != nil {
		t.Fatal(err)
	}
	body := read(t, filepath.Join(build(t, Options{Root: root}), "2026-09-18.html"))
	for _, tc := range []struct {
		pane string
		want string
	}{
		{"p-ai", aiAt.Format("2006-01-02 15:04")},
		{"p-douban", doubanAt.Format("2006-01-02 15:04")},
	} {
		start := strings.Index(body, `id="`+tc.pane+`"`)
		if start < 0 {
			t.Fatalf("缺少栏目 %s", tc.pane)
		}
		end := strings.Index(body[start:], `</section>`)
		if end < 0 {
			t.Fatalf("栏目 %s 未闭合", tc.pane)
		}
		section := body[start : start+end]
		if !strings.Contains(section, "本栏内容更新于 "+tc.want) {
			t.Errorf("%s 应显示自己的更新时间 %s", tc.pane, tc.want)
		}
	}
}

// 没有对外地址就不该生成 feed——feed 里的链接必须是绝对地址，
// 拿相对地址糊弄出来的 feed 在阅读器里每一条都点不开。
func TestNoFeedWithoutSiteURL(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root})
	if _, err := os.Stat(filepath.Join(out, "feed.xml")); err == nil {
		t.Fatal("没配 SITE_URL 时不该生成 feed")
	}
}

// 截断只此一处：日期下拉、上下一天、归档页、feed 全由 days 推导。
func TestDaysWindowTruncatesEverything(t *testing.T) {
	root := fixture(t, "2026-09-13", "2026-09-14", "2026-09-15", "2026-09-16", "2026-09-17")
	out := build(t, Options{Root: root, Days: 2, SiteURL: "https://x.example.com"})

	if _, err := os.Stat(filepath.Join(out, "2026-09-13.html")); err == nil {
		t.Fatal("超窗的日期页不该生成")
	}
	for _, p := range []string{"archive.html", "index.html", "feed.xml"} {
		if s := read(t, filepath.Join(out, p)); strings.Contains(s, "2026-09-13") {
			t.Errorf("%s 里还留着超窗日期的引用", p)
		}
	}
	if s := read(t, filepath.Join(out, "archive.html")); !strings.Contains(s, "2026-09-17") {
		t.Error("窗口内的日期应保留")
	}
}

// 页面要明说只保留多少天，否则访客会以为是站挂了或内容被删了。
func TestWindowedSiteSaysSo(t *testing.T) {
	root := fixture(t, "2026-09-13", "2026-09-16", "2026-09-17")
	out := build(t, Options{Root: root, Days: 2})
	if s := read(t, filepath.Join(out, "index.html")); !strings.Contains(s, "7") &&
		!strings.Contains(s, "2") {
		t.Errorf("窗口提示没出现在页面上")
	}
}

// 英文用 translate 预先翻好的 .en.md；带通配符的模式会连译文一起匹配到，
// 而刚翻完的译文 mtime 最新——不显式排除的话英文会被选进中文站，且不报错。
func TestTranslatedFileNeverLeaksIntoChineseSite(t *testing.T) {
	root := fixture(t, "2026-09-17")
	dir := filepath.Join(root, "reports", "2026-09-17")
	write(t, filepath.Join(dir, "ai-news.en.md"), "# AI Digest\n\nEnglish body.\n")
	// 译文比原文新，正是会被误选的情形
	now := time.Now()
	os.Chtimes(filepath.Join(dir, "ai-news.en.md"), now, now)
	os.Chtimes(filepath.Join(dir, "ai-news.md"), now.Add(-time.Hour), now.Add(-time.Hour))

	zh := read(t, filepath.Join(build(t, Options{Root: root, Lang: i18n.ZhCN}), "2026-09-17.html"))
	if strings.Contains(zh, "English body") {
		t.Fatal("英文译文混进了中文站")
	}
	en := read(t, filepath.Join(build(t, Options{Root: root, Lang: i18n.En}), "2026-09-17.html"))
	if !strings.Contains(en, "English body") {
		t.Fatal("英文站没用上译文")
	}
}

// 繁体由简体现转，不需要独立文件。
func TestTraditionalConvertsFromSimplified(t *testing.T) {
	root := fixture(t, "2026-09-17")
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.md"),
		"# 人工智能简报\n\n这里有软件和内存。\n")
	tw := read(t, filepath.Join(build(t, Options{Root: root, Lang: i18n.ZhTW}), "2026-09-17.html"))
	if !strings.Contains(tw, "軟體") || !strings.Contains(tw, "記憶體") {
		t.Fatalf("繁体没按台湾用词转换")
	}
	if strings.Contains(tw, "软件") {
		t.Fatal("简体残留")
	}
}

// 三语共用一个基址的话三份 feed 的条目 id 完全相同，而 Atom 要求 id 全局唯一：
// 同时订阅两个语言的读者会看到同一个 id 配不同内容，阅读器要么去重、
// 要么每次轮询来回翻。
func TestFeedIDsDifferPerLanguage(t *testing.T) {
	root := fixture(t, "2026-09-17")
	all := []i18n.Lang{i18n.ZhCN, i18n.ZhTW}
	ids := func(l i18n.Lang) []string {
		feed := read(t, filepath.Join(build(t, Options{Root: root, Lang: l,
			AllLangs: all, SiteURL: "https://x.example.com"}), "feed.xml"))
		var out []string
		for _, m := range reID.FindAllStringSubmatch(feed, -1) {
			if strings.Contains(m[1], "#") { // 只看条目 id，跳过 feed 自身的
				out = append(out, m[1])
			}
		}
		return out
	}
	zh, tw := ids(i18n.ZhCN), ids(i18n.ZhTW)
	if len(zh) == 0 || len(zh) != len(tw) {
		t.Fatalf("两个语言的条目数应一致：%d vs %d", len(zh), len(tw))
	}
	for i := range zh {
		if zh[i] == tw[i] {
			t.Fatalf("两个语言的条目 id 撞了：%s", zh[i])
		}
		if strings.Contains(zh[i], "/zh-TW/") {
			t.Fatalf("默认语言的条目 id 不该带语言前缀：%s", zh[i])
		}
		if !strings.Contains(tw[i], "/zh-TW/") {
			t.Fatalf("非默认语言的条目 id 应带语言前缀：%s", tw[i])
		}
	}
}

var reID = regexp.MustCompile(`<id>([^<]*)</id>`)

// 正文用 CDATA 而不是 XML 转义——转义会把每个 < 变成 &lt;，体积近乎翻倍。
func TestFeedUsesCDATA(t *testing.T) {
	root := fixture(t, "2026-09-17")
	feed := read(t, filepath.Join(build(t, Options{Root: root,
		SiteURL: "https://x.example.com"}), "feed.xml"))
	if !strings.Contains(feed, "<![CDATA[") {
		t.Fatal("正文应用 CDATA 包裹")
	}
	if strings.Contains(feed, "&lt;h1&gt;") {
		t.Fatal("正文被 XML 转义了，体积会近乎翻倍")
	}
}

// CDATA 内部只有 ]]> 需要处理，处理不好整个 feed 就是非法 XML。
func TestFeedEscapesCDATATerminator(t *testing.T) {
	root := fixture(t, "2026-09-17")
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.md"),
		"# 标题\n\n有人在正文里写了 `]]>` 这三个字符。\n")
	feed := read(t, filepath.Join(build(t, Options{Root: root,
		SiteURL: "https://x.example.com"}), "feed.xml"))
	inner := feed
	if i := strings.Index(feed, "<![CDATA["); i >= 0 {
		inner = feed[i+9:]
	}
	if j := strings.Index(inner, "]]></"); j >= 0 {
		inner = inner[:j]
	}
	if strings.Contains(inner, "]]>") {
		t.Fatal("CDATA 内部残留 ]]>，整个 feed 会是非法 XML")
	}
}

// 消费者是跑在 UTC 里的 Worker：只给本地 ISO 会被当成 UTC 读，差整整 8 小时。
func TestStatusJSONCarriesEpoch(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root})
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "status.json"))), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["generated_epoch"]; !ok {
		t.Fatal("status.json 缺 generated_epoch")
	}
	gen, _ := doc["generated"].(string)
	if _, err := time.Parse(time.RFC3339, gen); err != nil {
		t.Fatalf("generated 应是带时区的 RFC3339，得到 %q", gen)
	}
}

// 报告里混进的原始 HTML 必须当文本渲染，不能直通。
//
// 分析层读的是外部不可信文本（RSS 正文、仓库描述、热榜标题），而它的产出会
// **公开发布**——沙箱禁网拦不住「写进报告」这条路径。渲染器是安全构造的：
// 任何原始 HTML 都被转义，不存在「先直通再过滤」的清洗环节。
func TestRawHTMLInReportIsEscaped(t *testing.T) {
	root := fixture(t, "2026-09-17")
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.md"),
		"# 标题\n\n<script>alert(1)</script> 与 <img src=x onerror=alert(2)>\n"+
			"[链接](javascript:alert(3))\n")
	page := read(t, filepath.Join(build(t, Options{Root: root}), "2026-09-17.html"))
	body := paneOf(t, page, "ai") // 页面自身也有 <script>，只看报告正文那一段

	// ⚠️ 别用「正文里出现 onerror= 就算漏」这种判据：转义之后的
	// &lt;img src=x onerror=alert(2)&gt; 是纯文本，包含这个子串但完全无害。
	// 要判的是有没有真的生成出标签和属性。
	for _, bad := range []string{"<script", "<img", "<iframe", "<svg", "<form"} {
		if strings.Contains(body, bad) {
			t.Errorf("报告正文里生成了 %q 标签：\n%s", bad, body)
		}
	}
	if strings.Contains(body, `href="javascript:`) {
		t.Errorf("javascript: 链接被放行了：\n%s", body)
	}
	for _, want := range []string{
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"&lt;img src=x onerror=alert(2)&gt;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("原始 HTML 应当以文本形式呈现，没找到 %q：\n%s", want, body)
		}
	}
}

// paneOf 取出某个任务那一段正文。
func paneOf(t *testing.T, page, key string) string {
	t.Helper()
	i := strings.Index(page, `id="p-`+key+`"`)
	if i < 0 {
		t.Fatalf("页面里没有 %s 那一段", key)
	}
	rest := page[i:]
	if j := strings.Index(rest, "</section>"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// 空的 reports/ 应当明确报错而不是产出一个空站——静默的空站会被当成正常发布。
func TestEmptyReportsIsAnError(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "reports"), 0o755)
	if err := Build(Options{Root: root, Out: filepath.Join(root, "site"), Lang: i18n.ZhCN}); err == nil {
		t.Fatal("没有任何报告时应报错")
	}
}

// 只有一个语言时不显示切换器：一个孤零零的按钮只是占地方。
func TestLangSwitcherOnlyWhenMultiple(t *testing.T) {
	root := fixture(t, "2026-09-17")
	one := read(t, filepath.Join(build(t, Options{Root: root,
		AllLangs: []i18n.Lang{i18n.ZhCN}}), "index.html"))
	many := read(t, filepath.Join(build(t, Options{Root: root,
		AllLangs: []i18n.Lang{i18n.ZhCN, i18n.ZhTW, i18n.En}}), "index.html"))
	if strings.Contains(one, "繁體") {
		t.Fatal("只有一个语言时不该出现切换器")
	}
	if !strings.Contains(many, "繁體") || !strings.Contains(many, "English") {
		t.Fatal("多语言时应出现完整切换器")
	}
}

// 英文是后加的，早期那些天没有译文。不按语言筛天数的话会生成一堆只有导航和
// 页脚、正文为空的壳页面——21 KB，从归档点进去什么都没有，也不解释为什么。
func TestLanguageWithoutReportsSkipsThoseDays(t *testing.T) {
	root := fixture(t, "2026-09-15", "2026-09-16", "2026-09-17")
	// 只给最后一天准备英文译文
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.en.md"),
		"# AI Digest\n\nEnglish body.\n")

	en := build(t, Options{Root: root, Lang: i18n.En})
	for _, gone := range []string{"2026-09-15.html", "2026-09-16.html"} {
		if _, err := os.Stat(filepath.Join(en, gone)); err == nil {
			t.Errorf("没有英文内容的 %s 不该生成", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(en, "2026-09-17.html")); err != nil {
		t.Fatal("有译文的那天应当生成")
	}
	// 归档、日期下拉、feed 全由同一个 days 推导，所以也不该链到被跳过的天
	if s := read(t, filepath.Join(en, "archive.html")); strings.Contains(s, "2026-09-15") {
		t.Error("归档里还链着没有英文内容的日子，点进去是 404")
	}

	// 中文不受影响
	zh := build(t, Options{Root: root, Lang: i18n.ZhCN})
	for _, d := range []string{"2026-09-15.html", "2026-09-16.html", "2026-09-17.html"} {
		if _, err := os.Stat(filepath.Join(zh, d)); err != nil {
			t.Errorf("简体站不该少 %s", d)
		}
	}
}

// 一天都没有就该明确报错，而不是产出一个空站——空站会被当成正常发布。
func TestLanguageWithNoReportsAtAllIsAnError(t *testing.T) {
	root := fixture(t, "2026-09-17")
	err := Build(Options{Root: root, Out: filepath.Join(t.TempDir(), "s"), Lang: i18n.En})
	if err == nil {
		t.Fatal("该语言一篇报告都没有时应当报错")
	}
	if !strings.Contains(err.Error(), "en") {
		t.Fatalf("报错要说清是哪个语言：%v", err)
	}
}

// feed 比站点窄有两种原因，处置完全不同：条数上限截短是真不一致（要调上限
// 或收窄窗口），某天没内容是正常的。早先无条件写「被条数上限截短了」，
// 照着错误的原因去调上限，问题一动不动。
func TestFeedCoverageMatchesSite(t *testing.T) {
	root := fixture(t, "2026-09-15", "2026-09-16", "2026-09-17")
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.en.md"), "# D\n\nB.\n")

	var log strings.Builder
	out := filepath.Join(t.TempDir(), "s")
	if err := Build(Options{Root: root, Out: out, Lang: i18n.En,
		SiteURL: "https://x.example.com", Log: &log}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "截短") {
		t.Errorf("缺译文不是条数上限的问题，不该这么报：\n%s", log.String())
	}
	if !strings.Contains(log.String(), "覆盖 1 天 / 站点 1 天") {
		t.Errorf("筛过之后 feed 与站点应当一致：\n%s", log.String())
	}
}

// 页面是静态的、会被边缘缓存——不标生成时刻的话，「这是现在的榜还是几小时前的」
// 只能靠猜。精确到分，必须带时区偏移。
func TestPageCarriesGenerationTime(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root, SiteURL: "https://x.example.com"})
	reTime := regexp.MustCompile(`本栏内容更新于 (\d{4}-\d{2}-\d{2} \d{2}:\d{2}) \(UTC[+-]\d{2}:\d{2}\)`)

	for _, page := range []string{"index.html", "2026-09-17.html"} {
		body := read(t, filepath.Join(out, page))
		// 位置：面板第一行，读者进正文前先看到。以前上面还有一行 reports/ 路径，
		// 那是内部信息，占着正文最显眼的位置，已去掉。
		const open = `aria-labelledby="t-ai">`
		i := strings.Index(body, open)
		if i < 0 || !strings.HasPrefix(body[i+len(open):], `<div class="src gen">`) {
			t.Errorf("%s 的生成时刻没有在面板第一行", page)
		}
		if strings.Contains(body, `<div class="src">reports/`) {
			t.Errorf("%s 仍在输出 reports/ 路径行", page)
		}
		m := reTime.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s 缺生成时刻标注（或没带时区偏移）", page)
			continue
		}
		// 标的必须是真实可解析的时刻，不是占位符
		if _, err := time.Parse("2006-01-02 15:04", m[1]); err != nil {
			t.Errorf("%s 的时间不是合法格式：%s", page, m[1])
		}
	}
	// 归档页没有面板，标注留在页脚
	if a := read(t, filepath.Join(out, "archive.html")); !strings.Contains(a, `<p class="gen">`) {
		t.Error("归档页页脚应保留生成时刻")
	}

	// 三语各自成文，繁体由简体现转。英文要有当天的译文才生成（语言筛选）
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.en.md"), "# Digest\n\nBody.\n")
	en := read(t, filepath.Join(build(t, Options{Root: root, Lang: i18n.En}), "index.html"))
	if !strings.Contains(en, `<div class="src gen">Section content updated at `) {
		t.Error("英文版缺生成时刻标注或位置不对")
	}
	tw := read(t, filepath.Join(build(t, Options{Root: root, Lang: i18n.ZhTW}), "index.html"))
	if !strings.Contains(tw, `<div class="src gen">本欄內容更新於`) {
		t.Error("繁体版缺生成时刻标注或位置不对")
	}
}

// ── 图标与 webapp ──

func decodePNG(t *testing.T, p string) image.Image {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("%s 不是合法 PNG: %v", filepath.Base(p), err)
	}
	return img
}

// 加到主屏用到的那几样。这类东西本地看不出问题：没有报错、页面照常打开，
// 只有真机上主屏多一个丑图标。
func TestWebappAssets(t *testing.T) {
	out := build(t, Options{Root: fixture(t, "2026-09-17"), SiteURL: "https://news.example.com"})
	for _, f := range []string{
		"favicon.ico", "icon.svg", "icon-32.png", "apple-touch-icon.png",
		"icon-192.png", "icon-512.png", "icon-maskable-512.png", "manifest.webmanifest",
	} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("缺 %s", f)
		}
	}

	// 尺寸对不上 iOS 会自己缩放，边缘发虚；四角透明的话会被填成黑块
	for _, tc := range []struct {
		name string
		size int
	}{{"apple-touch-icon.png", 180}, {"icon-32.png", 32}, {"icon-192.png", 192}, {"icon-512.png", 512}} {
		img := decodePNG(t, filepath.Join(out, tc.name))
		if b := img.Bounds(); b.Dx() != tc.size || b.Dy() != tc.size {
			t.Errorf("%s 应为 %d×%d，实际 %d×%d", tc.name, tc.size, tc.size, b.Dx(), b.Dy())
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0xffff {
			t.Errorf("%s 四角是透明的，iOS 会给它垫上黑色", tc.name)
		}
	}

	// 矢量版是给浏览器标签页与桌面书签的，得是能解析的 SVG
	if s := read(t, filepath.Join(out, "icon.svg")); !strings.HasPrefix(s, "<svg ") ||
		!strings.HasSuffix(s, "</svg>") || strings.Contains(s, "<script") {
		t.Error("icon.svg 不像一份自洽的矢量图")
	}
}

// Android 的自适应图标按自己的形状裁切，只保证中心 80% 的圆可见：
// maskable 那份的内容必须缩进去，否则卡片四角会被切掉。
func TestMaskableIconIsInset(t *testing.T) {
	out := build(t, Options{Root: fixture(t, "2026-09-17")})
	// 卡片左边沿：标准份在 0.22、maskable 份在 0.27。取 0.25 一列正好分辨两者。
	white := func(name string) bool {
		img := decodePNG(t, filepath.Join(out, name))
		b := img.Bounds()
		r, g, bl, _ := img.At(b.Dx()/4, b.Dy()/2).RGBA()
		return r > 0xf000 && g > 0xf000 && bl > 0xf000
	}
	if !white("icon-512.png") {
		t.Error("标准份在这一列应该是白色卡片")
	}
	if white("icon-maskable-512.png") {
		t.Error("maskable 份这里仍是卡片，说明内容没有缩进安全区")
	}
}

// 清单里的图标必须与真实产出一一对得上：对不上浏览器静默丢掉那一条，
// 「加到主屏」就退回默认图标或首字母，同样不报错。
func TestManifestMatchesRealFiles(t *testing.T) {
	root := fixture(t, "2026-09-17")
	// 英文站只列有译文的那几天，没有译文就连目录都不生成
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.en.md"), "# Digest\n\nBody.\n")
	out := build(t, Options{Root: root, Lang: i18n.En})
	var m struct {
		Name        string `json:"name"`
		ShortName   string `json:"short_name"`
		Lang        string `json:"lang"`
		StartURL    string `json:"start_url"`
		Scope       string `json:"scope"`
		Display     string `json:"display"`
		ThemeColor  string `json:"theme_color"`
		Description string `json:"description"`
		Icons       []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "manifest.webmanifest"))), &m); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	if m.Display != "standalone" {
		t.Errorf("display 应为 standalone，实际 %q", m.Display)
	}
	// scope 写死 "/"：默认值是清单所在目录，那样 /en/ 装出来的应用点语言切换
	// 会跳出应用到浏览器
	if m.Scope != "/" {
		t.Errorf(`scope 应为 "/"，实际 %q`, m.Scope)
	}
	if m.StartURL != "." {
		t.Errorf(`start_url 应为 "."（相对清单自身解析），实际 %q`, m.StartURL)
	}
	// 清单里的名字跟着语言走，不是三语共用一份
	if m.Name != "Daily Briefing" || m.Lang != "en" {
		t.Errorf("英文站清单的 name/lang 不对：%q / %q", m.Name, m.Lang)
	}
	if m.ThemeColor == "" || m.Description == "" {
		t.Error("清单缺 theme_color 或 description")
	}
	if len(m.Icons) == 0 {
		t.Fatal("清单里没有图标")
	}
	for _, ic := range m.Icons {
		img := decodePNG(t, filepath.Join(out, ic.Src))
		var w int
		if _, err := fmt.Sscanf(ic.Sizes, "%dx", &w); err != nil {
			t.Fatalf("%s 的 sizes 读不出来：%q", ic.Src, ic.Sizes)
		}
		if got := img.Bounds().Dx(); got != w {
			t.Errorf("%s 标称 %s，实际 %dpx", ic.Src, ic.Sizes, got)
		}
		if ic.Type != "image/png" {
			t.Errorf("%s 的类型写成了 %q", ic.Src, ic.Type)
		}
	}
}

// iOS 只认 apple-touch-icon 与那几个 apple- 前缀的 meta，Android 只认 manifest。
// 两套都得在每一页的 head 里——漏一套的话只有一半设备受影响，而且不报错。
func TestHeadCarriesBothWebappFlavours(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root, SiteURL: "https://news.example.com"})
	for _, p := range []string{"index.html", "2026-09-17.html", "archive.html"} {
		body := read(t, filepath.Join(out, p))
		for _, want := range []string{
			`rel="apple-touch-icon" href="apple-touch-icon.png"`,
			`rel="manifest" href="manifest.webmanifest"`,
			`name="apple-mobile-web-app-capable"`,
			`name="apple-mobile-web-app-title"`,
			`name="theme-color"`,
			`rel="icon" href="icon.svg"`,
			// 不写 viewport-fit=cover 的话 CSS 里那些 env(safe-area-inset-*) 恒为 0，
			// 底部给 Home 指示条留白那行等于没写
			`viewport-fit=cover`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 的 head 里缺 %s", p, want)
			}
		}
	}
	// 图标是相对路径引用的，语言子目录下必须也有一份，否则 /en/ 的页面全指到上一级
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.en.md"), "# Digest\n\nBody.\n")
	en := build(t, Options{Root: root, Lang: i18n.En})
	for _, f := range []string{"apple-touch-icon.png", "manifest.webmanifest", "icon.svg"} {
		if _, err := os.Stat(filepath.Join(en, f)); err != nil {
			t.Errorf("语言子目录里缺 %s", f)
		}
	}
}

// 导航条上的三个控件在读屏里得自报家门：日期下拉没有可见 label（占位项只是提示），
// 主题按钮只有字形（title 在触屏上既不显示、也不是可靠的可访问名）。
func TestNavControlsCarryAccessibleNames(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root, SiteURL: "https://news.example.com"})
	// 归档页没有日期下拉（它自己就是全部日期），所以只查栏目页
	for _, p := range []string{"index.html", "2026-09-17.html"} {
		if body := read(t, filepath.Join(out, p)); !strings.Contains(body, `<select id="daysel" aria-label="`) {
			t.Errorf("%s 的日期下拉缺 aria-label", p)
		}
	}
	for _, p := range []string{"index.html", "2026-09-17.html", "archive.html"} {
		body := read(t, filepath.Join(out, p))
		for _, want := range []string{
			`id="theme"`,
			// 可访问名是状态名「深色模式」，aria-pressed 才读得通；动作名留在 title 里做描述
			`title="切换深浅色" aria-label="深色模式" aria-pressed="false"`,
			// 状态由脚本在首屏按已选主题（或系统偏好）改写
			`setAttribute('aria-pressed'`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 里缺 %s", p, want)
			}
		}
	}
}

// 表格数字列要对齐、粘性 tabbar 不能盖住滚进来的元素——两条都在 CSS 里，且都不显眼。
func TestNumericAndScrollStyles(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root, SiteURL: "https://news.example.com"})
	body := read(t, filepath.Join(out, "index.html"))
	for _, want := range []string{
		"font-variant-numeric:tabular-nums",
		"scroll-margin-top:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 的样式里缺 %s", want)
		}
	}
}

// 图标画的是同一份几何量，只是尺寸不同——换个尺寸就画歪的话，多半是缩放算错了。
func TestIconScalesWithSize(t *testing.T) {
	for _, img := range []image.Image{iconImage(64, 1), iconImage(256, 1)} {
		b := img.Bounds()
		// 卡片左内边距那一列：卡片在，三条横线不在（它们从 0.31 才开始）
		r, g, bl, _ := img.At(b.Dx()/4, b.Dy()/2).RGBA()
		if r < 0xf000 || g < 0xf000 || bl < 0xf000 {
			t.Errorf("%dpx 的卡片位置上不是白色：%04x %04x %04x", b.Dx(), r, g, bl)
		}
		// 画布角上是渐变底色，不能被卡片盖住：深蓝意味着蓝通道最高
		r, g, bl, _ = img.At(0, 0).RGBA()
		if !(bl > r && bl > g) {
			t.Errorf("%dpx 的左上角不是深蓝底色：%04x %04x %04x", b.Dx(), r, g, bl)
		}
	}
}

// 栏目 tab 要有 ARIA 语义：读屏靠 role / aria-selected 知道「哪一档是选中的」，
// aria-controls / aria-labelledby 把 tab 与面板对上。引用的 id 必须真的存在——
// 指向不存在 id 的 aria-controls 不报错，只是读屏什么也报不出来。
// 方向键有意不归 tab（留给换天），所以这里不测键盘，只测语义。
func TestTabsCarryARIASemantics(t *testing.T) {
	root := fixture(t, "2026-09-17") // fixture 只有 ai 与 douban 两份报告，另两档缺席
	body := read(t, filepath.Join(build(t, Options{Root: root}), "2026-09-17.html"))

	if !strings.Contains(body, `<div class="tabs" role="tablist" aria-label="`) {
		t.Error("tab 容器缺 role=tablist / aria-label")
	}
	if n := strings.Count(body, `role="tab"`); n != len(task.All) {
		t.Errorf("role=tab 应有 %d 个（含缺席的档），实际 %d", len(task.All), n)
	}
	if n := strings.Count(body, `aria-selected="true"`); n != 1 {
		t.Errorf("选中的 tab 应恰好一个，实际 %d", n)
	}
	// 第一档（ai）默认选中
	if !strings.Contains(body, `<button class="tab on" data-t="ai" id="t-ai" role="tab" aria-selected="true" aria-controls="p-ai">`) {
		t.Error("第一档没有按选中态渲染")
	}
	// 缺席的档也是能点开的 tab：有自己的面板，说明为什么没有
	if !strings.Contains(body, `class="tab absent" data-t="trending" id="t-trending" role="tab" aria-selected="false" aria-controls="p-trending" title="`) {
		t.Error("缺席的档没有按可点开的 tab 渲染")
	}
	if strings.Contains(body, `aria-disabled`) {
		t.Error("缺席的档不该再是禁用态")
	}
	// 面板与 tab 互指，且引用的 id 真的存在
	for _, m := range regexp.MustCompile(`aria-(?:controls|labelledby)="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		if !strings.Contains(body, `id="`+m[1]+`"`) {
			t.Errorf("%s 指向不存在的 id", m[0])
		}
	}
	if n := strings.Count(body, `role="tabpanel"`); n != len(task.All) {
		t.Errorf("每档一个面板（缺席的档放说明），应有 %d 个，实际 %d", len(task.All), n)
	}
	if n := strings.Count(body, `<p class="none">`); n != len(task.All)-2 {
		t.Errorf("缺席的两档应各有一段说明，实际 %d 段", n)
	}
	if !strings.Contains(body, `id="p-douban" role="tabpanel" aria-labelledby="t-douban"`) {
		t.Error("面板没有 aria-labelledby 指回 tab")
	}
	// 选中态由脚本在点击与深链时改写
	if !strings.Contains(body, `setAttribute('aria-selected'`) {
		t.Error("site.js 没有维护 aria-selected")
	}
}

// tab 同时输出全称与短名，CSS 按屏宽切换；短名沿用 short.* 那套，与旧状态条一致。
func TestTabsCarryShortLabels(t *testing.T) {
	root := fixture(t, "2026-09-17")
	body := read(t, filepath.Join(build(t, Options{Root: root}), "2026-09-17.html"))
	if !strings.Contains(body, `<b class="l">AI 简报</b><i class="s">AI</i>`) {
		t.Error("tab 缺全称 + 短名")
	}
}

// 今天这一页的 tab 带状态：失败红、还没到点灰点并写明计划时刻；缺报告的档能点开，
// 面板里是同一句说明。以前这些话只在悬浮提示里，触屏上看不到。
func TestTabsCarryTodayStatus(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	root := t.TempDir()
	dir := filepath.Join(root, "reports", today)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "ai-news.md"), "# AI\n\n正文。\n")
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "state", "schedule.json"),
		`{"douban":{"date":"`+today+`","status":"failed","attempts":3}}`)
	body := read(t, filepath.Join(build(t, Options{Root: root}), today+".html"))

	for _, want := range []string{
		`class="tab absent bad" data-t="douban"`, `<i class="m">✘</i>`,
		`<p class="none">今天失败了，重试 3 次后放弃`,
		`class="tab absent pend" data-t="trending"`, `<i class="m">·</i>`,
		`<p class="none">今天还没跑，计划 16:40</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s", want)
		}
	}
	// 正常跑完的不给记号，四个 tab 全带勾是噪声
	i := strings.Index(body, `data-t="ai"`)
	if seg := body[i : i+strings.Index(body[i:], "</button>")]; strings.Contains(seg, `class="m"`) {
		t.Error("跑完的档不该带状态记号")
	}
	// 独立的状态条不再输出
	if strings.Contains(body, `class="status`) || strings.Contains(body, `class="st `) {
		t.Error("状态条应已并进 tab")
	}
}

// 页头两行：标题 + 语言/主题在第一行，日期导航独占第二行；键盘用户有「跳到正文」，落点是 main。
func TestHeaderLayoutAndSkipLink(t *testing.T) {
	root := fixture(t, "2026-09-17")
	out := build(t, Options{Root: root})
	for _, p := range []string{"index.html", "2026-09-17.html", "archive.html"} {
		body := read(t, filepath.Join(out, p))
		if b := strings.Index(body, "<body>"); b < 0 || !strings.HasPrefix(body[b+6:], `<a class="skip" href="#main">`) {
			t.Errorf("%s 的 body 第一个元素应是跳到正文", p)
		}
		for _, want := range []string{`<main id="main" tabindex="-1">`, `<div class="tools">`, `<button class="tg" id="theme"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 缺 %s", p, want)
			}
		}
		nav, tools := strings.Index(body, `<div class="nav">`), strings.Index(body, `<div class="tools">`)
		if nav < 0 || tools < nav {
			t.Errorf("%s 的 tools 应紧跟在 nav 之后", p)
			continue
		}
		if strings.Contains(body[nav:tools], `class="tg"`) {
			t.Errorf("%s 的主题按钮仍在日期导航里", p)
		}
	}
}

// 归档页每行的四个栏目标签要带 chip 基类：样式表写的是 .chip.has，光有修饰类什么都
// 不匹配，四个标签就成了挤在一起的一串灰字（实测漏过）。
func TestArchiveChipsCarryBaseClass(t *testing.T) {
	root := fixture(t, "2026-09-17")
	a := read(t, filepath.Join(build(t, Options{Root: root}), "archive.html"))
	for _, want := range []string{`<span class="chip has">AI</span>`, `<span class="chip no">Trending</span>`} {
		if !strings.Contains(a, want) {
			t.Errorf("归档页缺 %s", want)
		}
	}
	if strings.Contains(a, `<span class="has">`) {
		t.Error("归档页仍在输出没有 chip 基类的标签")
	}
	if !strings.Contains(a, `</footer></main>`) {
		t.Error("归档页的 footer 没有闭合")
	}
}

// 分享卡片：description 总有；og 标签要绝对地址，配了 SITE_URL 才出，图用根目录的 512 图标。
func TestHeadCarriesShareMetadata(t *testing.T) {
	root := fixture(t, "2026-09-17")
	with := read(t, filepath.Join(build(t, Options{Root: root, SiteURL: "https://news.example.com"}), "2026-09-17.html"))
	for _, want := range []string{
		`<meta name="description" content="`,
		`<meta property="og:title" content="每日简报 2026-09-17">`,
		`<meta property="og:url" content="https://news.example.com/2026-09-17.html">`,
		`<meta property="og:image" content="https://news.example.com/icon-512.png">`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("缺 %s", want)
		}
	}
	without := read(t, filepath.Join(build(t, Options{Root: root}), "2026-09-17.html"))
	if !strings.Contains(without, `<meta name="description"`) || strings.Contains(without, `og:image`) {
		t.Error("没配 SITE_URL 时应只有 description，没有 og 标签")
	}
	// 英文站的 og:url 跟着语言子目录走，og:image 仍指根目录那份
	write(t, filepath.Join(root, "reports", "2026-09-17", "ai-news.en.md"), "# Digest\n\nBody.\n")
	// 单语构建时根语言就是英文、不带 /en；要有别的语言在前面它才是子目录
	en := read(t, filepath.Join(build(t, Options{Root: root, Lang: i18n.En, AllLangs: []i18n.Lang{i18n.ZhCN, i18n.En},
		SiteURL: "https://news.example.com"}), "2026-09-17.html"))
	for _, want := range []string{
		`<meta property="og:url" content="https://news.example.com/en/2026-09-17.html">`,
		`<meta property="og:image" content="https://news.example.com/icon-512.png">`,
	} {
		if !strings.Contains(en, want) {
			t.Errorf("英文站缺 %s", want)
		}
	}
}
