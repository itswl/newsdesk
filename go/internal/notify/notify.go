// Package notify 发告警。纯标准库，通用 webhook，不绑定任何具体服务。
//
// 为什么需要：这条流水线的失败闭环原本只写在报告和站点状态条里，而那**只有
// 已经打开页面的人**才看得见。整条停摆（机器没开、调度器挂了）不会有任何人
// 知道——最安全的失败方式同时也是最沉默的。
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UA 必须显式设。默认的 Go-http-client 会被 Cloudflare 按客户端签名封掉
// （实测某网关对 Python-urllib 的默认 UA 返回 403 error code 1010），
// 而告警端点多半也在 CDN 后面，告警发不出去这件事本身最难发现。
const UA = "newsdesk/2.0 (+https://github.com/itswl/newsdesk)"

type Notifier struct {
	Webhook string
	Payload string // 自定义 JSON 体模板，{{TEXT}} / {{TITLE}} 会被替换
	Client  *http.Client
}

func New(webhook, payload string) *Notifier {
	return &Notifier{Webhook: webhook, Payload: payload,
		Client: &http.Client{Timeout: 10 * time.Second}}
}

// Send 发一条。未配置时返回 nil（静默跳过）。
//
// ⚠️ 告警失败绝不能反过来弄坏流水线，所以调用方只记日志、不中断。
func (n *Notifier) Send(title, text string) error {
	if n == nil || n.Webhook == "" {
		return nil
	}
	body := title + "\n" + text
	var req *http.Request
	var err error
	// 两种形状自动区分：URL 里含 {{TEXT}} 走 GET（Bark / ntfy 这类），否则 POST JSON
	if strings.Contains(n.Webhook, "{{TEXT}}") {
		u := strings.ReplaceAll(n.Webhook, "{{TEXT}}", url.PathEscape(body))
		u = strings.ReplaceAll(u, "{{TITLE}}", url.PathEscape(title))
		req, err = http.NewRequest(http.MethodGet, u, nil)
	} else {
		var payload []byte
		if n.Payload != "" {
			// {{TEXT}} 落在 JSON 字符串内部，必须按 JSON 规则转义——
			// 正文里一个换行或引号就能把 payload 撑坏
			payload = []byte(strings.NewReplacer(
				"{{TEXT}}", jsonInner(body), "{{TITLE}}", jsonInner(title)).Replace(n.Payload))
		} else {
			payload, _ = json.Marshal(map[string]string{"title": title, "text": body})
		}
		req, err = http.NewRequest(http.MethodPost, n.Webhook, bytes.NewReader(payload))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return redact(err)
	}
	req.Header.Set("User-Agent", UA)
	resp, err := n.Client.Do(req)
	if err != nil {
		return redact(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, trim(raw))
	}
	return bodyOK(raw)
}

// redact 报错里的地址只留到主机名。
//
// *url.Error 会把完整地址带进报错，而 webhook 的路径就是令牌（飞书是 /hook/<令牌>），
// GET 形状的还带着整段正文。这条报错要进 scheduler.log：11-06 断网那次，飞书 webhook
// 的完整地址就这样写进了日志。
func redact(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	where := "（地址解析不了）"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		where = u.Scheme + "://" + u.Host
	}
	return fmt.Errorf("%s %s：%w", ue.Op, where, ue.Err)
}

// bodyOK 光看 HTTP 状态码不够。
//
// 飞书/Lark、钉钉、企业微信都是**错误也返回 200**，把失败写在 body 的
// code/errcode 里——实测给 Lark 发一个形状不对的 payload 拿到的是
// HTTP 200 + {"code":19002,"msg":"params error"}。只看状态码的话告警通道
// 早就断了却一直报「已发送」，而告警静默失效正是这套东西存在的理由要防的事。
//
// 非 JSON 的响应（Slack 返回纯文本 ok）一律按成功处理，不瞎猜。
func bodyOK(raw []byte) error {
	var d map[string]any
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	for _, k := range []string{"code", "errcode", "StatusCode"} {
		v, ok := d[k]
		if !ok {
			continue
		}
		if f, ok := v.(float64); ok && f != 0 {
			return fmt.Errorf("被服务端拒绝: %s", trim(raw))
		}
	}
	return nil
}

func jsonInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
