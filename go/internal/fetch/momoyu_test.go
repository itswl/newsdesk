package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"
)

// 显示与滞后都要按真实时刻算。
//
// ⚠️ 以前把时刻往后挪 8 小时再当 UTC 用：显示碰巧是北京时间，滞后却少算 8 小时。
// 2026-10-28 那档豆瓣热话 04:01Z 更新、北京时间 21:45 采集，实际滞后 9.7 小时，
// 算出来 1.7 小时，没过 6 小时的线，任务日志一个字没提。
func TestParseMomoyuTimeKeepsTheRealInstant(t *testing.T) {
	got, ok := parseMomoyuTime("2026-10-28T04:01:19.000Z")
	if !ok {
		t.Fatal("解析失败")
	}
	if s := got.Format("01-02 15:04"); s != "10-28 12:01" {
		t.Errorf("显示 %s，应为北京时间 10-28 12:01", s)
	}
	run := time.Date(2026, 10, 28, 21, 45, 0, 0, beijing)
	if lag := run.Sub(got).Hours(); lag < 9.7 || lag > 9.75 {
		t.Errorf("滞后 %.2fh，应为 9.73h", lag)
	}
	// 不带时区的写法按 UTC 解释
	if bare, ok := parseMomoyuTime("2026-10-28 04:01:19"); !ok || !bare.Equal(got.Truncate(time.Second)) {
		t.Errorf("不带时区的 %v 应与带 Z 的 %v 是同一时刻", bare, got)
	}
}

// parsed.txt 的表头直接带上距采集多久，模型照抄、不自己算。
// 2026-11-01 那档模型把「10-31 00:01」当成当天，豆瓣热话的 45.7 小时写成了 21.5 小时。
func TestMomoyuHeaderCarriesTheLag(t *testing.T) {
	run := time.Date(2026, 11, 1, 21, 45, 0, 0, beijing)
	cases := []struct {
		name, created string
		n             int
		want          string
	}{
		{"豆瓣热话", "2026-10-30T16:01:20.000Z", 40, "### [豆瓣热话] 更新:10-31 00:01（距采集 45.7 小时）(共40条)"},
		{"知乎热榜", "2026-11-01T13:30:00.000Z", 30, "### [知乎热榜] 更新:11-01 21:30（距采集 15 分钟）(共30条)"},
		// 接口的钟比本机快一点
		{"微博热搜", "2026-11-01T13:46:00.000Z", 50, "### [微博热搜] 更新:11-01 21:46（距采集 0 分钟）(共50条)"},
		// 解析不了就原样写，不写滞后
		{"某源", "不是时间", 3, "### [某源] 更新:不是时间 (共3条)"},
	}
	for _, c := range cases {
		ts, lag := momoyuStamp(c.created, run)
		if got := momoyuHeader(c.name, ts, lag, c.n); got != c.want {
			t.Errorf("表头\n  %s\n想要\n  %s", got, c.want)
		}
	}
}

// 没连上和连上了但内容不对要分开：前者等一等就好，后者多半是接口改了、要人去看。
// 11-06 本机断网，momoyu 报的却是「接口不可用，候选路径见上，需人工确认」。
func TestNetDownTellsNoConnectionFromBadContent(t *testing.T) {
	dns := &url.Error{Op: "Get", URL: "https://example.com/api", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}}}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{dns, true},
		{fmt.Errorf("包了一层：%w", dns), true},
		{context.DeadlineExceeded, true},
		{fmt.Errorf("HTTP 404: not found"), false},
		{errors.New("invalid character '<' looking for beginning of value"), false},
		{nil, false},
	} {
		if got := NetDown(c.err); got != c.want {
			t.Errorf("NetDown(%v) = %v，想要 %v", c.err, got, c.want)
		}
	}
}
