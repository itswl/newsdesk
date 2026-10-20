package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/store"
)

// reserve 预留配额，避免把它榨干导致下次运行直接失败。
const reserve = 8

type BoardItem struct {
	Rank           int    `json:"rank"`
	Repo           string `json:"repo"`
	Lang           string `json:"lang,omitempty"`
	Desc           string `json:"desc"`
	Stars          int    `json:"stars"`
	Forks          int    `json:"forks"`
	StarsTodayPage int    `json:"stars_today_page"`
}

type RepoSnap struct {
	Error       string   `json:"error,omitempty"`
	Stars       int      `json:"stars,omitempty"`
	Forks       int      `json:"forks,omitempty"`
	PushedAt    string   `json:"pushed_at,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	Branch      string   `json:"default_branch,omitempty"`
	HeadDate    string   `json:"head_date,omitempty"`
	HeadMsg     string   `json:"head_msg,omitempty"`
	LicenseSPDX string   `json:"license_spdx,omitempty"`
	OpenIssues  int      `json:"open_issues,omitempty"`
	Subscribers int      `json:"subscribers,omitempty"`
	SizeKB      int      `json:"size_kb,omitempty"`
	Archived    bool     `json:"archived,omitempty"`
	Topics      []string `json:"topics,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	DescAPI     string   `json:"desc_api,omitempty"`
	OwnerType   string   `json:"owner_type,omitempty"`
	IsFork      bool     `json:"is_fork,omitempty"`
}

type Extras struct {
	Releases        int    `json:"releases"`
	DownloadsTotal  int    `json:"asset_downloads_total"`
	LatestTag       string `json:"latest_tag,omitempty"`
	LatestPublished string `json:"latest_published,omitempty"`
	LatestDownloads int    `json:"latest_downloads,omitempty"`
}

type Snapshot struct {
	TSStart       string              `json:"_ts_local_start"`
	TSEnd         string              `json:"_ts_local"`
	BaselineFile  string              `json:"_baseline_file,omitempty"`
	BaselineTS    string              `json:"_baseline_ts,omitempty"`
	WindowHours   *float64            `json:"_window_hours"`
	BoardSize     int                 `json:"_board_size"`
	Errors        []string            `json:"_errors"`
	Authed        bool                `json:"_authed"`
	BudgetStart   int                 `json:"_budget_start"`
	SkippedRepos  []string            `json:"_skipped_repos"`
	SkippedExtras []string            `json:"_skipped_extras"`
	BoardExtras   map[string]Extras   `json:"board_extras"`
	Repos         map[string]RepoSnap `json:"repos"`
}

var (
	reRow   = regexp.MustCompile(`(?s)<article class="Box-row">(.*?)</article>`)
	reName  = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a\b[^>]*?href="/([^"]+?)"`)
	reDesc  = regexp.MustCompile(`(?s)<p class="col-9[^"]*">(.*?)</p>`)
	reLang  = regexp.MustCompile(`(?s)itemprop="programmingLanguage"[^>]*>\s*([^<]+?)\s*<`)
	reStars = regexp.MustCompile(`(?s)href="/[^"]+/stargazers"[^>]*>\s*(?:<[^>]*>\s*)*([\d,]+)`)
	reForks = regexp.MustCompile(`(?s)href="/[^"]+/forks"[^>]*>\s*(?:<[^>]*>\s*)*([\d,]+)`)
	reToday = regexp.MustCompile(`(?s)([\d,]+)\s*stars?\s*today`)
	reTags  = regexp.MustCompile(`<[^>]+>`)
)

// Trending 抓榜单页 + 逐仓库 API 快照。
//
// ⚠️ 页面上的 stars today 是滞后值，约为 24 小时前的速率。只能用于排序和判断
// 进出榜，绝不能当增量引用——加速中的项目被它低估，减速中的被高估。
func Trending(ctx context.Context, c *Client, gh *GitHub, st *store.Store, date, tier string) error {
	dd := st.Data("trending", date)
	if err := st.MkdirAll(dd); err != nil {
		return err
	}

	src, err := c.Get(ctx, "https://github.com/trending?since=daily", nil)
	if err != nil {
		return fmt.Errorf("榜单页抓取失败: %w", err)
	}
	if len(src) < 50000 {
		return fmt.Errorf("榜单页内容过短（%d 字节），可能被拦或改版", len(src))
	}
	if err := os.WriteFile(filepath.Join(dd, "page_raw_"+tier+".html"), src, 0o644); err != nil {
		return err
	}

	board := parseBoard(string(src))
	if len(board) == 0 {
		// 解析出 0 项当作失败上报，不静默——否则页面改版后这个源会无声消失
		return fmt.Errorf("页面结构变了：匹配到 %d 个 Box-row 但解析出 0 项",
			len(reRow.FindAllString(string(src), -1)))
	}
	if err := store.WriteJSON(filepath.Join(dd, "page.json"), board, true); err != nil {
		return err
	}
	fmt.Fprintf(c.w(), "榜单 %d 项 -> %s/page.json\n", len(board), st.RelTo(dd))

	// 追踪集合 = 今日榜 + **上一期榜单**，不是上一期快照里的全部仓库。
	// 后者会把它自己的 prev 一并继承、逐日累积（实测 9 天涨到 49 个）。
	// 方法论上只需要「上一期在榜、今天掉了」的那些来验证掉榜=降温。
	basePath, baseSnap := latestSnapshot(st, date)
	var prev []string
	if basePath != "" {
		prevDay := filepath.Base(filepath.Dir(basePath))
		if raw, err := os.ReadFile(filepath.Join(st.Data("trending", prevDay), "page.json")); err == nil {
			var pb []BoardItem
			if json.Unmarshal(raw, &pb) == nil {
				for _, x := range pb {
					prev = append(prev, x.Repo)
				}
			}
		}
		if len(prev) == 0 && baseSnap != nil {
			for r := range baseSnap.Repos {
				prev = append(prev, r) // 上一期榜单丢了才退回全量
			}
			sort.Strings(prev)
		}
	}
	var baseTS time.Time
	if baseSnap != nil {
		baseTS, _ = time.Parse(time.RFC3339, firstNonEmptyStr(baseSnap.TSStart, baseSnap.TSEnd))
	}
	fmt.Fprintf(c.w(), "基线: %s (%s)\n", orDash(st.RelTo(basePath), basePath == ""), orDash(baseTS.Format(time.RFC3339), baseTS.IsZero()))

	onBoard := map[string]bool{}
	repos := make([]string, 0, len(board)+len(prev))
	for _, x := range board {
		onBoard[x.Repo] = true
		repos = append(repos, x.Repo)
	}
	for _, r := range prev {
		if !onBoard[r] {
			repos = append(repos, r)
		}
	}

	budget := gh.Remaining(ctx)
	authed := gh.Authed() || budget > 100
	fmt.Fprintf(c.w(), "API 预算 %d 次（%s）\n", budget, ifStr(authed, "已认证", "未认证，将按预算降级"))

	snap := Snapshot{
		TSStart: time.Now().Format(time.RFC3339), BoardSize: len(board),
		Authed: authed, BudgetStart: budget,
		Errors: []string{}, SkippedRepos: []string{}, SkippedExtras: []string{},
		BoardExtras: map[string]Extras{}, Repos: map[string]RepoSnap{},
	}
	for _, r := range repos {
		if !authed && gh.Remaining(ctx) <= reserve {
			snap.SkippedRepos = append(snap.SkippedRepos, r)
			continue
		}
		d, err := gh.Repo(ctx, r)
		if err != nil {
			snap.Repos[r] = RepoSnap{Error: "repos_404"}
			snap.Errors = append(snap.Errors, r)
			continue
		}
		head, msg := gh.HeadCommit(ctx, r) // 免费，不占 API 预算
		s := RepoSnap{
			Stars: d.Stars, Forks: d.Forks, PushedAt: d.PushedAt, CreatedAt: d.CreatedAt,
			Branch: d.Branch, HeadDate: head, HeadMsg: msg,
			OpenIssues: d.OpenIssues, Subscribers: d.Subscribers, SizeKB: d.SizeKB,
			Archived: d.Archived, Topics: d.Topics, Homepage: d.Homepage,
			DescAPI: d.Description, OwnerType: d.Owner.Type, IsFork: d.Fork,
		}
		if d.License != nil {
			s.LicenseSPDX = d.License.SPDX
		}
		snap.Repos[r] = s
	}
	if n := len(snap.SkippedRepos); n > 0 {
		fmt.Fprintf(c.w(), "⚠ 预算不足，%d 个仓库未采集\n", n)
	}

	// 榜单项目的补充指标按剩余预算花。未认证时它们是第一批被牺牲的——
	// 核心的增量、授权、停更判断都不依赖它们。
	// ⚠️ release 下载量作为使用度代理有边界：走 Docker / PyPI / HuggingFace
	// 分发的项目下载数为 0 不代表无人使用。
	for _, x := range board {
		s, ok := snap.Repos[x.Repo]
		if !ok || s.Error != "" {
			continue
		}
		if gh.Remaining(ctx) <= reserve {
			snap.SkippedExtras = append(snap.SkippedExtras, x.Repo)
			continue
		}
		rel, err := gh.Releases(ctx, x.Repo)
		e := Extras{Releases: len(rel)}
		if err == nil && len(rel) > 0 {
			for _, r := range rel {
				for _, a := range r.Assets {
					e.DownloadsTotal += a.DownloadCount
				}
			}
			e.LatestTag = rel[0].TagName
			e.LatestPublished = rel[0].PublishedAt
			for _, a := range rel[0].Assets {
				e.LatestDownloads += a.DownloadCount
			}
		}
		snap.BoardExtras[x.Repo] = e
	}
	fmt.Fprintf(c.w(), "补充指标 %d 项%s\n", len(snap.BoardExtras),
		ifStr(len(snap.SkippedExtras) > 0, fmt.Sprintf("，%d 项因预算跳过", len(snap.SkippedExtras)), ""))

	snap.TSEnd = time.Now().Format(time.RFC3339)
	if basePath != "" {
		snap.BaselineFile = st.RelTo(basePath)
	}
	if !baseTS.IsZero() {
		snap.BaselineTS = baseTS.Format(time.RFC3339)
		start, _ := time.Parse(time.RFC3339, snap.TSStart)
		h := start.Sub(baseTS).Hours()
		h = float64(int(h*1000+0.5)) / 1000
		snap.WindowHours = &h
	}
	fn := filepath.Join(dd, "snap_"+tier+".json")
	if err := store.WriteJSON(fn, snap, true); err != nil {
		return err
	}
	fmt.Fprintf(c.w(), "快照 %d 项（%d 个 404）-> %s\n", len(snap.Repos), len(snap.Errors), st.RelTo(fn))
	if snap.WindowHours != nil {
		fmt.Fprintf(c.w(), "窗口 %.2fh\n", *snap.WindowHours)
	}
	return nil
}

func parseBoard(src string) []BoardItem {
	var out []BoardItem
	for i, r := range reRow.FindAllStringSubmatch(src, -1) {
		row := r[1]
		m := reName.FindStringSubmatch(row)
		if m == nil {
			continue
		}
		desc := ""
		if d := reDesc.FindStringSubmatch(row); d != nil {
			desc = strings.TrimSpace(reSpaces.ReplaceAllString(
				html.UnescapeString(reTags.ReplaceAllString(d[1], "")), " "))
		}
		lang := ""
		if l := reLang.FindStringSubmatch(row); l != nil {
			lang = strings.TrimSpace(l[1])
		}
		out = append(out, BoardItem{
			Rank: i + 1, Repo: strings.TrimSpace(m[1]), Lang: lang, Desc: desc,
			Stars: num(reStars, row), Forks: num(reForks, row), StarsTodayPage: num(reToday, row),
		})
	}
	return out
}

func num(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	return n
}

// latestSnapshot 找 date 之前最近一份基线，优先 t3（与 16:40 触发对齐），退回 t1。
func latestSnapshot(st *store.Store, date string) (string, *Snapshot) {
	for _, tier := range []string{"t3", "t1"} {
		hits, _ := filepath.Glob(filepath.Join(st.Root, "data", "trending", "*", "snap_"+tier+".json"))
		best := ""
		bestDay := ""
		for _, f := range hits {
			day := filepath.Base(filepath.Dir(f))
			if !reDateDir.MatchString(day) || day >= date {
				continue
			}
			if day > bestDay {
				best, bestDay = f, day
			}
		}
		if best != "" {
			raw, err := os.ReadFile(best)
			if err != nil {
				continue
			}
			var s Snapshot
			if json.Unmarshal(raw, &s) != nil {
				continue
			}
			return best, &s
		}
	}
	return "", nil
}

var reDateDir = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
func orDash(s string, empty bool) string {
	if empty {
		return "无"
	}
	return s
}
func ifStr(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
