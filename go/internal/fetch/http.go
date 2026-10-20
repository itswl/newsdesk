// Package fetch 是采集层。所有联网都在这里——分析层跑在沙箱里，读不到凭据也出不了网。
package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// UA 必须显式设。默认的 Go-http-client 和 Python-urllib 一样，会被 Cloudflare
// 按客户端签名封掉（实测某网关对 urllib 默认 UA 返回 403 error code 1010，
// 而那条错误长得像鉴权失败，很容易一路查到密钥上去）。
const UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

type Client struct {
	HTTP    *http.Client
	Retries int
	// Out 每个源的抓取结果写到哪；nil 时写标准输出。
	//
	// 流水线把它设成这一档的任务日志。以前直接 fmt.Printf：守护进程下标准输出只落在
	// scheduler.log、还没有任务前缀，单档日志里只剩一句「采集 用时 8s」，哪个源抓取
	// 失败、窗内几条，全看不到。
	Out io.Writer
}

func (c *Client) w() io.Writer {
	if c.Out != nil {
		return c.Out
	}
	return os.Stdout
}

func NewClient(timeout time.Duration) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout}, Retries: 1}
}

// Get 取一个 URL 的正文。失败返回错误而不是空串——返回空串的话，
// 调用方忘了判断就会把空内容当成「今天没有数据」静默吞掉。
func (c *Client) Get(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	var last error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", UA)
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if rerr != nil {
			last = rerr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
			// 4xx 重试没意义，除了 429
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return nil, last
			}
			continue
		}
		return body, nil
	}
	return nil, last
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// StripControl 去掉裸控制字符。豆瓣详情接口会在正文里夹带它们，不清掉 JSON 解析会失败。
func StripControl(b []byte) []byte {
	out := b[:0:0]
	for _, r := range string(b) {
		if r >= 32 || r == '\t' || r == '\n' {
			out = append(out, string(r)...)
		}
	}
	return out
}

// GetWithHeaders 与 Get 相同，另外返回响应头（GitHub 的 Link 分页要用）。
func (c *Client) GetWithHeaders(ctx context.Context, url string, headers map[string]string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", UA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.Header, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, resp.Header, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return body, resp.Header, nil
}
