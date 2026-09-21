package site

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/i18n"
	"github.com/itswl/newsdesk/internal/task"
)

//go:embed assets/site.css
var css string

//go:embed assets/site.js
var js string

// 主题预设脚本放在 <head>：晚一步会闪一下白底。
const headJS = `<script>try{var t=localStorage.getItem("theme");` +
	`if(t)document.documentElement.dataset.theme=t}catch(e){}</script>`

const recentDays = 14 // 下拉里直接列出的天数，其余走归档页

// 内容更新时间标注。精确到分，带时区偏移——页面是静态的、会被边缘缓存，读者
// 看到的内容可能比现在旧得多；不带偏移量的 10:41 也无法判断是哪个时区的。
//
// 栏目页跟在 reports/ 路径行下面，归档页没有路径行，放页脚。
func (b *Builder) genTimeText() string {
	return b.T("gen.at", b.genAt.Format("2006-01-02 15:04 (UTC-07:00)"))
}

func (b *Builder) genSectionTimeText(at time.Time) string {
	return b.T("gen.section_at", at.Format("2006-01-02 15:04 (UTC-07:00)"))
}

func (b *Builder) genSrcNote(at time.Time) string {
	return fmt.Sprintf(`<div class="src gen">%s</div>`, esc(b.genSectionTimeText(at)))
}

func (b *Builder) genNote() string {
	return fmt.Sprintf(`<p class="gen">%s</p>`, esc(b.genTimeText()))
}

func (b *Builder) disclaimer() string {
	s := b.T("dis.body")
	if b.o.Contact != "" {
		return s + b.T("dis.contact", esc(b.o.Contact), esc(b.o.Contact))
	}
	return s + b.T("dis.contact_none")
}

func (b *Builder) keepNote() string {
	if !b.windowed {
		return ""
	}
	return b.T("keep.note", len(b.days))
}

// headIcons 图标与「加到主屏」相关的元信息。
//
// 分成两套是有原因的，别删其中一套：
//
//	iOS      只认 apple-touch-icon、apple-mobile-web-app-* 那几个 meta，
//	         没有 apple-touch-icon 就拿页面截图当图标——「很丑」就从这来
//	Android  只认 manifest，上面那些 apple- 前缀的东西它全部忽略
//
// 路径都是相对的：同一份 HTML 在根目录和 /<lang>/ 下都成立，所以每个语言目录
// 里都有一份图标（见 writeAssets）。
func (b *Builder) headIcons() string {
	const (
		favicon = `<link rel="icon" href="favicon.ico" sizes="32x32">` +
			`<link rel="icon" href="icon.svg" type="image/svg+xml">` +
			`<link rel="apple-touch-icon" href="apple-touch-icon.png">` +
			`<link rel="manifest" href="manifest.webmanifest">`
		// 状态栏／地址栏取色，按系统偏好二选一；手动切换主题时由 JS 改写
		// （media 只认系统偏好，选过之后页面和状态栏会反过来）。
		themeColor = `<meta name="theme-color" content="` + themeBgDark + `" media="(prefers-color-scheme: dark)">` +
			`<meta name="theme-color" content="` + themeBgLight + `" media="(prefers-color-scheme: light)">`
		// 以独立窗口打开，不带浏览器地址栏。iOS 认 apple- 那个，Android 走清单。
		standalone = `<meta name="apple-mobile-web-app-capable" content="yes">` +
			`<meta name="mobile-web-app-capable" content="yes">`
		// default 而不是 black-translucent：后者让页面顶到状态栏下面，状态栏文字
		// 恒为白色，浅色主题下白字压在浅色页头上直接看不见。代价是不铺到状态栏
		// 底下，换来两套主题都不会出现读不出的状态栏。
		statusBar = `<meta name="apple-mobile-web-app-status-bar-style" content="default">`
		// 报告里成串的数字会被 iOS 认成电话号码，变成蓝色链接
		noPhone = `<meta name="format-detection" content="telephone=no">`
	)
	return favicon + themeColor + standalone + statusBar + noPhone +
		fmt.Sprintf(`<meta name="apple-mobile-web-app-title" content="%s">`, esc(b.T("app.short")))
}

// viewport 里的 viewport-fit=cover 是必需的：不写的话整页不铺到安全区，
// site.css 里那些 env(safe-area-inset-*) 全是 0——那几行等于没写。
const viewport = `<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">`

// headLinks feed 与语言互链。hreflang 声明三语互为同一内容的不同版本，
// 否则三个站会被搜索引擎当成互相抄袭的重复内容。
func (b *Builder) headLinks(page string) string {
	var sb strings.Builder
	if b.o.SiteURL != "" {
		fmt.Fprintf(&sb, `<link rel="alternate" type="application/atom+xml" title="%s" href="feed.xml">`,
			esc(b.feedTitle()))
	}
	if b.o.SiteURL == "" || len(b.o.AllLangs) < 2 {
		return sb.String()
	}
	for _, l := range b.o.AllLangs {
		fmt.Fprintf(&sb, `<link rel="alternate" hreflang="%s" href="%s/%s">`, l, b.langURL(l), page)
	}
	fmt.Fprintf(&sb, `<link rel="alternate" hreflang="x-default" href="%s/%s">`, b.langURL(b.rootLang), page)
	// 其他语言的 feed 也声明出来：阅读器填首页自动发现时能一次看到三份
	for _, l := range b.o.AllLangs {
		if l == b.o.Lang {
			continue
		}
		fmt.Fprintf(&sb, `<link rel="alternate" type="application/atom+xml" hreflang="%s" title="%s" href="%s/feed.xml">`,
			l, esc(i18n.T(l, "site.name")+" · "+i18n.Label[l]), b.langURL(l))
	}
	return sb.String()
}

// langSwitch 语言切换器：一个框里三段，不是三个独立按钮。
// 除了视觉上是整体，也更窄——省掉三份外边距与按钮间的间隙。
// 全称与缩写都输出，由 CSS 按屏宽切换：同一份 HTML 两种宽度都对。
func (b *Builder) langSwitch(page string) string {
	if len(b.o.AllLangs) < 2 {
		return ""
	}
	up := ""
	if b.o.Lang != b.rootLang {
		up = "../"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<span class="lang" title="%s">`, esc(b.T("nav.lang")))
	for _, l := range b.o.AllLangs {
		txt := fmt.Sprintf(`<b>%s</b><i>%s</i>`, esc(i18n.Label[l]), esc(i18n.Abbr[l]))
		if l == b.o.Lang {
			fmt.Fprintf(&sb, `<span class="on">%s</span>`, txt)
			continue
		}
		href := up
		if l != b.rootLang {
			href += string(l) + "/"
		}
		fmt.Fprintf(&sb, `<a href="%s%s">%s</a>`, href, page, txt)
	}
	sb.WriteString("</span>")
	return sb.String()
}

func (b *Builder) nav(day string) string {
	i := indexOf(b.days, day)
	prev := fmt.Sprintf(`<span class="dis" title="%s">%s</span>`,
		esc(firstNonEmpty(b.keepNote(), b.T("nav.oldest_tip"))),
		ifElse(b.windowed, b.T("nav.window_end", len(b.days)), b.T("nav.oldest_end")))
	if i > 0 {
		prev = fmt.Sprintf(`<a id="prevday" href="%s.html">‹<em>&nbsp;%s</em></a>`,
			b.days[i-1], b.days[i-1][5:])
	}
	next := fmt.Sprintf(`<span class="dis">%s</span>`, b.T("nav.newest_end"))
	if i < len(b.days)-1 {
		next = fmt.Sprintf(`<a id="nextday" href="%s.html"><em>%s&nbsp;</em>›</a>`,
			b.days[i+1], b.days[i+1][5:])
	}

	// 下拉默认不落在任何一天上——预选某天会让人以为「我选了这天」
	// 而不是「这是最新的一天」。
	var opts strings.Builder
	fmt.Fprintf(&opts, `<option value="" selected>%s</option>`, esc(b.T("nav.jump")))
	recent := reversed(b.days)
	if len(recent) > recentDays {
		recent = recent[:recentDays]
	}
	if !contains(recent, day) {
		recent = append(recent, day)
	}
	for _, d := range recent {
		mark := ""
		if d == b.days[len(b.days)-1] {
			mark = b.T("nav.newest_mark")
		}
		fmt.Fprintf(&opts, `<option value="%s">%s%s</option>`, d, d, esc(mark))
	}
	if len(b.days) > len(recent) {
		fmt.Fprintf(&opts, `<option value="@archive">%s</option>`, esc(b.T("nav.more", len(b.days))))
	}

	return fmt.Sprintf(`<div class="nav">%s<select id="daysel" aria-label="%s">%s</select>%s`+
		`<a class="all" href="archive.html">%s</a>%s`+
		`<button class="tg" id="theme" title="%s" aria-label="%s" aria-pressed="false">☀</button></div>`,
		prev, esc(b.T("nav.date_label")), opts.String(), next, esc(b.T("nav.all", len(b.days))),
		b.langSwitch(day+".html"), esc(b.T("nav.theme")), esc(b.T("nav.theme")))
}

func (b *Builder) writeDay(day string) error {
	var tabs, panes strings.Builder
	first := true
	for _, t := range task.All {
		label := b.T("panel." + t.Key)
		path, ok := b.reportFile(day, t)
		if !ok {
			// 缺报告的档也占一个 tab 位（读者要知道「今天没有这份」），但标成不可用；
			// 它没有对应面板，所以不给 aria-controls。
			fmt.Fprintf(&tabs, `<button class="tab absent" data-t="%s" role="tab" aria-selected="false" aria-disabled="true" title="%s">%s</button>`,
				t.Key, esc(b.T("tab.absent")), esc(label))
			continue
		}
		body, err := b.renderBody(path)
		if err != nil {
			return err
		}
		// 宽表在窄屏只能横向滚，包一层容器才能加滚动提示
		body = strings.ReplaceAll(body, "<table>", `<div class="tw"><div><table>`)
		body = strings.ReplaceAll(body, "</table>", "</table></div></div>")

		draft := strings.Contains(filepath.Base(path), "_draft")
		tip, badge := "", ""
		if draft {
			tip = fmt.Sprintf(` title="%s"`, esc(b.T("tab.draft_tip")))
			badge = fmt.Sprintf(` <em title="%s">%s</em>`,
				esc(b.T("tab.draft_title")), esc(b.T("tab.draft_badge")))
		}
		on, selected := "", "false"
		if first {
			on, selected = " on", "true"
		}
		// role / aria-selected 让读屏报出「哪一档是选中的」；aria-controls 与面板的
		// aria-labelledby 互指。选中态先按服务端的第一档写死，之后由 site.js 在点击
		// 与深链（#douban）时改写。方向键有意不归 tab——见 AGENTS.md「栏目 tab」一节。
		fmt.Fprintf(&tabs, `<button class="tab%s" data-t="%s" id="t-%s" role="tab" aria-selected="%s" aria-controls="p-%s"%s>%s%s</button>`,
			on, t.Key, t.Key, selected, t.Key, tip, esc(label), badge)
		st, err := os.Stat(path)
		at := b.genAt
		modTime := at.Unix()
		if err == nil {
			at = st.ModTime()
			modTime = st.ModTime().Unix()
		}
		fmt.Fprintf(&panes, `<section class="pane%s" id="p-%s" role="tabpanel" aria-labelledby="t-%s"><div class="src">%s</div>%s%s</section>`,
			on, t.Key, t.Key, esc(relTo(b.o.Root, path)), b.genSrcNote(at), body)
		first = false

		b.entries = append(b.entries, entry{Day: day, Key: t.Key, Label: label,
			ModTime: modTime, Body: body})
	}

	main := panes.String()
	if main == "" {
		main = "<p>" + esc(b.T("site.none_today")) + "</p>"
	}
	newest := day == b.days[len(b.days)-1]
	status := ""
	latest := ""
	if newest {
		status = b.statusBar()
		latest = fmt.Sprintf(`<b class="new">%s</b>`, esc(b.T("site.latest")))
	}
	note := ""
	if b.windowed {
		note = fmt.Sprintf(`<p class="keep">%s</p>`, esc(b.keepNote()))
	}

	doc := fmt.Sprintf(`<!doctype html><html lang="%s"><head><meta charset="utf-8">`+
		viewport+`<title>%s %s</title>%s%s%s<style>%s</style></head><body>`+
		`<header><div class="bar"><h1 class="site">%s · %s%s</h1>%s</div>%s</header>`+
		`<nav class="tabbar"><div class="tabs" role="tablist" aria-label="%s">%s</div></nav><main>%s`+
		`<footer class="dis">%s%s</footer></main><script>%s</script></body></html>`,
		i18n.HTMLLang(b.o.Lang), esc(b.T("site.name")), day,
		b.headIcons(), b.headLinks(day+".html"), headJS, css,
		esc(b.T("site.name")), day, latest, b.nav(day), status,
		esc(b.T("nav.tabs_label")), tabs.String(), main, note, b.disclaimer(), js)

	return os.WriteFile(filepath.Join(b.o.Out, day+".html"), []byte(doc), 0o644)
}

// writeIndex index.html 是最新一天的副本。对象存储没有「默认文档」概念，
// 访问 / 不会自动返回 index.html，Worker 那层会补。
func (b *Builder) writeIndex() error {
	newest := b.days[len(b.days)-1]
	src, err := os.ReadFile(filepath.Join(b.o.Out, newest+".html"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.o.Out, "index.html"), src, 0o644)
}

func (b *Builder) writeArchive() error {
	var rows strings.Builder
	var month string
	for _, d := range reversed(b.days) {
		if m := d[:7]; m != month {
			month = m
			n := 0
			for _, x := range b.days {
				if strings.HasPrefix(x, m) {
					n++
				}
			}
			fmt.Fprintf(&rows, `<tr class="mh"><td colspan="2">%s　<span>%s</span></td></tr>`,
				m, esc(b.T("arch.days", n)))
		}
		var chips strings.Builder
		for _, t := range task.All {
			path, ok := b.reportFile(d, t)
			cls := "no"
			mark := ""
			if ok {
				cls = "has"
				if strings.Contains(filepath.Base(path), "_draft") {
					cls = "draft"
					mark = b.T("tab.draft_mark")
				}
			}
			fmt.Fprintf(&chips, `<span class="%s">%s%s</span>`, cls, esc(b.T("short."+t.Key)), esc(mark))
		}
		latest := ""
		if d == b.days[len(b.days)-1] {
			latest = fmt.Sprintf(`<b class="new">%s</b>`, esc(b.T("site.latest")))
		}
		fmt.Fprintf(&rows, `<tr><td class="d"><a href="%s.html">%s</a>%s</td><td>%s</td></tr>`,
			d, d, latest, chips.String())
	}

	scope := b.T("arch.total")
	if b.windowed {
		scope = b.T("arch.recent")
	}
	note := ""
	if b.windowed {
		note = fmt.Sprintf(`<p class="keep">%s</p>`, esc(b.keepNote()))
	}
	doc := fmt.Sprintf(`<!doctype html><html lang="%s"><head><meta charset="utf-8">`+
		viewport+`<title>%s</title>%s%s%s<style>%s</style></head><body>`+
		`<header><div class="bar"><h1 class="site">%s · %s %s</h1>`+
		`<div class="nav"><a href="index.html">%s</a>%s`+
		`<button class="tg" id="theme" title="%s" aria-label="%s" aria-pressed="false">☀</button></div></div></header>`+
		`<main>%s<table class="arch"><tr><th>%s</th><th>%s</th></tr>%s</table>`+
		`<footer class="dis">%s<p class="gen">%s</p></main><script>%s</script></body></html>`,
		i18n.HTMLLang(b.o.Lang), esc(b.T("site.archive")),
		b.headIcons(), b.headLinks("archive.html"), headJS, css,
		esc(b.T("site.archive")), esc(scope), esc(b.T("arch.days", len(b.days))),
		esc(b.T("nav.back", b.days[len(b.days)-1])), b.langSwitch("archive.html"), esc(b.T("nav.theme")), esc(b.T("nav.theme")),
		note, esc(b.T("arch.date")), esc(b.T("arch.reports")), rows.String(),
		b.disclaimer(), b.genNote(), js)
	return os.WriteFile(filepath.Join(b.o.Out, "archive.html"), []byte(doc), 0o644)
}

// ── 小工具 ──

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}
func contains(ss []string, s string) bool { return indexOf(ss, s) >= 0 }
func reversed(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[len(ss)-1-i] = s
	}
	return out
}
func ifElse(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
func relTo(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = time.Now
