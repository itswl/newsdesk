package fetch

import (
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
