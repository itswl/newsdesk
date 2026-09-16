package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/itswl/newsdesk2/internal/store"
)

// 标签在 URL 里要 percent-encode。Python 版是写死的编码串表，
// 这里直接编码——少一张会过期的表。
var doubanTags = []string{"热门", "最新", "豆瓣高分", "华语", "欧美", "日本", "韩国"}

type DoubanOpts struct {
	Tag   string
	N     int
	Sort  string // recommend | time | rank
	Pause time.Duration
}

type doubanSubject struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type doubanMeta struct {
	Date   string   `json:"date"`
	Tag    string   `json:"tag"`
	Sort   string   `json:"sort"`
	OK     []string `json:"ok"`
	Failed []string `json:"failed"`
}

// Douban 抓热门电影：先取列表，再逐部取详情。
// 产物与 Python 版一致：list.json / meta.json / detail/<id>.json
func Douban(ctx context.Context, c *Client, st *store.Store, date string, o DoubanOpts) error {
	if o.Sort == "" {
		o.Sort = "recommend"
	}
	if o.N <= 0 {
		o.N = 10
	}
	if o.Pause == 0 {
		o.Pause = 500 * time.Millisecond
	}
	dd := st.Data("douban", date)
	if err := st.MkdirAll(filepath.Join(dd, "detail")); err != nil {
		return err
	}

	listURL := fmt.Sprintf(
		"https://movie.douban.com/j/search_subjects?type=movie&tag=%s&sort=%s&page_limit=%d&page_start=0",
		url.QueryEscape(o.Tag), o.Sort, o.N)
	raw, err := c.Get(ctx, listURL, map[string]string{"Referer": "https://movie.douban.com/explore"})
	if err != nil {
		return fmt.Errorf("列表接口失败: %w", err)
	}
	var list struct {
		Subjects []doubanSubject `json:"subjects"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("列表解析失败: %w（%s）", err, snippet(raw))
	}
	if len(list.Subjects) == 0 {
		// 空列表当作失败上报，不要静默返回——否则接口改版后这个源会无声消失
		return fmt.Errorf("列表为空，接口可能已改版: %s", snippet(raw))
	}
	if err := store.WriteJSON(filepath.Join(dd, "list.json"), list.Subjects, true); err != nil {
		return err
	}
	fmt.Printf("列表 %d 部\n", len(list.Subjects))

	meta := doubanMeta{Date: date, Tag: o.Tag, Sort: o.Sort, OK: []string{}, Failed: []string{}}
	for _, s := range list.Subjects {
		d, err := doubanDetail(ctx, c, s.ID)
		if err != nil {
			meta.Failed = append(meta.Failed, s.ID)
			fmt.Printf("  FAIL %s (%v)\n", s.ID, err)
			sleep(ctx, o.Pause)
			continue
		}
		if err := store.WriteJSON(filepath.Join(dd, "detail", s.ID+".json"), d, false); err != nil {
			return err
		}
		meta.OK = append(meta.OK, s.ID)
		fmt.Printf("  OK   %s  %v  %v\n", s.ID, d["title"], ratingValue(d))
		sleep(ctx, o.Pause)
	}
	if err := store.WriteJSON(filepath.Join(dd, "meta.json"), meta, true); err != nil {
		return err
	}
	fmt.Printf("成功 %d，失败 %d -> %s\n", len(meta.OK), len(meta.Failed), st.RelTo(dd))
	return nil
}

func doubanDetail(ctx context.Context, c *Client, id string) (map[string]any, error) {
	body, err := c.Get(ctx, "https://m.douban.com/rexxar/api/v2/movie/"+id,
		map[string]string{"Referer": "https://movie.douban.com/subject/" + id + "/"})
	if err != nil {
		return nil, err
	}
	var d map[string]any
	if err := json.Unmarshal(StripControl(body), &d); err != nil {
		return nil, err
	}
	// 没有 title 就是脏数据。必须显式检查——写进去的话只会在报告里表现为
	// 一部没有片名的电影，不报错。
	if t, _ := d["title"].(string); t == "" {
		return nil, fmt.Errorf("详情缺少 title")
	}
	return d, nil
}

// ratingValue 取 rating.value。注意报告里的「评价数」是 rating.count，
// 不是 comment_count——后者是短评数，只有前者的三到六成，量级接近极易抄错。
func ratingValue(d map[string]any) any {
	r, _ := d["rating"].(map[string]any)
	if r == nil {
		return nil
	}
	return r["value"]
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
