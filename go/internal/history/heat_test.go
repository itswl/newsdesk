package history

import "testing"

// 额外信息各源意思不同，只有读得出数的能比冷热；累计值只会涨，不能判「降温」。
func TestHeatTrend(t *testing.T) {
	for _, c := range []struct {
		src, prev, today, change, trend string
		ok                              bool
	}{
		{"知乎热榜", "100 万", "150 万", "+50%", "升温", true},
		{"微博热搜", "80.0万", "40.0万", "-50%", "降温", true},
		{"知乎热榜", "100 万", "105 万", "+5%", "持平", true},
		{"豆瓣热话", "1.4万", "2.8万", "+100%", "还在涨", true},
		{"虎扑步行街", "283回复", "290回复", "+2%", "没再涨", true},
		{"爱范儿", "产品", "产品", "", "", false},
		{"虎嗅", "50分钟前", "1小时前", "", "", false},
		{"豆瓣热话", "", "2.8万", "", "", false},
	} {
		change, trend, _, ok := heatTrend(c.src, c.prev, c.today)
		if ok != c.ok || change != c.change || trend != c.trend {
			t.Errorf("%s %q→%q：%q %q %v，想要 %q %q %v", c.src, c.prev, c.today, change, trend, ok, c.change, c.trend, c.ok)
		}
	}
}
