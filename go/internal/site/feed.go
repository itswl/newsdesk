package site

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/i18n"
	"github.com/itswl/newsdesk/internal/task"
)

// feedTitle 多语言时带语言标识：「每日简报」和「每日簡報」在阅读器侧边栏里
// 只差两个字，同时订阅三种语言基本分不出哪个是哪个。单语站点不加这个尾巴。
func (b *Builder) feedTitle() string {
	t := b.T("site.name")
	if len(b.o.AllLangs) > 1 {
		t += " · " + i18n.Label[b.o.Lang]
	}
	return t
}

// writeFeed 生成 Atom 1.0。
//
// 条数由窗口推导（天数 × 每天篇数），不另设固定上限——写死条数的话满负荷时
// feed 会比站点窄，而且不报错，只有逐条数日期才看得出来。
func (b *Builder) writeFeed() error {
	if b.o.SiteURL == "" {
		b.logf("  未配 SITE_URL，跳过 feed\n")
		return nil
	}
	items := append([]entry(nil), b.entries...)
	sort.Slice(items, func(i, j int) bool { return items[i].ModTime > items[j].ModTime })
	max := len(b.days) * len(task.All)
	if b.o.Days == 0 {
		max = 20 // 不限窗时 days 可能攒到上百天，全放进去会几十 MB
	}
	if len(items) > max {
		items = items[:max]
	}

	base := b.baseURL()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom">` + "\n")
	fmt.Fprintf(&sb, "<title>%s</title>\n", esc(b.feedTitle()))
	fmt.Fprintf(&sb, "<subtitle>%s</subtitle>\n", esc(b.T("site.feed_sub")))
	fmt.Fprintf(&sb, "<id>%s/</id>\n", base)
	fmt.Fprintf(&sb, `<link rel="alternate" type="text/html" href="%s/"/>`+"\n", base)
	fmt.Fprintf(&sb, `<link rel="self" type="application/atom+xml" href="%s/feed.xml"/>`+"\n", base)
	for _, l := range b.o.AllLangs {
		if l == b.o.Lang {
			continue
		}
		fmt.Fprintf(&sb, `<link rel="alternate" type="application/atom+xml" hreflang="%s" title="%s" href="%s/feed.xml"/>`+"\n",
			l, esc(i18n.T(l, "site.name")+" · "+i18n.Label[l]), b.langURL(l))
	}
	updated := time.Now()
	if len(items) > 0 {
		updated = time.Unix(items[0].ModTime, 0)
	}
	fmt.Fprintf(&sb, "<updated>%s</updated>\n", updated.Format(time.RFC3339))
	fmt.Fprintf(&sb, `<generator uri="%s">newsdesk</generator>`+"\n", b.o.SiteURL)
	if b.o.Contact != "" {
		fmt.Fprintf(&sb, "<author><name>newsdesk</name><email>%s</email></author>\n", esc(b.o.Contact))
	}

	for _, e := range items {
		// id 跟着语言走，且稳定不变——变了阅读器会把旧内容当新的重推一遍
		url := fmt.Sprintf("%s/%s.html#%s", base, e.Day, e.Key)
		t := time.Unix(e.ModTime, 0).Format(time.RFC3339)
		sb.WriteString("<entry>\n")
		fmt.Fprintf(&sb, "<title>%s · %s</title>\n", esc(e.Label), e.Day)
		fmt.Fprintf(&sb, "<id>%s</id>\n", url)
		fmt.Fprintf(&sb, `<link rel="alternate" type="text/html" href="%s"/>`+"\n", url)
		fmt.Fprintf(&sb, "<updated>%s</updated>\n<published>%s</published>\n", t, t)
		fmt.Fprintf(&sb, `<category term="%s"/>`+"\n", e.Key)
		fmt.Fprintf(&sb, "<content type=\"html\">%s</content>\n", cdata(e.Body))
		sb.WriteString("</entry>\n")
	}
	sb.WriteString("</feed>\n")

	path := filepath.Join(b.o.Out, "feed.xml")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	covered := map[string]bool{}
	for _, e := range items {
		covered[e.Day] = true
	}
	st, _ := os.Stat(path)
	warn := ""
	if len(covered) != len(b.days) {
		warn = " ← 被条数上限截短了"
	}
	b.logf("  feed.xml  (%d 条 · 覆盖 %d 天 / 站点 %d 天%s，%.0f KB)\n",
		len(items), len(covered), len(b.days), warn, float64(st.Size())/1024)
	return nil
}

// cdata 把正文包进 CDATA。
//
// 不用 XML 转义：转义会把每个 < 变成 &lt;，正文体积近乎翻倍。CDATA 内部只有
// ]]> 这一个序列需要处理——它会提前结束 CDATA 段，把后面的正文当标记解析，
// 整个 feed 就此损坏。拆成两段是唯一正确的做法，没有转义写法。
func cdata(s string) string {
	return "<![CDATA[" + strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>") + "]]>"
}
