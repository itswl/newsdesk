package oci

import (
	"context"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testProfile(t *testing.T) (*Profile, *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Profile{User: "ocid1.user.oc1..u", Tenancy: "ocid1.tenancy.oc1..t",
		Fingerprint: "aa:bb", Region: "us-phoenix-1", key: k}, k
}

func TestObjectURLKeepsSlashes(t *testing.T) {
	u := objectURL("https://x", "ns", "bucket", "2026-09-17/ai-news.html")
	if !strings.HasSuffix(u, "/o/2026-09-17/ai-news.html") {
		t.Fatalf("对象名里的 / 必须是路径分隔符，得到 %s", u)
	}
	// 空格之类仍要转义，否则请求非法
	if got := objectURL("https://x", "ns", "b", "a b/c.html"); !strings.Contains(got, "a%20b/c.html") {
		t.Fatalf("特殊字符没转义：%s", got)
	}
}

func TestSignGETHeaders(t *testing.T) {
	p, k := testProfile(t)
	req, _ := http.NewRequest(http.MethodGet, "https://objectstorage.us-phoenix-1.oraclecloud.com/n/", nil)
	if err := p.sign(req, nil); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, `headers="(request-target) host date"`) {
		t.Fatalf("GET 应只签三个头：%s", auth)
	}
	if req.Header.Get("X-Content-Sha256") != "" {
		t.Fatal("GET 不该带 x-content-sha256")
	}
	verify(t, k, req, []string{
		"(request-target): get /n/",
		"host: objectstorage.us-phoenix-1.oraclecloud.com",
		"date: " + req.Header.Get("Date"),
	})
}

// 有正文的方法要多签三个头，少签任何一个都是 401，
// 而错误信息只说 NotAuthenticated，不会告诉你少了哪个。
func TestSignPUTCoversBodyHeaders(t *testing.T) {
	p, k := testProfile(t)
	body := []byte("<html>hi</html>")
	req, _ := http.NewRequest(http.MethodPut,
		"https://objectstorage.us-phoenix-1.oraclecloud.com/n/ns/b/bk/o/a.html", nil)
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	if err := p.sign(req, body); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	for _, h := range []string{"x-content-sha256", "content-type", "content-length"} {
		if !strings.Contains(auth, h) {
			t.Fatalf("PUT 的签名头里缺 %s：%s", h, auth)
		}
	}
	sum := sha256.Sum256(body)
	if want := base64.StdEncoding.EncodeToString(sum[:]); req.Header.Get("X-Content-Sha256") != want {
		t.Fatalf("正文摘要不对")
	}
	verify(t, k, req, []string{
		"(request-target): put /n/ns/b/bk/o/a.html",
		"host: objectstorage.us-phoenix-1.oraclecloud.com",
		"date: " + req.Header.Get("Date"),
		"x-content-sha256: " + req.Header.Get("X-Content-Sha256"),
		"content-type: text/html; charset=utf-8",
		"content-length: 15",
	})
}

func TestSignIncludesQueryInRequestTarget(t *testing.T) {
	p, k := testProfile(t)
	req, _ := http.NewRequest(http.MethodGet,
		"https://objectstorage.us-phoenix-1.oraclecloud.com/n/ns/b/bk/o?limit=1000&prefix=site%2F", nil)
	if err := p.sign(req, nil); err != nil {
		t.Fatal(err)
	}
	verify(t, k, req, []string{
		"(request-target): get /n/ns/b/bk/o?limit=1000&prefix=site%2F",
		"host: objectstorage.us-phoenix-1.oraclecloud.com",
		"date: " + req.Header.Get("Date"),
	})
}

func verify(t *testing.T, k *rsa.PrivateKey, req *http.Request, lines []string) {
	t.Helper()
	m := regexp.MustCompile(`signature="([^"]+)"`).FindStringSubmatch(req.Header.Get("Authorization"))
	if m == nil {
		t.Fatal("Authorization 里没有 signature")
	}
	sig, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, d[:], sig); err != nil {
		t.Fatalf("签名串对不上，服务端会回 401 NotAuthenticated：\n%s", strings.Join(lines, "\n"))
	}
}

func TestContentTypeByExtension(t *testing.T) {
	// feed 打错类型阅读器直接不认，而且不会有任何报错——只是订阅者再也收不到更新
	for name, want := range map[string]string{
		"feed.xml": "application/xml; charset=utf-8", "a.html": "text/html; charset=utf-8",
		"status.json": "application/json; charset=utf-8", "x.md": "text/markdown; charset=utf-8",
	} {
		if got := ContentType(name); got != want {
			t.Errorf("%s → %s，期望 %s", name, got, want)
		}
	}
}

func TestLoadProfileRejectsEncryptedKey(t *testing.T) {
	dir := t.TempDir()
	kp := filepath.Join(dir, "key.pem")
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"},
		Bytes: []byte("x")}), 0o600)
	cp := filepath.Join(dir, "config")
	os.WriteFile(cp, []byte("[DEFAULT]\nuser=u\ntenancy=t\nfingerprint=f\nkey_file="+kp+"\nregion=r\n"), 0o600)
	_, err := LoadProfile(cp, "DEFAULT")
	if err == nil || !strings.Contains(err.Error(), "口令") {
		t.Fatalf("带口令的私钥无人值守跑不了，应当明确报出来，得到 %v", err)
	}
}

func TestLoadProfileReadsPKCS8(t *testing.T) {
	dir := t.TempDir()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	b, _ := x509.MarshalPKCS8PrivateKey(k)
	kp := filepath.Join(dir, "key.pem")
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), 0o600)
	cp := filepath.Join(dir, "config")
	os.WriteFile(cp, []byte("[X]\nuser=u\ntenancy=t\nfingerprint=f\nkey_file="+kp+"\nregion=us-phoenix-1\n"), 0o600)
	p, err := LoadProfile(cp, "X")
	if err != nil {
		t.Fatal(err)
	}
	if p.keyID() != "t/u/f" {
		t.Fatalf("keyId 拼错：%s", p.keyID())
	}
}

// Sync 的核心是「按 MD5 跳过没变的」：一天四份报告，站点里绝大多数页面不动，
// 全量重传会把几千次 PUT 变成日常动作。
func TestSyncSkipsUnchangedAndDeletesStale(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.html"), []byte("same"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.html"), []byte("changed"), 0o644)

	var puts, dels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o"):
			json.NewEncoder(w).Encode(map[string]any{"objects": []Object{
				{Name: "a.html", Size: 4, MD5: md5b64("same")},
				{Name: "b.html", Size: 3, MD5: md5b64("old")},
				{Name: "gone.html", Size: 1, MD5: md5b64("x")},
			}})
		case r.Method == http.MethodPut:
			puts = append(puts, strings.TrimPrefix(r.URL.Path, "/n/ns/b/bk/o/"))
		case r.Method == http.MethodDelete:
			dels = append(dels, strings.TrimPrefix(r.URL.Path, "/n/ns/b/bk/o/"))
		}
	}))
	defer srv.Close()

	p, _ := testProfile(t)
	u, _ := url.Parse(srv.URL)
	p.Endpoint, p.Namespace = u.String(), "ns"
	st, err := New(p).Sync(context.Background(), dir,
		SyncOpts{Bucket: "bk", Delete: true, Parallel: 1})
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped != 1 || st.Uploaded != 1 || st.Deleted != 1 || st.Failed != 0 {
		t.Fatalf("同步账不对：%+v（上传 %v，删除 %v）", st, puts, dels)
	}
	if len(puts) != 1 || puts[0] != "b.html" {
		t.Fatalf("只有变过的该重传，实际传了 %v", puts)
	}
	// 超窗的旧页面必须真删：桶列不出清单，但地址是能猜的
	if len(dels) != 1 || dels[0] != "gone.html" {
		t.Fatalf("远端多出来的对象该删掉，实际删了 %v", dels)
	}
}

func md5b64(s string) string {
	sum := md5.Sum([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// 对象名是远端给的，拼路径前必须挡住 ../ ——否则一个损坏的清单就能让恢复
// 写到目录外面去。
func TestPullRejectsPathTraversal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
			json.NewEncoder(w).Encode(map[string]any{"objects": []Object{
				{Name: "reports/../../evil.txt", Size: 1, MD5: md5b64("x")},
				{Name: "reports/ok.md", Size: 2, MD5: md5b64("hi")},
			}})
			return
		}
		w.Write([]byte("hi"))
	}))
	defer srv.Close()

	p, _ := testProfile(t)
	p.Endpoint, p.Namespace = srv.URL, "ns"
	dir := t.TempDir()
	st, err := New(p).Pull(context.Background(), "bk", "reports/", dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 1 || st.Failed != 1 {
		t.Fatalf("含 .. 的对象名必须被拒：%+v", st)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatal("写到目录外面去了")
	}
}

func TestPullSkipsIdenticalFiles(t *testing.T) {
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/o") {
			json.NewEncoder(w).Encode(map[string]any{"objects": []Object{
				{Name: "reports/a.md", Size: 4, MD5: md5b64("same")},
			}})
			return
		}
		gets++
		w.Write([]byte("same"))
	}))
	defer srv.Close()

	p, _ := testProfile(t)
	p.Endpoint, p.Namespace = srv.URL, "ns"
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("same"), 0o644)
	st, err := New(p).Pull(context.Background(), "bk", "reports/", dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped != 1 || st.Files != 0 || gets != 0 {
		t.Fatalf("内容一致的文件不该重下：%+v（下载 %d 次）", st, gets)
	}
}
