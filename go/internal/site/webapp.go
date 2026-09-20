package site

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/itswl/newsdesk/internal/i18n"
)

// 状态栏与启动闪屏的颜色，必须和 site.css 里两套主题的 --bg 一致：
// 对不上的话加到主屏后每次打开都会先闪一下另一种底色。
//
// 清单里只有一个 theme_color，取深色那份——:root 本身就是深色，浅色是覆盖上去的。
const (
	themeBgDark  = "#16181d"
	themeBgLight = "#fcfcfb"
)

// ⚠️ 对外能取到的文件类型每加一种，三处要一起改，少一处就静默失效：
//
//	产出      这里与 icon.go 写进 site/ 的文件
//	白名单    ts/src/paths.ts 的 ALLOW（不加 → Worker 直接 404，桶里有也取不到）
//	类型      ts/src/paths.ts 的 contentType（不加 → 按 text/html 送出去）
//
// 图标就是照这条走的第一批：png / svg / ico / webmanifest。

// writeAssets 写图标与 webapp 清单。
//
// 三语各写一份，不共用一套：清单里的名字要跟着语言走。页面里用相对路径引用
// （根目录与 /<lang>/ 下是同一份 HTML），所以文件也得各目录一份，否则 /en/ 下
// 的图标会指到上一级去。位图只在进程内画一次，这里只是多写几遍字节。
func (b *Builder) writeAssets() error {
	for name, data := range iconFiles(b.T("site.name")) {
		if err := os.WriteFile(filepath.Join(b.o.Out, name), data, 0o644); err != nil {
			return err
		}
	}
	return b.writeManifest()
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
}

type manifest struct {
	Name        string `json:"name"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Lang        string `json:"lang"`
	// start_url 相对清单自身解析：根目录那份落在 /，/en/ 那份落在 /en/。
	StartURL string `json:"start_url"`
	// scope 显式写 "/"。默认值是「清单所在目录」，那样英文版装出来的应用里点
	// 「简」会跳出应用回到浏览器——切语言是站内跳转，不该离开应用。
	Scope           string         `json:"scope"`
	Display         string         `json:"display"`
	BackgroundColor string         `json:"background_color"`
	ThemeColor      string         `json:"theme_color"`
	Icons           []manifestIcon `json:"icons"`
}

// writeManifest 生成 webapp 清单。
//
// Android 与桌面 Chrome 靠它决定加到主屏后叫什么、用什么图标、以什么形态打开；
// iOS 不读它，iOS 只认 <head> 里那几行 meta 和 apple-touch-icon，两边都要有。
func (b *Builder) writeManifest() error {
	m := manifest{
		Name:        b.T("site.name"),
		ShortName:   b.T("app.short"),
		Description: b.T("site.feed_sub"),
		Lang:        i18n.HTMLLang(b.o.Lang),
		StartURL:    ".",
		Scope:       "/",
		Display:     "standalone",
		// 启动闪屏的底色。清单里只有一个值，取深色——站点默认就是深色。
		BackgroundColor: themeBgDark,
		ThemeColor:      themeBgDark,
		Icons: []manifestIcon{
			{Src: "icon-192.png", Sizes: "192x192", Type: "image/png"},
			{Src: "icon-512.png", Sizes: "512x512", Type: "image/png"},
			// maskable 单独一份：内容缩到中心 80% 的圆里，Android 想怎么裁都不切到卡片
			{Src: "icon-maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
		},
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.o.Out, "manifest.webmanifest"), append(data, '\n'), 0o644)
}
