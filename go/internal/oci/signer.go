// Package oci 是 OCI 对象存储客户端，只用标准库。
//
// 不调 `oci` CLI：那是个 Python 包，装起来一百多兆、还要跟本机 Python 版本
// 较劲，而我们只用到「传一个对象、删一个对象、列一下清单」三件事。
// 认证是 draft-cavage HTTP 签名（RSA-SHA256），一百行就够。
package oci

import (
	"bufio"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Profile struct {
	User, Tenancy, Fingerprint, KeyFile, Region, Namespace string
	// Endpoint 覆盖按 region 拼出来的地址。留空走标准地址；
	// 专属 endpoint 和测试用得到。
	Endpoint string
	key      *rsa.PrivateKey
}

func (p *Profile) keyID() string {
	return p.Tenancy + "/" + p.User + "/" + p.Fingerprint
}

// LoadProfile 读 ~/.oci/config 的某个 profile。
func LoadProfile(path, name string) (*Profile, error) {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".oci", "config")
	}
	if name == "" {
		name = "DEFAULT"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读 OCI 配置 %s: %w", path, err)
	}
	defer f.Close()

	cur, kv := "", map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.Trim(line, "[]")
			continue
		}
		if cur != name {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			kv[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	if len(kv) == 0 {
		return nil, fmt.Errorf("OCI 配置 %s 里没有 [%s] 段", path, name)
	}
	p := &Profile{
		User: kv["user"], Tenancy: kv["tenancy"], Fingerprint: kv["fingerprint"],
		KeyFile: expand(kv["key_file"]), Region: kv["region"],
	}
	for k, v := range map[string]string{"user": p.User, "tenancy": p.Tenancy,
		"fingerprint": p.Fingerprint, "key_file": p.KeyFile, "region": p.Region} {
		if v == "" {
			return nil, fmt.Errorf("OCI 配置 [%s] 缺 %s", name, k)
		}
	}
	if p.key, err = loadKey(p.KeyFile); err != nil {
		return nil, err
	}
	return p, nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func loadKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读私钥 %s: %w", path, err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, fmt.Errorf("私钥 %s 不是 PEM 格式", path)
	}
	// 带口令的私钥这里解不开，也不该解——无人值守的流水线没地方输口令。
	// 报清楚比让签名在运行时莫名失败好。
	if strings.Contains(blk.Headers["Proc-Type"], "ENCRYPTED") ||
		blk.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, fmt.Errorf("私钥 %s 带口令，无人值守跑不了；请另生成一份无口令的 API 密钥", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥 %s: %w", path, err)
	}
	k, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥 %s 不是 RSA（OCI 只认 RSA）", path)
	}
	return k, nil
}

// sign 按 draft-cavage-http-signatures 给请求签名。
//
// 有正文的方法要多签三个头（x-content-sha256 / content-type / content-length），
// 少签任何一个都是 401，而错误信息只说 "NotAuthenticated"，不会告诉你少了哪个。
func (p *Profile) sign(req *http.Request, body []byte) error {
	if req.Header.Get("Date") == "" {
		req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}
	req.Header.Set("Host", req.URL.Host)

	headers := []string{"(request-target)", "host", "date"}
	switch req.Method {
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		sum := sha256.Sum256(body)
		req.Header.Set("X-Content-Sha256", base64.StdEncoding.EncodeToString(sum[:]))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		req.Header.Set("Content-Length", fmt.Sprint(len(body)))
		req.ContentLength = int64(len(body))
		headers = append(headers, "x-content-sha256", "content-type", "content-length")
	}

	var lines []string
	for _, h := range headers {
		switch h {
		case "(request-target)":
			target := req.URL.EscapedPath()
			if req.URL.RawQuery != "" {
				target += "?" + req.URL.RawQuery
			}
			lines = append(lines, "(request-target): "+strings.ToLower(req.Method)+" "+target)
		case "host":
			lines = append(lines, "host: "+req.URL.Host)
		default:
			lines = append(lines, h+": "+req.Header.Get(h))
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`Signature version="1",keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		p.keyID(), strings.Join(headers, " "), base64.StdEncoding.EncodeToString(sig)))
	return nil
}

func (p *Profile) endpoint() string {
	if p.Endpoint != "" {
		return strings.TrimRight(p.Endpoint, "/")
	}
	return "https://objectstorage." + p.Region + ".oraclecloud.com"
}

// objectURL 对象名里的 `/` 必须保留成路径分隔符，其余字符要转义。
// 直接 url.PathEscape 会把 `/` 变成 %2F，OCI 会当成一个名字里带斜杠的对象，
// 传上去能成功、但按正常路径取不回来。
func objectURL(base, ns, bucket, name string) string {
	parts := strings.Split(name, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/n/%s/b/%s/o/%s",
		base, url.PathEscape(ns), url.PathEscape(bucket), strings.Join(parts, "/"))
}
