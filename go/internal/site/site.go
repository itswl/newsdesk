// Package site 把 reports/ 渲染成静态站。
//
// 只读 reports/ 与 state/，不碰 data/——这个边界是有意的：渲染可以脱离采集重跑。
package site

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/itswl/newsdesk/internal/i18n"
	"github.com/itswl/newsdesk/internal/markdown"
	"github.com/itswl/newsdesk/internal/task"
	"time"
)

type Options struct {
	Root     string
	Out      string      // 输出目录（绝对路径）
	Days     int         // 只生成最近 N 天，0 = 全部
	Lang     i18n.Lang   // 本次生成的语言
	AllLangs []i18n.Lang // 语言切换器里列哪些；少于 2 个则不显示切换器
	SiteURL  string      // 对外地址，留空则不生成 feed
	Contact  string
	// PushKey VAPID 公钥。留空则页面上不出现通知开关。
	// 是公开值（订阅时浏览器要用），但名字里**不能带 KEY**——发布前的凭据自检
	// 按名字收集配置值，带 KEY 会被当成密钥，于是每次发布都因为「产出里有凭据」
	// 而中止。私钥叫 PUSH_VAPID_PRIVATE_KEY，正是要被它收走。
	PushKey string
	// Out 进度输出去向。nil = os.Stdout。
	// 调度器无人值守跑时这些行要落进任务日志，不然出事后只剩一句「失败了」。
	Log io.Writer
}

type Builder struct {
	o        Options
	days     []string // 截断后的日期，升序
	allDays  []string // 截断前
	rootLang i18n.Lang
	genAt    time.Time // 当前页面所含报告的最新更新时间，页脚标注用
	windowed bool
	entries  []entry // feed 用
}

type entry struct {
	Day, Key, Label string
	ModTime         int64
	Body            string
}

var reDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func Build(o Options) error {
	b := &Builder{o: o, genAt: time.Now()}
	if len(o.AllLangs) > 0 {
		b.rootLang = o.AllLangs[0]
	} else {
		b.rootLang = o.Lang
	}
	if err := b.collectDays(); err != nil {
		return err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return err
	}
	// 图标与 webapp 清单不依赖日期，先写：页面里引用它们，缺了就是一堆 404
	if err := b.writeAssets(); err != nil {
		return err
	}
	for _, d := range b.days {
		b.genAt = b.contentTime(d)
		if err := b.writeDay(d); err != nil {
			return err
		}
	}
	if err := b.writeIndex(); err != nil {
		return err
	}
	b.genAt = b.latestContentTime()
	if err := b.writeArchive(); err != nil {
		return err
	}
	if err := b.writeFeed(); err != nil {
		return err
	}
	if err := b.writeStatus(); err != nil {
		return err
	}
	note := ""
	if b.windowed {
		note = fmt.Sprintf("（共 %d 天，按 --days %d 截断）", len(b.allDays), o.Days)
	}
	b.logf("已重建 %d 天：%s … %s%s\n  输出: %s\n",
		len(b.days), b.days[0], b.days[len(b.days)-1], note, o.Out)
	return nil
}

// contentTime 返回某天当前语言页面所含报告的最新更新时间。
//
// 站点是全量重建的，但报告通常只更新一档；用 time.Now() 会让所有历史页看起来
// 都被重新生成。页脚应反映页面内容最后一次变化，而不是这次全站构建的时刻。
func (b *Builder) contentTime(day string) time.Time {
	var latest time.Time
	for _, t := range task.All {
		path, ok := b.reportFile(day, t)
		if !ok {
			continue
		}
		st, err := os.Stat(path)
		if err == nil && st.ModTime().After(latest) {
			latest = st.ModTime()
		}
	}
	if latest.IsZero() {
		return time.Now()
	}
	return latest
}

func (b *Builder) latestContentTime() time.Time {
	var latest time.Time
	for _, day := range b.days {
		if t := b.contentTime(day); t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return time.Now()
	}
	return latest
}

func (b *Builder) collectDays() error {
	dir := filepath.Join(b.o.Root, "reports")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读 reports/: %w", err)
	}
	var skipped int
	for _, e := range ents {
		if !e.IsDir() || !reDate.MatchString(e.Name()) {
			continue
		}
		// 天数列表必须按**当前语言**筛。英文是后加的，早期那些天根本没有译文，
		// 不筛的话会生成一堆只有导航和页脚、正文为空的壳页面——21 KB，
		// 从归档点进去什么都没有，也不解释为什么。
		//
		// 筛在这一处就够：日期下拉、上下一天、归档页、feed 全由 days 推导，
		// 砍掉之后自动一致。换成「生成了再在页面上写一句没有译文」的话，
		// feed 里仍会多出空条目，归档也还是会链过去。
		if !b.hasAny(e.Name()) {
			skipped++
			continue
		}
		b.allDays = append(b.allDays, e.Name())
	}
	sort.Strings(b.allDays)
	if len(b.allDays) == 0 {
		return fmt.Errorf("reports/ 下没有任何含 %s 内容的日期目录（共 %d 天有其他语言的报告）",
			b.o.Lang, skipped)
	}
	if skipped > 0 {
		b.logf("  %s：%d 天没有该语言的报告，已跳过\n", b.o.Lang, skipped)
	}
	b.days = b.allDays
	// 截断只此一处：下面的日期下拉、上下一天、归档页、feed 全由 days 推导，
	// 砍掉尾巴之后自动一致。
	if b.o.Days > 0 && len(b.days) > b.o.Days {
		b.days = b.days[len(b.days)-b.o.Days:]
		b.windowed = true
	}
	return nil
}

// reportFile 挑出某天某个任务的报告文件。
//
// 英文是 translate 预先翻好的 <名字>.en.md；繁体没有独立文件，用简体原文现转。
// 带通配符的模式会连译文一起匹配到（github-trending_draft*.md 会匹配
// github-trending_draft0916.en.md），而刚翻完的译文 mtime 最新——不显式排除的话
// 英文会被选进中文站，且不报错。
func (b *Builder) reportFile(day string, t task.Task) (string, bool) {
	dir := filepath.Join(b.o.Root, "reports", day)
	var best string
	var bestMod int64 = -1
	for _, pat := range t.Files {
		if b.o.Lang == i18n.En {
			pat = strings.TrimSuffix(pat, ".md") + ".en.md"
		}
		hits, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, h := range hits {
			if b.o.Lang != i18n.En && isTranslated(h) {
				continue
			}
			st, err := os.Stat(h)
			if err != nil {
				continue
			}
			if m := st.ModTime().Unix(); m > bestMod {
				best, bestMod = h, m
			}
		}
	}
	return best, best != ""
}

// isTranslated 判断是不是某个语言的译文（.en.md 这类）。
func isTranslated(path string) bool {
	base := filepath.Base(path)
	for _, l := range i18n.All {
		if strings.HasSuffix(strings.ToLower(base), "."+strings.ToLower(string(l))+".md") {
			return true
		}
	}
	return false
}

func (b *Builder) T(key string, args ...any) string { return i18n.T(b.o.Lang, key, args...) }

// hasAny 这一天在当前语言下有没有任何一篇报告。
func (b *Builder) hasAny(day string) bool {
	for _, t := range task.All {
		if _, ok := b.reportFile(day, t); ok {
			return true
		}
	}
	return false
}

func (b *Builder) logf(format string, a ...any) {
	w := b.o.Log
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, a...)
}

func esc(s string) string { return html.EscapeString(s) }

// langURL 某语言的绝对基址：默认语言在根，其余带 /<lang>。
//
// feed 的 id 与链接必须跟着语言走——三语共用一个基址的话三份 feed 的条目 id
// 完全相同，而 Atom 要求 id 全局唯一：同时订阅两个语言的读者会看到同一个 id
// 配不同内容，阅读器要么去重、要么每次轮询来回翻。
func (b *Builder) langURL(l i18n.Lang) string {
	if b.o.SiteURL == "" {
		return ""
	}
	if l == b.rootLang {
		return b.o.SiteURL
	}
	return b.o.SiteURL + "/" + string(l)
}

func (b *Builder) baseURL() string { return b.langURL(b.o.Lang) }

// renderBody 读报告并渲染。繁体在这里现转，不落盘——24 KB 报告转换 0.02 秒，
// 存一份反而多一处会过期的副本。
func (b *Builder) renderBody(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	src := string(raw)
	if b.o.Lang == i18n.ZhTW {
		src = i18n.ToTraditional(src)
	}
	return markdown.Render(src), nil
}
