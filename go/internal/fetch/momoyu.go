package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/store"
)

const momoyuAPI = "https://momoyu.cc/api/hot/list?type=1"

type momoyuResp struct {
	Data []momoyuCat `json:"data"`
}
type momoyuCat struct {
	Name string         `json:"name"`
	Data []momoyuSource `json:"data"`
}
type momoyuSource struct {
	Name       string       `json:"name"`
	CreateTime string       `json:"create_time"`
	Data       []momoyuItem `json:"data"`
}
type momoyuItem struct {
	Title string `json:"title"`
	Extra string `json:"extra"`
}

// Momoyu 抓热榜并解析成给模型读的文本。
//
// 产物按**运行日期**归档，不按源数据自己的日期（见下文）。
func Momoyu(ctx context.Context, c *Client, st *store.Store, date string) error {
	raw, err := c.Get(ctx, momoyuAPI, map[string]string{"Referer": "https://momoyu.cc/"})
	var resp momoyuResp
	if err == nil {
		err = json.Unmarshal(raw, &resp)
	}
	if err != nil || len(resp.Data) == 0 {
		// 接口变更时给出候选路径，但不猜着用——猜错会静默产出错误数据
		cands := momoyuDiscover(ctx, c)
		fmt.Fprintf(os.Stderr, "主接口异常（%v），降级探测到的候选: %v\n", err, cands)
		return fmt.Errorf("momoyu 接口不可用，候选路径见上，需人工确认")
	}

	nSrc, nItem := 0, 0
	for _, cat := range resp.Data {
		nSrc += len(cat.Data)
		for _, s := range cat.Data {
			nItem += len(s.Data)
		}
	}
	fmt.Fprintf(c.w(), "分类 %d，源 %d，条目 %d\n", len(resp.Data), nSrc, nItem)

	// 目录按**运行日期**分，不按源数据自己的日期。
	//
	// 差别在午夜之后和源滞后时就会显出来：接口最新条目还停在昨天，按它建目录
	// 的话数据落进 data/momoyu/<昨天>/，而 prompt 里写的是今天的路径、
	// 跨期比对也按今天找——模型会去读一个不存在的目录然后自己编。
	// 源的滞后另有出口：下面的滞后源清单，以及报告结尾必须带的各源时间戳表。
	dd := st.Data("momoyu", date)
	if err := st.MkdirAll(dd); err != nil {
		return err
	}

	var lines []string
	type staleSrc struct {
		name, ts string
		lag      float64
	}
	var stale []staleSrc
	now := time.Now()
	for _, cat := range resp.Data {
		lines = append(lines, strings.Repeat("=", 70), "## 分类: "+cat.Name, "")
		for _, s := range cat.Data {
			ts := s.CreateTime
			lag := -1.0
			if t, ok := parseMomoyuTime(s.CreateTime); ok {
				ts = t.Format("01-02 15:04")
				lag = now.Sub(t).Hours()
			}
			// 更新时刻旧于 6 小时的源列出来。这只是接口给的时刻：掘金的会卡在几天前、
			// 内容照常在换，源新不新以跨期比对的 freshness（比上一期新进几条）为准
			if lag > 6 {
				stale = append(stale, staleSrc{s.Name, ts, lag})
			}
			lines = append(lines, fmt.Sprintf("### [%s] 更新:%s (共%d条)", s.Name, ts, len(s.Data)))
			for i, it := range s.Data {
				if i >= 15 {
					break
				}
				lines = append(lines, fmt.Sprintf("%2d. %s || %s", i+1, it.Title, it.Extra))
			}
			lines = append(lines, "")
		}
	}

	if err := os.WriteFile(filepath.Join(dd, "raw.json"), raw, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dd, "parsed.txt"),
		[]byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(c.w(), "-> %s/{raw.json, parsed.txt}\n", st.RelTo(dd))
	if len(stale) > 0 {
		var parts []string
		for _, s := range stale {
			parts = append(parts, fmt.Sprintf("%s(%s, 滞后%.1fh)", s.name, s.ts, s.lag))
		}
		fmt.Fprintln(c.w(), "⚠ 滞后源: "+strings.Join(parts, "; "))
	}
	return nil
}

// beijing 是摸摸鱼站点展示用的时区。固定偏移就够：没有夏令时，也不依赖本机的时区库。
var beijing = time.FixedZone("CST", 8*3600)

// parseMomoyuTime 接口给的是 UTC，站点按北京时间展示。
//
// 返回真实时刻、挂北京时区：Format 出来是北京时间，拿它算的滞后也是真的。
// ⚠️ 以前是把时刻往后挪 8 小时再当 UTC 用，显示碰巧对，滞后却少算 8 小时，
// 滞后 6–14 小时的源因此从不报（TestParseMomoyuTimeKeepsTheRealInstant）。
func parseMomoyuTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.In(beijing), true
		}
	}
	return time.Time{}, false
}

var reAssetJS = regexp.MustCompile(`/assets/index-[\w.-]+\.js`)
var reURLLiteral = regexp.MustCompile(`url:"([^"]+)"`)

// momoyuDiscover 接口路径变了就从打包产物里重新提取候选。
// 只报告不自动采用——猜错会静默产出错误数据。
func momoyuDiscover(ctx context.Context, c *Client) []string {
	home, err := c.Get(ctx, "https://momoyu.cc/", nil)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, js := range reAssetJS.FindAllString(string(home), -1) {
		body, err := c.Get(ctx, "https://momoyu.cc"+js, nil)
		if err != nil {
			continue
		}
		for _, m := range reURLLiteral.FindAllStringSubmatch(string(body), -1) {
			u := m[1]
			if strings.Contains(u, "hot") || strings.Contains(u, "list") {
				seen[u] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}
