package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这里唯一真正重要的性质：**发不出去必须被发现**。
// 告警通道静默失效比没有告警更糟——你以为有人会知道，其实没有。
func TestBodyOK(t *testing.T) {
	cases := []struct {
		body string
		ok   bool
	}{
		{`{"StatusCode":0,"code":0,"msg":"success"}`, true},
		{`{"errcode":0,"errmsg":"ok"}`, true},
		{`ok`, true}, // Slack 返回纯文本，不是 JSON——不瞎猜
		{``, true},   // 空体
		{`[1,2,3]`, true},
		{`{"code":"ok"}`, true}, // code 不是数字时不能崩
		// 以下都是 HTTP 200 但实际失败
		{`{"code":19002,"msg":"params error, msg_type need"}`, false},
		{`{"errcode":300001,"errmsg":"token is not exist"}`, false},
	}
	for _, c := range cases {
		err := bodyOK([]byte(c.body))
		if (err == nil) != c.ok {
			t.Errorf("bodyOK(%q) = %v，想要 ok=%v", c.body, err, c.ok)
		}
	}
}

func TestPayloadEscaping(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		got = string(b)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	n := New(srv.URL, `{"msg_type":"text","content":{"text":"{{TEXT}}"}}`)
	// 正文来自失败日志，里面有引号和换行是常态
	if err := n.Send("出错", "他说\"炸了\"\n第二行\t制表符"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(got), &d); err != nil {
		t.Fatalf("payload 被撑坏了，不是合法 JSON: %s", got)
	}
	inner := d["content"].(map[string]any)["text"].(string)
	if !strings.Contains(inner, "\"炸了\"") || !strings.Contains(inner, "\n") {
		t.Errorf("正文内容丢失: %q", inner)
	}
}

func TestErrorBodyWithHTTP200IsDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"code":19002,"msg":"params error"}`))
	}))
	defer srv.Close()
	if err := New(srv.URL, "").Send("x", "y"); err == nil {
		t.Error("HTTP 200 + 错误码必须判为失败")
	}
}

func TestUnconfiguredIsSilent(t *testing.T) {
	if err := New("", "").Send("x", "y"); err != nil {
		t.Errorf("未配置时应静默跳过，得到 %v", err)
	}
	var nilN *Notifier
	if err := nilN.Send("x", "y"); err != nil {
		t.Errorf("nil 接收者应安全: %v", err)
	}
}

func TestURLTemplateShape(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if err := New(srv.URL+"/bark/{{TEXT}}", "").Send("标题", "有 空格"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(path, "/bark/") || strings.Contains(path, "{{TEXT}}") {
		t.Errorf("URL 模板未替换: %q", path)
	}
}
