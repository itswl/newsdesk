// Package fetch 是采集层。所有联网都在这里——分析层跑在沙箱里，读不到凭据也出不了网。
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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

// NetDown 这个错误是不是根本没连上：解析不了域名、拨不通、超时——不是连上了但内容不对。
//
// 两者的处置相反：连上了但内容不对多半是接口改了，要人去看；没连上要么是对方挂了，
// 要么是本机断网（调度器用 Online 再分一次），等一等就好。11-06 晚上本机 Wi-Fi 掉了，
// momoyu 报的却是「接口不可用，候选路径见上，需人工确认」，把人往接口改版上引。
func NetDown(err error) bool {
	if err == nil {
		return false
	}
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || errors.As(err, &op) || os.IsTimeout(err) ||
		errors.Is(err, context.DeadlineExceeded)
}

// onlineProbes 判断本机能不能上网时访问的地址：国内外各挑大站，一个有回应就算能。
var onlineProbes = []string{"https://www.apple.com/", "https://www.baidu.com/", "https://github.com/"}

// Online 本机此刻能不能连上外网。
//
// 调度器靠它分辨「这一档失败是因为本机断网」：11-06 晚上 Wi-Fi 从 19:41 掉到 22:07，
// 21:45 那档 momoyu 三次重试在 14 分钟里用完、当天放弃，失败告警也发不出去。
//
// 走 HTTP（HEAD）而不是直接拨端口：跟采集走同一条路，代理环境变量照样生效——直连拨不通、
// 走代理能通的机器上，拨端口会把「能上网」误判成断网，而误判的代价是真失败被当成断网、
// 每 10 分钟重跑一次。回什么状态码都算能上网；ctx 管超时。
func Online(ctx context.Context) bool {
	hc := &http.Client{Transport: http.DefaultTransport}
	ok := make(chan bool, len(onlineProbes))
	for _, u := range onlineProbes {
		go func(u string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
			if err == nil {
				req.Header.Set("User-Agent", UA)
				var resp *http.Response
				if resp, err = hc.Do(req); err == nil {
					resp.Body.Close()
				}
			}
			ok <- err == nil
		}(u)
	}
	for range onlineProbes {
		if <-ok {
			return true
		}
	}
	return false
}
