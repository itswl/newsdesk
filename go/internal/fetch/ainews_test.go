package fetch

import (
	"strings"
	"testing"
	"time"
)

// Qwen 走 Hugging Face 组织接口。私有仓库与空 id 要剔掉；createdAt 是带毫秒的
// ISO 串，必须解析得出，否则每一条都永远算不进时间窗——而那正是旧 feed 死了
// 一年没人发现的形状。
func TestParseHFModels(t *testing.T) {
	body := []byte(`[
	 {"id":"Qwen/Example-A","createdAt":"2026-09-20T08:46:47.000Z","private":false},
	 {"id":"Qwen/Secret","createdAt":"2026-09-21T00:00:00.000Z","private":true},
	 {"id":"","createdAt":"2026-09-19T00:00:00.000Z"}]`)
	items, err := parseHFModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("应只剩 1 条公开模型，得到 %d", len(items))
	}
	it := items[0]
	if it.Source != "Qwen" || it.Title != "Qwen/Example-A" || it.Link != "https://huggingface.co/Qwen/Example-A" {
		t.Errorf("条目字段不对: %+v", it)
	}
	if _, _, ok := normDate(it.Date); !ok {
		t.Errorf("createdAt %q 解析不出来，会永远进不了时间窗", it.Date)
	}
	for _, bad := range []string{`[]`, `<html>`} {
		if _, err := parseHFModels([]byte(bad)); err == nil {
			t.Errorf("%q 应报错而不是静默返回空", bad)
		}
	}
}

// 2026-10-21 改版后的布局，结构照抄线上页面，文字全是编的。上面是精选卡片（<article>，
// 日期在卡片头部、排在链接前面，模型发布那张的链接指向产品页），下面是表格（每行一个
// <li>，日期在同一行的日期单元格里），导航与页脚的 <li> 没有日期。
const anthropicNewLayout = `<html><body>
<header><nav><ul><li><a class="Nav_link" href="/claude"><h3>Claude</h3></a></li><li><a href="/news">Newsroom</a></li></ul></nav></header>
<div class="FeaturedGrid-module-scss-module__AbC123__grid">
<article class="FeaturedGrid-module-scss-module__AbC123__tile"><header class="AssetItem-module-scss-module__XyZ789__header"><div><button aria-label="Related to Introducing Example Model 9"></button></div><p><a class="TopicLink-module-scss-module__Q1w2E3__link" href="/news/announcements">Announcements</a></p><time>Oct 20, 2026</time></header><figure><img src="x.png"/></figure><a class="FeaturedGrid-module-scss-module__AbC123__tileLink" href="/example-model-9"><h2 class="typography-module-scss-module__T9t9T9__headline">Introducing Example Model 9</h2><p class="typography-module-scss-module__T9t9T9__body">Example Model 9 is a placeholder used only in tests.</p></a></article>
<article class="FeaturedGrid-module-scss-module__AbC123__tile"><header class="AssetItem-module-scss-module__XyZ789__header"><p><a class="TopicLink-module-scss-module__Q1w2E3__link" href="/news/policy">Policy</a></p><time>Oct 6, 2026</time></header><a class="FeaturedGrid-module-scss-module__AbC123__tileLink" href="/news/example-program"><h2 class="typography-module-scss-module__T9t9T9__headline">Expanding the Example Program</h2><p>We are expanding a placeholder program.</p></a></article>
</div>
<ul class="TableView-module-scss-module__R5r5R5__list">
<li class="TableView-module-scss-module__R5r5R5__row"><div><div class="TableView-module-scss-module__R5r5R5__summary"><div class="TableView-module-scss-module__R5r5R5__title"><a class="TableView-module-scss-module__R5r5R5__title" href="/news/example-fund"><span class="TableRowPanningText-module-scss-module__P4p4P4__panningText"><h3 class="font-medium">Investing in an example fund</h3></span></a></div><div class="TableView-module-scss-module__R5r5R5__description"><a class="TableView-module-scss-module__R5r5R5__above" href="/news/example-fund"><span><p>A placeholder description of the example fund.</p></span></a></div><div class="TableView-module-scss-module__R5r5R5__category"><a class="TopicLink-module-scss-module__Q1w2E3__link" href="/news/announcements">Announcements</a></div><div class="TableView-module-scss-module__R5r5R5__date"><time>Oct 18, 2026</time></div></div><div class="TableView-module-scss-module__R5r5R5__details" aria-hidden="true"><div>Oct 18, 2026</div><a class="TableView-module-scss-module__R5r5R5__details" href="/news/example-fund"><h3>Investing in an example fund</h3></a></div></div></li>
<li class="TableView-module-scss-module__R5r5R5__row"><div><div class="TableView-module-scss-module__R5r5R5__title"><a class="TableView-module-scss-module__R5r5R5__title" href="/news/example-program"><h3 class="font-medium">Expanding the Example Program</h3></a></div><div class="TableView-module-scss-module__R5r5R5__date"><time>Oct 6, 2026</time></div></div></li>
</ul>
<footer><ul><li><a href="/legal">Legal</a></li><li><a href="//cdn.example.com/x"><h3>CDN</h3></a></li></ul></footer>
</body></html>`

func TestParseAnthropicNewsCards(t *testing.T) {
	items, err := parseAnthropicNews(anthropicNewLayout)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ link, title, date, summary string }{
		{"https://www.anthropic.com/example-model-9", "Introducing Example Model 9", "Oct 20, 2026", "Example Model 9 is a placeholder used only in tests."},
		{"https://www.anthropic.com/news/example-program", "Expanding the Example Program", "Oct 6, 2026", "We are expanding a placeholder program."},
		{"https://www.anthropic.com/news/example-fund", "Investing in an example fund", "Oct 18, 2026", "A placeholder description of the example fund."},
	}
	if len(items) != len(want) {
		for _, it := range items {
			t.Logf("  %s %q %s", it.Link, it.Title, it.Date)
		}
		t.Fatalf("应解析出 %d 条（导航、栏目名、页脚不算，精选与表格重复的只算一次），得到 %d", len(want), len(items))
	}
	for i, w := range want {
		it := items[i]
		if it.Link != w.link || it.Title != w.title || it.Date != w.date || it.Summary != w.summary {
			t.Errorf("第 %d 条 = %q %q %q %q\n想要      %q %q %q %q", i,
				it.Link, it.Title, it.Date, it.Summary, w.link, w.title, w.date, w.summary)
		}
		if it.Source != "Anthropic" || it.Tier != "一手·厂商公告" {
			t.Errorf("第 %d 条来源标错了：%q %q", i, it.Source, it.Tier)
		}
	}
}

// 日期要真能进时间窗：「Oct 20, 2026」这种写法解析不出来的话，keepFresh 会把它当没日期
// 一律保留，每一条都永远在窗内。
func TestAnthropicNewsDatesFeedTheWindow(t *testing.T) {
	items, err := parseAnthropicNews(anthropicNewLayout)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 10, 19, 0, 0, 0, 0, time.Local)
	kept := keepFresh(items, cutoff, 0)
	if len(kept) != 1 || kept[0].Title != "Introducing Example Model 9" {
		t.Fatalf("窗内应只剩 10-20 那一条，得到 %+v", kept)
	}
}

// 改版前的布局：标题、日期、摘要都在同一个 /news/ 链接里，外面没有 <article>/<li>。
// 卡片切不出来时退回按链接找；class 排在 href 前面也要认。
func TestParseAnthropicNewsOldLayout(t *testing.T) {
	body := `<div class="PublicationList_list">
<a href="/news/old-a" class="PublicationList_item"><span class="PublicationList_title">Old layout A</span><time>Sep 3, 2026</time><p>Summary A</p></a>
<a class="FeaturedGrid_tile" href="/news/old-b"><h4>Old layout B</h4><time>Sep 1, 2026</time></a>
<a href="/news/announcements">Announcements</a></div>`
	items, err := parseAnthropicNews(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Title != "Old layout A" || items[0].Date != "Sep 3, 2026" ||
		items[0].Summary != "Summary A" || items[1].Title != "Old layout B" || items[1].Date != "Sep 1, 2026" {
		t.Fatalf("旧布局解析不对：%+v", items)
	}
}

// 解析得出标题、却一条日期都没有，等于页面结构变了：当失败上报，不能把二十几条旧闻
// 交给 keepFresh 一律保留。2026-10-21 只放宽链接匹配时就是这个形状。
func TestParseAnthropicNewsRejectsUndated(t *testing.T) {
	body := `<a class="x" href="/news/a"><h3>A</h3></a><a class="x" href="/news/b"><h3>B</h3></a>`
	_, err := parseAnthropicNews(body)
	if err == nil || !strings.Contains(err.Error(), "没有一条带日期") {
		t.Fatalf("全都没有日期应报失败，得到 %v", err)
	}
	if _, err := parseAnthropicNews(`<html><body>空壳</body></html>`); err == nil ||
		!strings.Contains(err.Error(), "解析出 0 条") {
		t.Fatalf("什么都没解析出来应报失败，得到 %v", err)
	}
}

// 〔编号〕就是条目在 AIOrder 里的位置：digest 与排版都按它编号，顺序只能有一处。
// 源按 srcRank 排（Anthropic 最前，Feeds 依次，不在 Feeds 里的排最后），源内保持抓取顺序。
func TestAIDigestNumbersInOrder(t *testing.T) {
	doc := AIDoc{Items: []Item{
		{Source: "HN Algolia", Title: "社区帖"},
		{Source: "TechCrunch AI", Title: "媒体一"},
		{Source: "OpenAI", Title: "公告"},
		{Source: "TechCrunch AI", Title: "媒体二"},
		{Source: "Anthropic", Title: "官网"},
	}}
	var got []string
	for _, it := range AIOrder(doc) {
		got = append(got, it.Title)
	}
	if want := "官网 公告 媒体一 媒体二 社区帖"; strings.Join(got, " ") != want {
		t.Errorf("顺序 %q，想要 %q", strings.Join(got, " "), want)
	}
	d := AIDigest(doc, func(id int) string {
		if id == 2 {
			return "今日"
		}
		return ""
	})
	for _, s := range []string{"〔1〕官网\n", "〔2〕公告  【今日】\n", "〔5〕社区帖\n"} {
		if !strings.Contains(d, s) {
			t.Errorf("digest 里缺 %q：\n%s", s, d)
		}
	}
}
