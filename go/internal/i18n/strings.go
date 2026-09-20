// Package i18n 管界面文案与繁体转换。
//
// 只手写简体与英文两份。繁体由简体现推——s2twp 的词汇转换质量够用，
// 再维护第三份表只会让三者慢慢对不上。
package i18n

import "fmt"

type Lang string

const (
	ZhCN Lang = "zh-CN"
	ZhTW Lang = "zh-TW"
	En   Lang = "en"
)

var All = []Lang{ZhCN, ZhTW, En}

func Valid(s string) bool {
	for _, l := range All {
		if Lang(s) == l {
			return true
		}
	}
	return false
}

// Label 语言切换器上的全称；Abbr 是窄屏用的缩写。
// 完整标签三个加起来 142px，在 320px 的屏上会把日期下拉挤没。
var (
	Label = map[Lang]string{ZhCN: "简体", ZhTW: "繁體", En: "English"}
	Abbr  = map[Lang]string{ZhCN: "简", ZhTW: "繁", En: "EN"}
)

// s 是文案表：key -> {简体, 英文}。新增文案只需补这两项。
var s = map[string][2]string{
	"site.name":       {"每日简报", "Daily Briefing"},
	"site.archive":    {"历史简报", "Archive"},
	"site.latest":     {"最新", "Latest"},
	"site.none_today": {"当天没有任何报告。", "No reports for this day."},
	"site.feed_sub": {"AI 新闻 · GitHub Trending · 摸摸鱼热榜 · 豆瓣电影",
		"AI News · GitHub Trending · Momoyu Hot List · Douban Films"},

	// 手机主屏图标底下的名字。主屏只给十几个字的位置，英文尤其不能长，
	// 所以这里比 site.name 短——超出会被截成「Daily Brief…」。
	"app.short": {"每日简报", "Daily Brief"},

	"panel.ai":       {"AI 简报", "AI News"},
	"panel.trending": {"GitHub Trending", "GitHub Trending"},
	"panel.momoyu":   {"摸摸鱼热榜", "Momoyu Hot List"},
	"panel.douban":   {"豆瓣电影", "Douban Films"},
	"short.ai":       {"AI", "AI"},
	"short.trending": {"Trending", "Trending"},
	"short.momoyu":   {"摸摸鱼", "Momoyu"},
	"short.douban":   {"豆瓣", "Douban"},

	"nav.jump": {"跳到日期…", "Jump to date…"},
	// 下拉本身没有可见 label，占位项只是提示语；读屏需要在聚焦时报出用途。
	"nav.date_label":  {"选择日期", "Choose a date"},
	"nav.newest_mark": {"（最新）", " (latest)"},
	"nav.more":        {"更多历史（共 %d 天）…", "More history (%d days)…"},
	"nav.all":         {"全部 %d 天", "All %d days"},
	"nav.newest_end":  {"最新 ›", "Latest ›"},
	"nav.oldest_end":  {"‹ 最早", "‹ Oldest"},
	"nav.window_end":  {"‹ 仅存 %d 天", "‹ %d days kept"},
	"nav.oldest_tip":  {"这是最早的一天", "This is the earliest day available."},
	"nav.back":        {"回到最新 (%s) ›", "Back to latest (%s) ›"},
	"nav.theme":       {"切换深浅色", "Toggle light/dark"},
	"nav.lang":        {"切换语言", "Switch language"},

	"tab.absent": {"当天没有这份报告——任务没跑，或者跑了但失败了",
		"No such report for this day — the task did not run, or it failed."},
	"tab.draft_tip": {"这是草稿版本：当天的正式版还没生成，正式版出来后会取代它",
		"Draft: the final version for this day has not been generated yet."},
	"tab.draft_badge": {"草稿", "draft"},
	"tab.draft_mark":  {"·草稿", "·draft"},
	"tab.draft_title": {"草稿版本，非最终稿", "Draft, not the final version"},

	"arch.date":    {"日期", "Date"},
	"arch.reports": {"报告", "Reports"},
	"arch.days":    {"%d 天", "%d days"},
	"arch.total":   {"共", "all"},
	"arch.recent":  {"最近", "last"},

	"keep.note": {"本站对外仅保留最近 %d 天的简报，更早的已下线。",
		"Only the last %d days are kept online; earlier reports have been taken down."},

	"st.ok":      {"今天 %s 跑完", "finished today at %s"},
	"st.draft":   {"今天 %s 出了草稿，正式版还没生成", "draft produced today at %s; final version pending"},
	"st.failed":  {"今天失败了，重试 %s 次后放弃；详见 logs/", "failed today, gave up after %s retries; see logs/"},
	"st.retry":   {"第 %s 次重试中", "retrying, attempt %s"},
	"st.skipped": {"错过补跑截止点，今日跳过", "missed the catch-up deadline; skipped today"},
	"st.pending": {"今天还没跑", "not run yet today"},
	"st.planned": {"，计划 %s", ", scheduled %s"},

	// 内容更新时间。精确到分，带时区偏移——这套东西在时区上吃过两次亏
	// （status.json 的 epoch、Worker 的 UTC 解析），页面上的时间不带偏移量
	// 的话，读者无法判断它是哪个时区的 10:41。
	"gen.at":         {"本页内容更新于 %s", "Page content updated at %s"},
	"gen.section_at": {"本栏内容更新于 %s", "Section content updated at %s"},

	"dis.body": {
		"本站为个人非商业性质的信息聚合与评述项目，内容由程序自动采集公开可访问的信息源，" +
			"交由 AI 自主分析生成，仅供个人学习与研究使用，不代表任何机构立场，亦不用于任何商业目的。" +
			"文中引用的标题、摘要、简介、仓库描述等材料，著作权均归原作者或原平台所有，" +
			"本站仅作必要引用以支撑评述，并在各处标注来源、保留原文链接，" +
			"不主张对这些材料的任何权利，也不鼓励脱离原始出处传播。" +
			"分析与评述部分为 AI 自主分析，可能存在错误、遗漏或偏差，一切以原始来源为准。",
		"This is a personal, non-commercial aggregation-and-commentary project. " +
			"Content is collected automatically from publicly accessible sources and analysed by AI, " +
			"for personal study and research only; it represents no organisation and is not used commercially. " +
			"Copyright in quoted titles, summaries, synopses and repository descriptions remains with the " +
			"original authors or platforms; this site quotes only what the commentary requires, attributes " +
			"sources throughout, keeps links to the originals, claims no rights over that material, and does " +
			"not encourage circulating it apart from its source. " +
			"The analysis and commentary are produced by AI and may contain errors, omissions or bias — " +
			"always defer to the original source."},
	"dis.contact": {
		"如您是权利人并认为本站内容侵犯了您的权益，或希望移除对某一来源的引用，" +
			"请联系 <a href=\"mailto:%s\">%s</a>，核实后立即删除，无需其他前置程序。",
		"If you are a rights holder and believe this site infringes your rights, or you would like a " +
			"source removed, contact <a href=\"mailto:%s\">%s</a>; it will be deleted upon verification, " +
			"with no further formalities."},
	"dis.contact_none": {
		"如您是权利人并认为本站内容侵犯了您的权益，请与本站联系，核实后立即删除。",
		"If you are a rights holder and believe this site infringes your rights, please contact us; " +
			"it will be deleted upon verification."},
}

// T 取某语言的文案。缺 key 直接 panic——静默退回会让漏翻的地方一直没人发现，
// 而 key 是代码里写死的常量，缺失只可能是笔误。
func T(lang Lang, key string, args ...any) string {
	v, ok := s[key]
	if !ok {
		panic("i18n: 未知文案 key " + key)
	}
	var out string
	switch lang {
	case En:
		out = v[1]
	case ZhTW:
		out = ToTraditional(v[0])
	default:
		out = v[0]
	}
	if len(args) > 0 {
		return fmt.Sprintf(out, args...)
	}
	return out
}

// Keys 供测试遍历。
func Keys() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

// HTMLLang <html lang="..."> 的取值。
func HTMLLang(l Lang) string { return string(l) }
