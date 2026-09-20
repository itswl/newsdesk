// Package leak 在发布前扫产出里有没有混进凭据。
//
// 为什么需要这一道：分析层是唯一接触外部不可信文本的环节（RSS 正文、仓库描述、
// 热榜标题），而它的产出会**公开发布**。沙箱禁网，令牌拿不出去，但「写进报告」
// 这条路径禁网拦不住——报告第二天就在公开站点上了。
//
// 这一层不依赖任何引擎内部行为，确定有效。
package leak

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Hit 只记位置和类型，不记内容——把凭据打进日志就自相矛盾了。
type Hit struct {
	File string
	Line int
	What string
}

// shapes 不依赖配置、按形状认的常见凭据。
// 配置里没写全、或模型从别处抄来的也能拦下。
var shapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"Anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{"OpenAI 风格 key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"Cloudflare token", regexp.MustCompile(`\bcf(?:ut|at|k)_[A-Za-z0-9_\-]{20,}\b`)},
	{"AWS key id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"Slack/飞书 webhook", regexp.MustCompile(`https://(?:hooks\.slack\.com|open\.(?:feishu|larksuite)\.com)/\S{20,}`)},
	{"私钥块", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// 只扫文本产出。图片之类里的字节序列碰巧命中没有意义。
// 扫的是「会公开发布、且内容来自模型或配置」的那几类文本。图标那种二进制不在此列；
// .webmanifest 是站点产出的一部分（webapp 清单），跟 .json 一样按文本扫。
var scanExt = map[string]bool{".md": true, ".html": true, ".xml": true, ".json": true,
	".txt": true, ".webmanifest": true}

// Scan 扫若干目录。secrets 是配置里的真实值（太短的不要——空值或 none 会命中一切）。
func Scan(dirs []string, secrets map[string]string) []Hit {
	var hits []Hit
	for _, base := range dirs {
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !scanExt[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
			for n := 1; sc.Scan(); n++ {
				line := sc.Text()
				for k, v := range secrets {
					if len(v) >= 12 && strings.Contains(line, v) {
						hits = append(hits, Hit{p, n, "config 里的 " + k})
					}
				}
				for _, s := range shapes {
					if s.re.MatchString(line) {
						hits = append(hits, Hit{p, n, s.name})
					}
				}
			}
			return nil
		})
	}
	return hits
}

// Report 打印并返回是否有命中。
func Report(root string, hits []Hit) bool {
	if len(hits) == 0 {
		fmt.Println("  ✓ 产出中未发现凭据")
		return false
	}
	fmt.Println("!! 产出里混进了凭据，拒绝发布：")
	for _, h := range hits {
		rel, err := filepath.Rel(root, h.File)
		if err != nil {
			rel = h.File
		}
		fmt.Printf("   %s:%d  %s\n", rel, h.Line, h.What)
	}
	fmt.Println("!! 先把这些内容从报告里删掉，确认凭据是否需要轮换，再重新发布。")
	return true
}
