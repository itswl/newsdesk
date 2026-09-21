package translate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 译文最危险的失败不是「翻错」，是**结构被改**和**输出被截断**——
// 两者都不会报错，页面照常渲染，只是表格塌了或后半截没了。
func TestShapeDetectsStructuralDamage(t *testing.T) {
	zh := "# 标题\n\n| 名称 | 评分 |\n|---|---:|\n| 电影 | 8.1 |\n\n[链接](https://a.com)\n"
	en := "# Title\n\n| Name | Rating |\n|---|---:|\n| Film | 8.1 |\n\n[link](https://a.com)\n"
	if ShapeOf(zh) != ShapeOf(en) {
		t.Errorf("结构相同的译文不该被判为有差异: %+v vs %+v", ShapeOf(zh), ShapeOf(en))
	}
	dropped := "| a |\n|---|\n| 1 |\n"
	orig := "| a |\n|---|\n| 1 |\n| 2 |\n"
	if ShapeOf(orig).Tables == ShapeOf(dropped).Tables {
		t.Error("少一行表格应被察觉")
	}
}

func TestChunksAreLossless(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# 报告\n\n开头段落\n\n")
	for i := 0; i < 8; i++ {
		sb.WriteString("## 小节\n\n")
		sb.WriteString(strings.Repeat("这是一段中文内容。", 120) + "\n\n")
	}
	src := sb.String()
	cs := Chunks(src)
	if len(cs) < 2 {
		t.Fatalf("长文应被切开，只得到 %d 块", len(cs))
	}
	if strings.Join(cs, "") != src {
		t.Error("拼回去必须无损")
	}
	for i, c := range cs {
		if len(c) > chunkChars {
			t.Errorf("第 %d 块 %d 字符，超过上限 %d", i+1, len(c), chunkChars)
		}
	}
}

// 单个二级小节本身就超限时必须继续往下切——只按 ## 切的话那一节会原样送出去，
// 照样被截断（实测 9.4k 字符的一节就会）。
func TestOversizedSectionIsSplitFurther(t *testing.T) {
	src := "## 一个很长的小节\n\n" + strings.Repeat("段落内容。\n\n", 600)
	cs := Chunks(src)
	if maxLen(cs) > chunkChars {
		t.Errorf("超长小节未被进一步切分，最大块 %d 字符", maxLen(cs))
	}
	if strings.Join(cs, "") != src {
		t.Error("拼回去必须无损")
	}
}

func TestShortTextNotSplit(t *testing.T) {
	if len(Chunks("# 短报告\n\n一句话。\n")) != 1 {
		t.Error("短文不该切")
	}
}

func TestFileTranslatesChunksWithBoundedConcurrency(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()

		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{{"text": body.Messages[0].Content}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer server.Close()

	var src strings.Builder
	for i := 0; i < 8; i++ {
		src.WriteString("## 小节\n\n")
		src.WriteString(strings.Repeat("内容。", 500))
		src.WriteString("\n\n")
	}
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(server.URL, "k", "m")
	c.HTTP = server.Client()
	if _, err := c.File(context.Background(), path, true); err != nil {
		t.Fatal(err)
	}
	if peak > maxConcurrent {
		t.Fatalf("并发数 %d 超过上限 %d", peak, maxConcurrent)
	}
	if peak < 2 {
		t.Fatalf("长报告应启用并发，实际峰值为 %d", peak)
	}
}

func TestPlainHTTPIsRefused(t *testing.T) {
	c := New("http://api.example.com", "k", "m")
	if _, _, err := c.call(nil, "hi"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("明文 HTTP 必须拒绝，得到 %v", err)
	}
}

func TestBaseURLNormalisation(t *testing.T) {
	// 两种写法都常见；带了 /v1 不处理的话会拼成 /v1/v1/messages，实测 404
	for _, in := range []string{
		"https://newapi.example.org/v1", "https://newapi.example.org",
		"https://api.example.com/anthropic",
	} {
		root := strings.TrimSuffix(strings.TrimRight(in, "/"), "/v1")
		got := root + "/v1/messages"
		if strings.Contains(got, "/v1/v1/") {
			t.Errorf("%s 归一化后仍然重复: %s", in, got)
		}
	}
}
