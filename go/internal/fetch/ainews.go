package fetch

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/store"
)

type Feed struct {
	Name, URL, Tier string
	Cap             int           // 每源上限，0 = 用默认
	Pause           time.Duration // 抓取前等待，应对限流
}

// 分层不是装饰：报告里「厂商公告」和「社区讨论」的权重不同，
// 分类随时间会调整，但一手/二手/社区这三档是稳定的。
//
// 每源上限：论文和社区帖天天都有，不限的话会把厂商公告、深度报道淹掉——
// 实测不设限时社区源占七成、一手公告只有 2 条，日报不该是这个配比。
//
// arXiv 官方要求请求间隔 3 秒，Reddit 未公开阈值给 2 秒经验值。
// 单独测能通、混在批量里就 429。
var Feeds = []Feed{
	{"OpenAI", "https://openai.com/news/rss.xml", "一手·厂商公告", 0, 0},
	{"Google DeepMind", "https://deepmind.google/blog/rss.xml", "一手·厂商公告", 0, 0},
	{"TechCrunch AI", "https://techcrunch.com/category/artificial-intelligence/feed/", "二手·科技媒体", 0, 0},
	{"The Verge AI", "https://www.theverge.com/rss/ai-artificial-intelligence/index.xml", "二手·科技媒体", 0, 0},
	{"Ars Technica", "https://feeds.arstechnica.com/arstechnica/technology-lab", "二手·科技媒体", 0, 0},
	{"Simon Willison", "https://simonwillison.net/atom/everything/", "二手·独立评论", 0, 0},
	{"Hacker News", "https://news.ycombinator.com/rss", "社区·首页", 12, 0},
	{"Hugging Face", "https://huggingface.co/blog/feed.xml", "一手·厂商公告", 0, 0},
	{"MIT Tech Review", "https://www.technologyreview.com/topic/artificial-intelligence/feed", "二手·科技媒体", 0, 0},
	{"Import AI", "https://importai.substack.com/feed", "二手·独立评论", 0, 0},
	{"r/LocalLLaMA", "https://www.reddit.com/r/LocalLLaMA/.rss", "社区·开源模型", 10, 2 * time.Second},
	{"arXiv cs.AI", "https://export.arxiv.org/api/query?search_query=cat:cs.AI" +
		"&sortBy=submittedDate&sortOrder=descending&max_results=12", "一手·论文预印本", 12, 3 * time.Second},
}

type Item struct {
	Source  string `json:"source"`
	Tier    string `json:"source_kind"`
	Title   string `json:"title"`
	Link    string `json:"link"`
	Date    string `json:"date"`
	Summary string `json:"summary,omitempty"`
	Points  int    `json:"points,omitempty"`
	Comment int    `json:"comments,omitempty"`
}

type aiDoc struct {
	FetchedAt   string `json:"fetched_at"`
	WindowHours int    `json:"window_hours"`
	Items       []Item `json:"items"`
	// sources 是 [名称, 状态, 总数, 窗内数] 的四元组数组。
	// history 要按这个形状读它来找「近期有失败的源」，所以形状不能随便改。
	Sources []aiStat `json:"sources"`
}

type aiStat struct {
	Source, Status string
	Total, Kept    int
}

// MarshalJSON 输出成四元组数组而不是对象——history 按位置读。
func (s aiStat) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{s.Source, s.Status, s.Total, s.Kept})
}

func (s *aiStat) UnmarshalJSON(b []byte) error {
	var raw []any
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) < 4 {
		return err
	}
	s.Source, _ = raw[0].(string)
	s.Status, _ = raw[1].(string)
	if f, ok := raw[2].(float64); ok {
		s.Total = int(f)
	}
	if f, ok := raw[3].(float64); ok {
		s.Kept = int(f)
	}
	return nil
}

type AIOpts struct {
	Hours   int // 只保留这个时间窗内的条目
	PerFeed int // 无 Cap 的源用这个上限
}

// AINews 抓 15 个源。
//
// 所有联网都在这里——分析层跑在沙箱里，读不到凭据也出不了网。这是双引擎可互换
// 的前提：没有联网能力的引擎会用 shell 去本地翻文件硬凑答案。
func AINews(ctx context.Context, c *Client, st *store.Store, date string, o AIOpts) error {
	if o.Hours == 0 {
		o.Hours = 36
	}
	if o.PerFeed == 0 {
		o.PerFeed = 25
	}
	dd := st.Data("ainews", date)
	if err := st.MkdirAll(dd); err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(o.Hours) * time.Hour)

	doc := aiDoc{FetchedAt: time.Now().Format(time.RFC3339), WindowHours: o.Hours}

	// Anthropic 没有 RSS，单独解析页面
	if items, err := anthropicNews(ctx, c); err != nil {
		doc.Sources = append(doc.Sources, aiStat{"Anthropic", "FAIL", 0, 0})
		fmt.Printf("  %-18s 抓取失败: %v\n", "Anthropic", err)
	} else {
		kept := keepFresh(items, cutoff, 0)
		doc.Items = append(doc.Items, kept...)
		doc.Sources = append(doc.Sources, aiStat{"Anthropic", "OK", len(items), len(kept)})
		fmt.Printf("  %-18s 总 %-4d 窗内 %d\n", "Anthropic", len(items), len(kept))
	}

	// Qwen 没有可用的 RSS，走 Hugging Face 的组织接口（见 qwenModels）
	if items, err := qwenModels(ctx, c); err != nil {
		doc.Sources = append(doc.Sources, aiStat{"Qwen", "FAIL", 0, 0})
		fmt.Printf("  %-18s 抓取失败: %v\n", "Qwen", err)
	} else {
		kept := keepFresh(items, cutoff, 0)
		doc.Items = append(doc.Items, kept...)
		doc.Sources = append(doc.Sources, aiStat{"Qwen", "OK", len(items), len(kept)})
		fmt.Printf("  %-18s 总 %-4d 窗内 %d\n", "Qwen", len(items), len(kept))
	}

	for _, f := range Feeds {
		if f.Pause > 0 {
			sleep(ctx, f.Pause)
		}
		body, err := c.Get(ctx, f.URL, nil)
		if err != nil {
			doc.Sources = append(doc.Sources, aiStat{f.Name, "FAIL", 0, 0})
			fmt.Printf("  %-18s 抓取失败: %v\n", f.Name, err)
			continue
		}
		recs := parseFeed(body)
		limit := f.Cap
		if limit == 0 {
			limit = o.PerFeed
		}
		if len(recs) > limit {
			recs = recs[:limit]
		}
		for i := range recs {
			recs[i].Source, recs[i].Tier = f.Name, f.Tier
		}
		kept := keepFresh(recs, cutoff, 0)
		doc.Items = append(doc.Items, kept...)
		doc.Sources = append(doc.Sources, aiStat{f.Name, "OK", len(recs), len(kept)})
		fmt.Printf("  %-18s 总 %-4d 窗内 %d\n", f.Name, len(recs), len(kept))
	}

	// HN Algolia：按分数排序的近期高热帖，与 HN 首页互补
	if n, err := hnAlgolia(ctx, c, &doc, o.Hours); err != nil {
		doc.Sources = append(doc.Sources, aiStat{"HN Algolia", "FAIL", 0, 0})
		fmt.Printf("  HN Algolia 失败: %v\n", err)
	} else {
		doc.Sources = append(doc.Sources, aiStat{"HN Algolia", "OK", n, n})
		fmt.Printf("  %-18s AI 相关 %d\n", "HN Algolia", n)
	}

	if err := store.WriteJSON(filepath.Join(dd, "items.json"), doc, true); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dd, "digest.txt"),
		[]byte(digest(doc)), 0o644); err != nil {
		return err
	}
	fmt.Printf("-> %s/{items.json, digest.txt}  共 %d 条\n", st.RelTo(dd), len(doc.Items))
	return nil
}

func keepFresh(items []Item, cutoff time.Time, _ int) []Item {
	var out []Item
	for _, it := range items {
		iso, t, ok := normDate(it.Date)
		it.Date = iso
		// 解析不出日期的一律保留，交给模型判断——宁可多给不可漏
		if !ok || !t.Before(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

var (
	reTag      = regexp.MustCompile(`<[^>]+>`)
	reWS       = regexp.MustCompile(`\s+`)
	reAnchor   = regexp.MustCompile(`(?s)<a\s+href="(/news/[a-z0-9\-]+)"[^>]*>(.*?)</a>`)
	reTime     = regexp.MustCompile(`<time[^>]*>([^<]+)</time>`)
	reTitleCls = regexp.MustCompile(`(?s)<[a-z0-9]+[^>]*class="[^"]*title[^"]*"[^>]*>(.*?)</[a-z0-9]+>`)
	reHeading  = regexp.MustCompile(`(?s)<h[1-6][^>]*>(.*?)</h[1-6]>`)
	rePara     = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
)

func plain(s string) string {
	return strings.TrimSpace(reWS.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, "")), " "))
}

// anthropicNews Anthropic 官网没有 RSS（/rss.xml、/feed.xml 都 404），只能解析页面。
//
// 页面有两套布局：FeaturedGrid 用 <h4>，PublicationList 用 <span>。共同点是
// class 里带 title——按 class 匹配比按标签稳，因为 class 名带的构建哈希会随改版变。
//
// 解析出 0 条当作失败上报，不要静默返回空：否则改版后这个源会无声消失，
// 而它混在 15 个源里没人会注意到少了一个。
func anthropicNews(ctx context.Context, c *Client) ([]Item, error) {
	body, err := c.Get(ctx, "https://www.anthropic.com/news", nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Item
	for _, m := range reAnchor.FindAllStringSubmatch(string(body), -1) {
		href, inner := m[1], m[2]
		h := reTitleCls.FindStringSubmatch(inner)
		if h == nil {
			h = reHeading.FindStringSubmatch(inner)
		}
		if h == nil {
			continue
		}
		link := "https://www.anthropic.com" + href
		if seen[link] { // 同一条目在页面上可能出现多次
			continue
		}
		seen[link] = true
		it := Item{Source: "Anthropic", Tier: "一手·厂商公告", Title: plain(h[1]), Link: link}
		if t := reTime.FindStringSubmatch(inner); t != nil {
			it.Date = strings.TrimSpace(t[1])
		}
		if p := rePara.FindStringSubmatch(inner); p != nil {
			it.Summary = truncate(plain(p[1]), 400)
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("解析出 0 条，页面结构可能已变")
	}
	return out, nil
}

// qwenModels Qwen 的公告源。
//
// 官方博客 qwen.ai 是纯客户端渲染：普通 HTTP 只拿得到 SPA 外壳，六个常见路径都
// 没有 RSS。旧的 qwenlm.github.io feed 停在 2025-09——它在配置里躺了一年，天天
// 「OK 窗内 0」没人发现（这也是 sources_quiet 要跟 sources_failed 分开的原因）。
// 改用 Hugging Face 的组织接口：JSON、普通 HTTP，而对 Qwen 来说模型上 HF 就是
// 公告本身，博客反而滞后。
//
// 按 createdAt 排，不用 lastModified：后者 README 改一个字就刷新，2025 年的旧模型
// 会天天冒出来。
func qwenModels(ctx context.Context, c *Client) ([]Item, error) {
	body, err := c.Get(ctx, "https://huggingface.co/api/models?author=Qwen&sort=createdAt&direction=-1&limit=12",
		map[string]string{"accept": "application/json"})
	if err != nil {
		return nil, err
	}
	return parseHFModels(body)
}

func parseHFModels(body []byte) ([]Item, error) {
	var raw []struct {
		ID        string `json:"id"`
		CreatedAt string `json:"createdAt"`
		Private   bool   `json:"private"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 HF 模型列表: %w", err)
	}
	var out []Item
	for _, m := range raw {
		if m.Private || m.ID == "" {
			continue
		}
		out = append(out, Item{Source: "Qwen", Tier: "一手·厂商公告", Title: m.ID,
			Link: "https://huggingface.co/" + m.ID, Date: m.CreatedAt,
			Summary: "Hugging Face 上新建的模型仓库（Qwen 的发布以模型上架为准，博客滞后）"})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("解析出 0 个模型，接口形状可能已变")
	}
	return out, nil
}

// parseFeed RSS 2.0 与 Atom 通吃。namespace 各家不同，一律按 localname 匹配。
func parseFeed(body []byte) []Item {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var out []Item
	var cur *Item
	var field string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "item", "entry":
				cur = &Item{}
			case "link":
				if cur != nil {
					for _, a := range t.Attr {
						if a.Name.Local == "href" && cur.Link == "" {
							cur.Link = strings.TrimSpace(a.Value)
						}
					}
				}
			}
			field = t.Name.Local
		case xml.CharData:
			if cur == nil {
				continue
			}
			s := string(t)
			switch field {
			case "title":
				cur.Title += s
			case "link":
				if cur.Link == "" {
					cur.Link += strings.TrimSpace(s)
				}
			case "pubDate", "published", "updated":
				if cur.Date == "" {
					cur.Date = strings.TrimSpace(s)
				}
			case "description", "summary", "content":
				if cur.Summary == "" {
					cur.Summary += s
				}
			}
		case xml.EndElement:
			if t.Name.Local == "item" || t.Name.Local == "entry" {
				if cur != nil && strings.TrimSpace(cur.Title) != "" {
					cur.Title = plain(cur.Title)
					cur.Summary = truncate(plain(cur.Summary), 400)
					out = append(out, *cur)
				}
				cur = nil
			}
			field = ""
		}
	}
	return out
}

var dateLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano,
	"2006-01-02T15:04:05Z", "Jan 2, 2006", "2006-01-02",
}

// normDate 各家日期格式不统一，统一成 ISO；解析不了返回原串并告知失败。
func normDate(s string) (string, time.Time, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "GMT", "+0000"))
	if s == "" {
		return "", time.Time{}, false
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Local().Format("2006-01-02T15:04-07:00"), t, true
		}
	}
	return s, time.Time{}, false
}

type hnHit struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Points    int    `json:"points"`
	Comments  int    `json:"num_comments"`
	CreatedAt string `json:"created_at"`
	ObjectID  string `json:"objectID"`
}

var aiKW = regexp.MustCompile(`(?i)\b(ai|llm|gpt|claude|gemini|openai|anthropic|deepmind|deepseek|qwen|llama|mistral|nvidia|gpu|tpu|agent|agentic|transformer|diffusion|model|inference|training|rag|embedding|multimodal|reasoning|copilot|cursor|xai|grok)\b`)

func hnAlgolia(ctx context.Context, c *Client, doc *aiDoc, hours int) (int, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	// numericFilters 里的 > 必须 URL 编码成 %3E，否则 Algolia 直接 400。
	// 门槛 40 分是为了滤掉噪声——低分帖在 36 小时窗里能有几百条。
	u := fmt.Sprintf("https://hn.algolia.com/api/v1/search_by_date?tags=story"+
		"&numericFilters=created_at_i%%3E%d,points%%3E40&hitsPerPage=80", since)
	body, err := c.Get(ctx, u, nil)
	if err != nil {
		return 0, err
	}
	var r struct {
		Hits []hnHit `json:"hits"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, err
	}
	n := 0
	for _, h := range r.Hits {
		if !aiKW.MatchString(h.Title) {
			continue
		}
		link := h.URL
		if link == "" {
			link = "https://news.ycombinator.com/item?id=" + h.ObjectID
		}
		iso, _, _ := normDate(h.CreatedAt)
		doc.Items = append(doc.Items, Item{
			Source: "HN Algolia", Tier: "社区·高热", Title: h.Title, Link: link,
			Date: iso, Points: h.Points, Comment: h.Comments,
		})
		n++
	}
	return n, nil
}

// digest 给模型读的纯文本摘要。按源分组，源的顺序固定，便于模型对照分层。
func digest(doc aiDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "AI 新闻采集 · %s\n", time.Now().Format("2006-01-02 15:04 -0700"))
	b.WriteString(strings.Repeat("=", 70) + "\n")
	for _, s := range doc.Sources {
		fmt.Fprintf(&b, "  %-18s %-5s 总 %-4d 窗内 %d\n", s.Source, s.Status, s.Total, s.Kept)
	}
	b.WriteString("\n")
	bySrc := map[string][]Item{}
	var order []string
	for _, it := range doc.Items {
		if _, ok := bySrc[it.Source]; !ok {
			order = append(order, it.Source)
		}
		bySrc[it.Source] = append(bySrc[it.Source], it)
	}
	sort.SliceStable(order, func(i, j int) bool { return srcRank(order[i]) < srcRank(order[j]) })
	for _, src := range order {
		items := bySrc[src]
		fmt.Fprintf(&b, "\n## %s (%s) — %d 条\n", src, items[0].Tier, len(items))
		for i, it := range items {
			extra := ""
			if it.Points > 0 {
				extra = fmt.Sprintf("  [%d分/%d评]", it.Points, it.Comment)
			}
			fmt.Fprintf(&b, "%2d. %s%s\n    %s  %s\n", i+1, it.Title, extra, it.Date, it.Link)
			if it.Summary != "" {
				fmt.Fprintf(&b, "    %s\n", truncate(it.Summary, 300))
			}
		}
	}
	return b.String()
}

func srcRank(name string) int {
	for i, f := range Feeds {
		if f.Name == name {
			return i + 1
		}
	}
	if name == "Anthropic" {
		return 0
	}
	return 99
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

var _ = url.QueryEscape
