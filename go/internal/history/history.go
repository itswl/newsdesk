// Package history 跨期比对：把「今天 vs 前 N 天」的差异算成确定性的量。
//
// 为什么算在采集层而不是让模型自己翻历史：模型逐日读原始数据既贵又容易看漏；
// 更重要的是「新出现 / 已报过 / 连续第几天」这类判断必须是确定性的——
// 交给模型凭印象判断，写出来的「今日增量」就不可信了。
//
// 输出的字段名是 prompts 的契约，改名等于改 prompt。
package history

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/store"
)

// Window 回看一周。再长意义不大：话题很少真热过三四天，
// 拉到两周主要是把不更新的源的陈年条目翻出来，信号被噪声淹掉。
const Window = 7

// Cap 只带信号不带清单：全量条目在各源自己的文件里已经有了，
// 这里重复一遍既占上下文又没新信息。
const Cap = 12

var reDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// daysBefore 按日期倒序返回 date 之前的数据目录。
func daysBefore(st *store.Store, src, date string, n int) [][2]string {
	dirs, _ := filepath.Glob(filepath.Join(st.Root, "data", src, "*"))
	var days []string
	for _, d := range dirs {
		b := filepath.Base(d)
		if reDate.MatchString(b) && b < date {
			days = append(days, b)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	if len(days) > n {
		days = days[:n]
	}
	out := make([][2]string, len(days))
	for i, d := range days {
		out[i] = [2]string{d, filepath.Join(st.Root, "data", src, d)}
	}
	return out
}

func loadJSON(path string, v any) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

// NormLink 链接归一化，用于判重。
//
// ⚠️ 先小写再剥协议头：反过来的话 ^https?:// 是大小写敏感的，一条
// HTTPS://EXAMPLE.COM/... 剥不掉协议头，就和它的小写版本判成两条，
// 同一篇文章第二天会被当成新条目重新报一遍。
func NormLink(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimPrefix(s, "www.")
}

var rePunct = regexp.MustCompile(`[【】\[\]（）()「」“”"'：:，,。.！!？?、·\-—_\s]`)

// NormTitle 标题归一化：去标点与空白后比较。
func NormTitle(t string) string { return strings.ToLower(rePunct.ReplaceAllString(t, "")) }

// Build 算一个源的跨期比对并写出 history.json。
//
// w 是这一档的任务日志；nil 时写标准输出。
func Build(st *store.Store, src, date string, w io.Writer) error {
	if w == nil {
		w = os.Stdout
	}
	dd := filepath.Join(st.Root, "data", src, date)
	if fi, err := os.Stat(dd); err != nil || !fi.IsDir() {
		return fmt.Errorf("没有 %s 的 %s 数据", src, date)
	}
	var out map[string]any
	var err error
	switch src {
	case "ainews":
		out, err = ainews(st, date, dd)
	case "momoyu":
		out, err = momoyu(st, date, dd)
	case "douban":
		out, err = douban(st, date, dd)
	case "trending":
		out, err = trending(st, date, dd)
	default:
		return fmt.Errorf("未知源: %s", src)
	}
	if err != nil {
		return err
	}
	out["_window_days"] = Window
	out["_generated"] = time.Now().Format(time.RFC3339)
	if err := store.WriteJSON(filepath.Join(dd, "history.json"), out, true); err != nil {
		return err
	}
	key := map[string]string{"ainews": "new_count", "momoyu": "new_today_count",
		"douban": "history_days", "trending": "history_boards"}[src]
	fmt.Fprintf(w, "跨期比对 -> %s/history.json（回看 %d 天，%s=%v）\n",
		st.RelTo(dd), Window, key, out[key])
	return nil
}
