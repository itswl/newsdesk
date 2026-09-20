package oci

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Client struct {
	P    *Profile
	HTTP *http.Client
}

func New(p *Profile) *Client {
	return &Client{P: p, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, u string, body []byte, hdr map[string]string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if err := c.P.sign(req, body); err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s %s → %s: %s", method, redact(u), resp.Status, snip(raw))
	}
	return raw, nil
}

func redact(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Path
	}
	return u
}

func snip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Namespace 探测租户的 object storage namespace。配置里没写就自动取。
func (c *Client) Namespace(ctx context.Context) (string, error) {
	if c.P.Namespace != "" {
		return c.P.Namespace, nil
	}
	raw, err := c.do(ctx, http.MethodGet, c.P.endpoint()+"/n/", nil, nil)
	if err != nil {
		return "", err
	}
	var ns string
	if err := json.Unmarshal(raw, &ns); err != nil {
		return "", fmt.Errorf("解析 namespace: %w (%s)", err, snip(raw))
	}
	c.P.Namespace = ns
	return ns, nil
}

type Object struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	MD5  string `json:"md5"`
}

// List 列出桶里某前缀下的全部对象，自动翻页。
func (c *Client) List(ctx context.Context, bucket, prefix string) (map[string]Object, error) {
	ns, err := c.Namespace(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]Object{}
	start := ""
	for {
		q := url.Values{"fields": {"name,size,md5"}, "limit": {"1000"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if start != "" {
			q.Set("start", start)
		}
		u := fmt.Sprintf("%s/n/%s/b/%s/o?%s",
			c.P.endpoint(), url.PathEscape(ns), url.PathEscape(bucket), q.Encode())
		raw, err := c.do(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Objects   []Object `json:"objects"`
			NextStart string   `json:"nextStartWith"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("解析对象清单: %w", err)
		}
		for _, o := range page.Objects {
			out[o.Name] = o
		}
		if page.NextStart == "" {
			return out, nil
		}
		start = page.NextStart
	}
}

func (c *Client) Put(ctx context.Context, bucket, name string, body []byte, ctype string) error {
	ns, err := c.Namespace(ctx)
	if err != nil {
		return err
	}
	if ctype == "" {
		ctype = ContentType(name)
	}
	_, err = c.do(ctx, http.MethodPut,
		objectURL(c.P.endpoint(), ns, bucket, name), body, map[string]string{"Content-Type": ctype})
	return err
}

func (c *Client) Delete(ctx context.Context, bucket, name string) error {
	ns, err := c.Namespace(ctx)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, objectURL(c.P.endpoint(), ns, bucket, name), nil, nil)
	return err
}

// ContentType 按扩展名定 content-type。
//
// ⚠️ 不能退回无条件的 text/html：feed.xml 打错类型阅读器直接不认，
// 而这个错不会有任何报错，只是订阅者「再也收不到更新」。
func ContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".xml":
		return "application/xml; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".webmanifest":
		// 系统 mime 表里不一定有它（本机就没有），落到下面的兜底会变成
		// octet-stream——桶里的元数据是小事，但别让它和 Worker 报的对不上
		return "application/manifest+json; charset=utf-8"
	}
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

type SyncStat struct {
	Uploaded, Skipped, Deleted, Failed int
	Bytes                              int64
	Errs                               []string
}

type SyncOpts struct {
	Bucket   string
	Prefix   string                // 远端前缀，如 "site/"；空则直接放桶根
	Delete   bool                  // 远端多出来的对象是否删掉
	Parallel int                   // 并发上传数
	Keep     func(rel string) bool // 本地哪些文件要传，nil = 全传
	// DryRun 只算差异不动远端。算的是真的要传/要删哪些，不是「跳过这一步」——
	// 发布会删对象，看不到清单就敢按回车的话，删错了才发现就晚了。
	DryRun bool
	Log    func(format string, a ...any)
}

// Sync 把本地目录同步到桶里。
//
// 按 MD5 比对跳过没变的文件：一天四份报告，站点里绝大多数页面是不动的，
// 全量重传会把几千次 PUT 变成日常动作。
func (c *Client) Sync(ctx context.Context, dir string, o SyncOpts) (SyncStat, error) {
	var st SyncStat
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Parallel <= 0 {
		o.Parallel = 8
	}

	remote, err := c.List(ctx, o.Bucket, o.Prefix)
	if err != nil {
		return st, fmt.Errorf("列远端 %s: %w", o.Bucket, err)
	}

	type job struct {
		rel, path string
		sum       string
	}
	var jobs []job
	local := map[string]bool{}
	err = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(filepath.Base(p), ".") {
			return nil
		}
		if o.Keep != nil && !o.Keep(rel) {
			return nil
		}
		name := o.Prefix + rel
		local[name] = true
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		sum := md5.Sum(b)
		enc := base64.StdEncoding.EncodeToString(sum[:])
		if r, ok := remote[name]; ok && r.MD5 == enc && r.Size == fi.Size() {
			st.Skipped++
			return nil
		}
		jobs = append(jobs, job{rel: name, path: p, sum: enc})
		return nil
	})
	if err != nil {
		return st, fmt.Errorf("遍历 %s: %w", dir, err)
	}

	if o.DryRun {
		for _, j := range jobs {
			st.Uploaded++
			logf("  + %s", j.rel)
		}
		for _, name := range staleOf(remote, local) {
			if o.Delete {
				st.Deleted++
				logf("  - %s", name)
			}
		}
		return st, nil
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	ch := make(chan job)
	for i := 0; i < o.Parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				b, err := os.ReadFile(j.path)
				if err == nil {
					err = c.Put(ctx, o.Bucket, j.rel, b, "")
				}
				mu.Lock()
				if err != nil {
					st.Failed++
					if len(st.Errs) < 10 {
						st.Errs = append(st.Errs, fmt.Sprintf("%s: %v", j.rel, err))
					}
				} else {
					st.Uploaded++
					st.Bytes += int64(len(b))
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		select {
		case <-ctx.Done():
		case ch <- j:
		}
	}
	close(ch)
	wg.Wait()

	if o.Delete {
		for _, name := range staleOf(remote, local) {
			if err := c.Delete(ctx, o.Bucket, name); err != nil {
				st.Failed++
				if len(st.Errs) < 10 {
					st.Errs = append(st.Errs, fmt.Sprintf("删 %s: %v", name, err))
				}
				continue
			}
			st.Deleted++
			logf("  - %s", name)
		}
	}
	return st, nil
}

// Get 取回一个对象。
// staleOf 远端有、本地没有的对象。
func staleOf(remote map[string]Object, local map[string]bool) []string {
	var gone []string
	for name := range remote {
		if !local[name] {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	return gone
}

func (c *Client) Get(ctx context.Context, bucket, name string) ([]byte, error) {
	ns, err := c.Namespace(ctx)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodGet, objectURL(c.P.endpoint(), ns, bucket, name), nil, nil)
}

type PullStat struct {
	Files, Skipped, Failed int
	Bytes                  int64
	Errs                   []string
}

// Pull 把桶里某前缀下的对象拉回本地目录。
//
// 已存在且 MD5 一致的跳过——换机器恢复时大多数文件是对的，全量重下没意义。
func (c *Client) Pull(ctx context.Context, bucket, prefix, dir string) (PullStat, error) {
	var st PullStat
	objs, err := c.List(ctx, bucket, prefix)
	if err != nil {
		return st, err
	}
	names := make([]string, 0, len(objs))
	for n := range objs {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		rel := strings.TrimPrefix(name, prefix)
		// 对象名是远端给的，拼路径前必须挡住 ../ ——否则一个恶意或损坏的清单
		// 就能让恢复写到目录外面去。
		if rel == "" || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
			st.Failed++
			st.Errs = append(st.Errs, "跳过可疑对象名: "+name)
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if b, err := os.ReadFile(dst); err == nil {
			sum := md5.Sum(b)
			if base64.StdEncoding.EncodeToString(sum[:]) == objs[name].MD5 {
				st.Skipped++
				continue
			}
		}
		body, err := c.Get(ctx, bucket, name)
		if err != nil {
			st.Failed++
			if len(st.Errs) < 10 {
				st.Errs = append(st.Errs, fmt.Sprintf("%s: %v", name, err))
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return st, err
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return st, err
		}
		st.Files++
		st.Bytes += int64(len(body))
	}
	return st, nil
}
