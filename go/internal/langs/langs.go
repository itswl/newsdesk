// Package langs 决定实际发布哪些语言。
//
// 「列进配置」不等于「能发布」。英文要靠按量付费的翻译端点，端点没配好时
// 生成出来的是一堆空壳页面——链接在、内容不在，比没有这个语言更糟。
// 所以每种语言都有两个条件：**列进来** 且 **依赖齐备**，缺一不发。
//
// 缺依赖的语言被剔除并说明原因，其余照常——不因为英文没配好就把整条线停掉。
package langs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/itswl/newsdesk2/internal/config"
	"github.com/itswl/newsdesk2/internal/i18n"
)

type Status struct {
	Lang    i18n.Lang
	Listed  bool
	Ready   bool
	Reason  string // 不 Ready 的原因
	Default bool   // 放在站点根目录的那个
}

// deps 报告某语言的依赖是否齐备。
//
// zh-TW 的转换表是 go:embed 进二进制的，没有运行时依赖——Python 版那边
// 它要装 opencc，装漏了就静默少一个语言。这里不会。
func deps(l i18n.Lang, cfg *config.Config, root string) (bool, string) {
	switch l {
	case i18n.ZhCN:
		return true, ""
	case i18n.ZhTW:
		return true, ""
	case i18n.En:
		var miss []string
		for _, k := range []string{"FALLBACK_BASE_URL", "FALLBACK_AUTH_TOKEN", "FALLBACK_MODEL"} {
			if cfg.Get(k, "") == "" {
				miss = append(miss, k)
			}
		}
		if len(miss) > 0 {
			return false, "翻译端点未配置（缺 " + strings.Join(miss, " / ") + "）"
		}
		return true, ""
	}
	return false, "未知语言"
}

// Resolve 解析 SITE_LANGS。第一个是默认语言，放在站点根目录。
func Resolve(cfg *config.Config, root string) []Status {
	listed := cfg.List("SITE_LANGS")
	if len(listed) == 0 {
		listed = []string{string(i18n.ZhCN)}
	}
	seen := map[string]bool{}
	var out []Status
	for _, s := range listed {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		st := Status{Lang: i18n.Lang(s), Listed: true, Default: len(out) == 0}
		if !i18n.Valid(s) {
			st.Reason = fmt.Sprintf("不是支持的语言（可选 %s）", joinLangs(i18n.All))
		} else {
			st.Ready, st.Reason = deps(i18n.Lang(s), cfg, root)
		}
		out = append(out, st)
	}
	// 默认语言不可用时整站就没有根目录页面了，退回简体
	if len(out) == 0 || !out[0].Ready {
		out = append([]Status{{Lang: i18n.ZhCN, Listed: false, Ready: true, Default: true,
			Reason: "默认语言不可用，回退到简体"}}, out...)
		for i := 1; i < len(out); i++ {
			out[i].Default = false
		}
	}
	return out
}

// Effective 实际要生成的语言，顺序即发布顺序，[0] 是根目录那个。
func Effective(cfg *config.Config, root string) []i18n.Lang {
	var out []i18n.Lang
	for _, s := range Resolve(cfg, root) {
		if s.Ready {
			out = append(out, s.Lang)
		}
	}
	return out
}

// Explain 人可读的诊断，`newsdesk langs` 打它。
func Explain(cfg *config.Config, root string) string {
	var b strings.Builder
	b.WriteString("SITE_LANGS 解析结果：\n")
	for _, s := range Resolve(cfg, root) {
		mark, note := "✓", ""
		if !s.Ready {
			mark = "✗"
		}
		if s.Default {
			note = "（默认，放在站点根目录）"
		}
		if s.Reason != "" {
			note += " —— " + s.Reason
		}
		fmt.Fprintf(&b, "  %s %-6s %s\n", mark, s.Lang, note)
	}
	eff := Effective(cfg, root)
	fmt.Fprintf(&b, "实际生成：%s\n", joinLangs(eff))
	return b.String()
}

func joinLangs(ls []i18n.Lang) string {
	ss := make([]string, len(ls))
	for i, l := range ls {
		ss[i] = string(l)
	}
	return strings.Join(ss, ", ")
}

// SubDir 某语言在输出目录下的位置：默认语言在根，其余在 <lang>/ 下。
func SubDir(out string, l i18n.Lang, all []i18n.Lang) string {
	if len(all) > 0 && l == all[0] {
		return out
	}
	return filepath.Join(out, string(l))
}

// Exists 供调用方判断某语言目录是否已生成。
func Exists(p string) bool { _, err := os.Stat(p); return err == nil }
