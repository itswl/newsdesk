package fetch

import "testing"

// Qwen 走 Hugging Face 组织接口。私有仓库与空 id 要剔掉；createdAt 是带毫秒的
// ISO 串，必须解析得出，否则每一条都永远算不进时间窗——而那正是旧 feed 死了
// 一年没人发现的形状。
func TestParseHFModels(t *testing.T) {
	body := []byte(`[
	 {"id":"Qwen/Example-A","createdAt":"2026-09-20T08:46:47.000Z","private":false},
	 {"id":"Qwen/Secret","createdAt":"2026-09-21T00:00:00.000Z","private":true},
	 {"id":"","createdAt":"2026-09-19T00:00:00.000Z"}]`)
	items, err := parseHFModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("应只剩 1 条公开模型，得到 %d", len(items))
	}
	it := items[0]
	if it.Source != "Qwen" || it.Title != "Qwen/Example-A" || it.Link != "https://huggingface.co/Qwen/Example-A" {
		t.Errorf("条目字段不对: %+v", it)
	}
	if _, _, ok := normDate(it.Date); !ok {
		t.Errorf("createdAt %q 解析不出来，会永远进不了时间窗", it.Date)
	}
	for _, bad := range []string{`[]`, `<html>`} {
		if _, err := parseHFModels([]byte(bad)); err == nil {
			t.Errorf("%q 应报错而不是静默返回空", bad)
		}
	}
}
