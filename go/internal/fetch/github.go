package fetch

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// GitHub 是走 HTTPS 的 API 客户端，不依赖 gh CLI。
//
// 令牌是可选的，别让它变回必需：Trending 采集按约 50 次调用设计，装得进未认证的
// 60 次/小时预算。三条约束支撑这个数——
//   - 每个仓库只花 1 次 repos/{r}，它一次带回 stars/forks/license/created_at/
//     pushed_at/issues/subscribers/topics，别再拆成多次
//   - 停更判断走 commits.atom（github.com，不吃 API 配额）
//   - 追踪集合 = 今日榜 + 上一期榜，不是上一期快照的全部仓库
type GitHub struct {
	c     *Client
	token string
}

func NewGitHub(c *Client, token string) *GitHub {
	if strings.EqualFold(token, "none") || strings.EqualFold(token, "no") || strings.EqualFold(token, "off") {
		token = ""
	}
	return &GitHub{c: c, token: token}
}

func (g *GitHub) Authed() bool { return g.token != "" }

func (g *GitHub) api(ctx context.Context, path string, out any) (http.Header, error) {
	url := path
	if !strings.HasPrefix(path, "http") {
		url = "https://api.github.com/" + strings.TrimPrefix(path, "/")
	}
	h := map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if g.token != "" {
		h["Authorization"] = "Bearer " + g.token
	}
	body, hdr, err := g.c.GetWithHeaders(ctx, url, h)
	if err != nil {
		return hdr, err
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return hdr, fmt.Errorf("解析 %s: %w", path, err)
		}
	}
	return hdr, nil
}

type rateLimit struct {
	Rate struct {
		Remaining int `json:"remaining"`
	} `json:"rate"`
}

// Remaining 还剩多少配额。/rate_limit 端点本身不扣配额，可以随时查。
func (g *GitHub) Remaining(ctx context.Context) int {
	var rl rateLimit
	if _, err := g.api(ctx, "rate_limit", &rl); err != nil {
		return 0
	}
	return rl.Rate.Remaining
}

// Repo 是 repos/{owner}/{name} 里我们要用的字段。一次调用全拿到。
type Repo struct {
	Stars       int      `json:"stargazers_count"`
	Forks       int      `json:"forks_count"`
	PushedAt    string   `json:"pushed_at"`
	CreatedAt   string   `json:"created_at"`
	Branch      string   `json:"default_branch"`
	OpenIssues  int      `json:"open_issues_count"`
	Subscribers int      `json:"subscribers_count"`
	SizeKB      int      `json:"size"`
	Archived    bool     `json:"archived"`
	Fork        bool     `json:"fork"`
	Topics      []string `json:"topics"`
	Homepage    string   `json:"homepage"`
	Description string   `json:"description"`
	License     *struct {
		SPDX string `json:"spdx_id"`
	} `json:"license"`
	Owner struct {
		Type string `json:"type"`
	} `json:"owner"`
}

func (g *GitHub) Repo(ctx context.Context, repo string) (*Repo, error) {
	var r Repo
	if _, err := g.api(ctx, "repos/"+repo, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

type Release struct {
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		DownloadCount int `json:"download_count"`
	} `json:"assets"`
}

func (g *GitHub) Releases(ctx context.Context, repo string) ([]Release, error) {
	var rs []Release
	if _, err := g.api(ctx, "repos/"+repo+"/releases?per_page=100", &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

type atomFeed struct {
	Entries []struct {
		Updated string `xml:"updated"`
		Title   string `xml:"title"`
	} `xml:"entry"`
}

var reSpaces = regexp.MustCompile(`\s+`)

// HeadCommit 默认分支最新提交的时间与标题。
//
// 走 github.com 的 commits.atom 而不是 API——这条路不吃 API 限额，所以
// 「停更核查」在没有令牌时也能完整保留。
//
// ⚠️ 停更必须读 HEAD 而不是 pushed_at：后者会被 fork 推送、tag、CI 刷新，
// 会掩盖真实停更。两者差值大的要单列。
func (g *GitHub) HeadCommit(ctx context.Context, repo string) (date, msg string) {
	body, err := g.c.Get(ctx, "https://github.com/"+repo+"/commits.atom", nil)
	if err != nil {
		return "", ""
	}
	var f atomFeed
	if err := xml.Unmarshal(body, &f); err != nil || len(f.Entries) == 0 {
		return "", ""
	}
	e := f.Entries[0]
	t := strings.TrimSpace(reSpaces.ReplaceAllString(e.Title, " "))
	if len(t) > 120 {
		t = t[:120]
	}
	return e.Updated, t
}

var _ = time.Now
