package langs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/newsdesk2/internal/config"
	"github.com/itswl/newsdesk2/internal/i18n"
)

func cfgOf(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultIsSimplifiedOnly(t *testing.T) {
	got := Effective(cfgOf(t, "SITE_LANGS=\n"), "/tmp")
	if len(got) != 1 || got[0] != i18n.ZhCN {
		t.Fatalf("留空应只发简体，得到 %v", got)
	}
}

// 「列进来」不等于「开启」：英文要靠按量付费的翻译端点，端点没配好时
// 生成出来的是一堆空壳页面——链接在、内容不在，比没有这个语言更糟。
func TestEnglishNeedsEndpoint(t *testing.T) {
	c := cfgOf(t, "SITE_LANGS=zh-CN,zh-TW,en\n")
	got := Effective(c, "/tmp")
	for _, l := range got {
		if l == i18n.En {
			t.Fatal("没配翻译端点时英文不该生效")
		}
	}
	if len(got) != 2 {
		t.Fatalf("其余语言应照常发布，得到 %v", got)
	}
	// 原因要说清楚，不能只是消失
	if ex := Explain(c, "/tmp"); !strings.Contains(ex, "FALLBACK_BASE_URL") {
		t.Fatalf("诊断里应说明缺什么：\n%s", ex)
	}
}

func TestEnglishEnabledWhenConfigured(t *testing.T) {
	c := cfgOf(t, "SITE_LANGS=zh-CN,en\nFALLBACK_BASE_URL=https://x/v1\nFALLBACK_AUTH_TOKEN=tok\nFALLBACK_MODEL=m\n")
	got := Effective(c, "/tmp")
	if len(got) != 2 || got[1] != i18n.En {
		t.Fatalf("配齐了就该生效，得到 %v", got)
	}
}

// 繁体由简体现推，转换表编译进二进制，没有运行时依赖——
// 不会出现「装漏了一个包，线上就静默少一个语言」。
func TestTraditionalHasNoRuntimeDependency(t *testing.T) {
	got := Effective(cfgOf(t, "SITE_LANGS=zh-TW,zh-CN\n"), "/tmp")
	if len(got) != 2 || got[0] != i18n.ZhTW {
		t.Fatalf("繁体应无条件可用且可作默认，得到 %v", got)
	}
}

// 默认语言不可用时整站就没有根目录页面了，必须退回简体而不是留空。
func TestUnusableDefaultFallsBack(t *testing.T) {
	got := Effective(cfgOf(t, "SITE_LANGS=en\n"), "/tmp")
	if len(got) == 0 || got[0] != i18n.ZhCN {
		t.Fatalf("默认语言不可用应回退简体，得到 %v", got)
	}
}

func TestUnknownLangIsReported(t *testing.T) {
	ex := Explain(cfgOf(t, "SITE_LANGS=zh-CN,ja\n"), "/tmp")
	if !strings.Contains(ex, "ja") || !strings.Contains(ex, "不是支持的语言") {
		t.Fatalf("未知语言要说出来：\n%s", ex)
	}
}

func TestSubDirPutsDefaultAtRoot(t *testing.T) {
	all := []i18n.Lang{i18n.ZhCN, i18n.ZhTW, i18n.En}
	if got := SubDir("/site", i18n.ZhCN, all); got != "/site" {
		t.Fatalf("默认语言应在根目录，得到 %s", got)
	}
	if got := SubDir("/site", i18n.En, all); got != "/site/en" {
		t.Fatalf("其余语言应在子目录，得到 %s", got)
	}
}

func TestDuplicatesCollapse(t *testing.T) {
	if got := Effective(cfgOf(t, "SITE_LANGS=zh-CN,zh-CN,zh-TW\n"), "/tmp"); len(got) != 2 {
		t.Fatalf("重复的语言应去重，得到 %v", got)
	}
}
