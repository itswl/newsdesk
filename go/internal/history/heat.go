package history

import (
	"fmt"
	"regexp"
	"strconv"
)

// 摸摸鱼条目的「额外信息」各源意思不同：知乎、微博是此刻的热度（能升能降），虎扑是回复数、
// 豆瓣热话是参与数（累计值，只会涨），其余源是分类（爱范儿、B站）、商家（值得买）、
// 「50分钟前」这类发布时间（虎嗅），或者干脆空着。只有前两类能拿来比冷热。

var reHeat = regexp.MustCompile(`^\s*(\d+(?:\.\d+)?)\s*(万|亿)?\s*(回复|热度|讨论|参与)?\s*$`)

// heatValue 把「1374 万」「115.3万」「283回复」「1300」读成数；分类、商家、「50分钟前」读不出来。
func heatValue(extra string) (float64, bool) {
	m := reHeat.FindStringSubmatch(extra)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "万":
		v *= 1e4
	case "亿":
		v *= 1e8
	}
	return v, true
}

// instantHeat 热度值是「此刻有多热」的源：能升也能降，判得了升温、降温。
var instantHeat = map[string]bool{"知乎热榜": true, "微博热搜": true}

// heatTrend 比同一条目在前一期和今天的热度值。ok 为假表示有一头读不出数，比不了。
//
// 知乎、微博按 ±10% 判升温 / 降温 / 持平。累计值只会涨：涨 5% 以上算「还在涨」，否则
// 「没再涨」——说它「降温」是错的，它压根降不了。
func heatTrend(src, prev, today string) (change, trend string, ratio float64, ok bool) {
	p, ok1 := heatValue(prev)
	t, ok2 := heatValue(today)
	if !ok1 || !ok2 || p <= 0 {
		return "", "", 0, false
	}
	ratio = t / p
	change = fmt.Sprintf("%+.0f%%", (ratio-1)*100)
	switch {
	case instantHeat[src] && ratio >= 1.1:
		trend = "升温"
	case instantHeat[src] && ratio <= 0.9:
		trend = "降温"
	case instantHeat[src]:
		trend = "持平"
	case ratio >= 1.05:
		trend = "还在涨"
	default:
		trend = "没再涨"
	}
	return change, trend, ratio, true
}
