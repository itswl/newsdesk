package site

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"math"
	"strconv"
	"strings"
	"sync"
)

// 站点图标：构建时用标准库画出来。不提交二进制、不装任何图形工具。
//
// 为什么非得有位图：iOS 的 apple-touch-icon 只认 PNG，给 SVG 会被直接忽略，
// 系统转而拿页面截图当图标——「加到主屏后很丑」就是这么来的。所以矢量源和位图
// 都得有。两套几何各写一份必然会漂移，这里只定义一次：下面那些矩形都是画布
// 0..1 坐标，PNG 逐像素求值，SVG 用同一组数字生成。
//
// 图案是深蓝渐变底 + 白色卡片 + 三条横线，对应「每日简报」。纯几何、不带文字——
// 图标最小只有 32px，任何字形缩到那里都是一团糊。

type rgb struct{ r, g, b float64 }

var (
	iconBgTop    = rgb{61, 120, 238}  // #3d78ee
	iconBgBottom = rgb{22, 41, 94}    // #16295e
	iconCardCol  = rgb{255, 255, 255} // 卡片
	iconTitleCol = rgb{59, 123, 240}  // 站点 accent #3b7bf0，最上面那条
	iconLineCol  = rgb{178, 194, 220} // 别调更浅：主屏上图标只有 60px，浅了看成一团白
	iconShadow   = rgb{8, 18, 42}
)

// 几何量，都是画布边长的比例；横线的位置与宽度相对卡片自己。
const (
	iconCardW = 0.56
	iconCardH = 0.66
	iconCardR = 0.075
	iconBarH  = 0.056 // 横线高度，两端半圆
	iconPad   = 0.16  // 卡片左右内边距，相对卡片宽
)

var (
	iconBarW = [3]float64{0.42, 0.68, 0.54} // 相对卡片内宽
	iconBarY = [3]float64{0.22, 0.46, 0.70} // 横线顶端，相对卡片高
)

// maskable 那份要把内容缩小：Android 的自适应图标按自己的形状裁切，
// 只保证中心 80% 的圆可见，原尺寸的卡片四角会被切掉。
const iconMaskableScale = 0.82

// rect 是圆角矩形，坐标在画布 0..1。
type rect struct{ x, y, w, h, r float64 }

func (r rect) cx() float64 { return r.x + r.w/2 }
func (r rect) cy() float64 { return r.y + r.h/2 }

// sdRound 是圆角矩形的有符号距离：负在内部，正的在外部，绝对值≈到边缘的距离。
// 用距离场而不是「像素在不在矩形里」，是为了边缘能直接算出覆盖率——不必把图形
// 画大几倍再缩，抗锯齿是解析的。
func sdRound(px, py float64, r rect) float64 {
	qx := math.Abs(px-r.cx()) - (r.w/2 - r.r)
	qy := math.Abs(py-r.cy()) - (r.h/2 - r.r)
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r.r
}

// cardRect 居中卡片。scale 只缩卡片不缩画布，maskable 那份靠它留出裁切余量。
func cardRect(scale float64) rect {
	w, h := iconCardW*scale, iconCardH*scale
	return rect{x: 0.5 - w/2, y: 0.5 - h/2, w: w, h: h, r: iconCardR * scale}
}

// cardBars 卡片上的三条横线，随卡片一起缩放。
func cardBars(card rect, scale float64) []rect {
	pad := card.w * iconPad
	inner := card.w - 2*pad
	h := iconBarH * scale
	out := make([]rect, 0, len(iconBarW))
	for i, w := range iconBarW {
		out = append(out, rect{
			x: card.x + pad,
			y: card.y + card.h*iconBarY[i],
			w: inner * w,
			h: h,
			r: h / 2,
		})
	}
	return out
}

// canvas 是超采样后的画布。RGB 各一份 float64：一次混合就是一次乘加，
// 换成 uint8 会在大面积渐变上攒出条带。
type canvas struct {
	n  int
	px []float64
}

func newCanvas(n int) *canvas { return &canvas{n: n, px: make([]float64, n*n*3)} }

// blend 按覆盖率叠一层颜色，cov 同时充当这一层的 alpha。
func (c *canvas) blend(x, y int, col rgb, cov float64) {
	if cov <= 0 {
		return
	}
	if cov > 1 {
		cov = 1
	}
	i := (y*c.n + x) * 3
	c.px[i] += (col.r - c.px[i]) * cov
	c.px[i+1] += (col.g - c.px[i+1]) * cov
	c.px[i+2] += (col.b - c.px[i+2]) * cov
}

// each 遍历矩形外扩 margin（画布比例）之后的像素，给出像素中心坐标。
// 图形只在自己的包围盒里求值——整幅逐像素扫一遍会白白多算十几倍。
func (c *canvas) each(r rect, margin float64, fn func(x, y int, px, py float64)) {
	px0 := clampInt(int(math.Floor((r.x-margin)*float64(c.n)+0.5)), 0, c.n)
	px1 := clampInt(int(math.Floor((r.x+r.w+margin)*float64(c.n)+0.5)), 0, c.n)
	py0 := clampInt(int(math.Floor((r.y-margin)*float64(c.n)+0.5)), 0, c.n)
	py1 := clampInt(int(math.Floor((r.y+r.h+margin)*float64(c.n)+0.5)), 0, c.n)
	for y := py0; y < py1; y++ {
		cy := (float64(y) + 0.5) / float64(c.n)
		for x := px0; x < px1; x++ {
			fn(x, y, (float64(x)+0.5)/float64(c.n), cy)
		}
	}
}

// gradient 铺底色，自上而下插值。
func (c *canvas) gradient(top, bottom rgb) {
	for y := 0; y < c.n; y++ {
		t := 0.0
		if c.n > 1 {
			t = float64(y) / float64(c.n-1)
		}
		col := rgb{top.r + (bottom.r-top.r)*t, top.g + (bottom.g-top.g)*t, top.b + (bottom.b-top.b)*t}
		for x := 0; x < c.n; x++ {
			i := (y*c.n + x) * 3
			c.px[i], c.px[i+1], c.px[i+2] = col.r, col.g, col.b
		}
	}
}

// fill 填充圆角矩形。aa 是一个设备像素的宽度，SDF 给出的距离超过半个像素
// 就完全在内/在外，中间那一段按距离铺开——边缘因此是一像素宽的过渡带。
func (c *canvas) fill(r rect, col rgb, aa float64) {
	c.each(r, aa, func(x, y int, px, py float64) {
		c.blend(x, y, col, 0.5-sdRound(px, py, r)/aa)
	})
}

// shadow 卡片底下的软阴影：从矩形边缘往外按 smoothstep 衰减到 0。
// 不加它的话卡片像贴纸一样浮在渐变上，没有层次。
func (c *canvas) shadow(r rect, dy, blur, alpha, aa float64) {
	r.y += dy
	c.each(r, blur+aa, func(x, y int, px, py float64) {
		t := 1 - sdRound(px, py, r)/blur
		if t <= 0 {
			return
		}
		if t > 1 {
			t = 1
		}
		c.blend(x, y, iconShadow, alpha*t*t*(3-2*t))
	})
}

// image 盒式降采样回目标尺寸。SDF 已经给过一次抗锯齿，这次滤波把剩下的
// 采样噪声抹平——小尺寸图标上能看出差别。
func (c *canvas) image(ss int) *image.NRGBA {
	n := c.n / ss
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	k := float64(ss * ss)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var r, g, b float64
			for dy := 0; dy < ss; dy++ {
				row := (y*ss + dy) * c.n
				for dx := 0; dx < ss; dx++ {
					i := (row + x*ss + dx) * 3
					r += c.px[i]
					g += c.px[i+1]
					b += c.px[i+2]
				}
			}
			o := (y*n + x) * 4
			img.Pix[o] = clamp8(r / k)
			img.Pix[o+1] = clamp8(g / k)
			img.Pix[o+2] = clamp8(b / k)
			img.Pix[o+3] = 255 // 不透明：iOS 会把透明区填黑，看着像图标破了一块
		}
	}
	return img
}

// iconImage 画一张位图图标。
func iconImage(size int, scale float64) *image.NRGBA {
	ss := 3
	if size >= 256 {
		ss = 2 // 大图上超采样倍数可以低一档：同样的边缘质量，内存省一半
	}
	c := newCanvas(size * ss)
	c.gradient(iconBgTop, iconBgBottom)
	card := cardRect(scale)
	aa := 1 / float64(c.n)
	c.shadow(card, 0.012*scale, 0.02*scale, 0.30, aa)
	c.fill(card, iconCardCol, aa)
	for i, b := range cardBars(card, scale) {
		c.fill(b, barColor(i), aa)
	}
	return c.image(ss)
}

func barColor(i int) rgb {
	if i == 0 {
		return iconTitleCol
	}
	return iconLineCol
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		// 图是内存里刚画的，尺寸与像素都合法，编码失败只可能是代码写错了。
		// 返回空字节的话会安静地写出一个 0 字节图标文件，那更难查。
		panic("site: 编码图标 PNG: " + err.Error())
	}
	return buf.Bytes()
}

// iconSVG 用同一组几何量生成矢量版，给浏览器标签页和桌面书签用。
func iconSVG(title string) string {
	const vb = 512
	f := func(v float64) string { return strconv.FormatFloat(v*vb, 'f', 2, 64) }
	rectAttrs := func(r rect) string {
		return fmt.Sprintf(`x="%s" y="%s" width="%s" height="%s" rx="%s"`,
			f(r.x), f(r.y), f(r.w), f(r.h), f(r.r))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="%s">`,
		vb, vb, esc(title))
	fmt.Fprintf(&sb, `<title>%s</title>`, esc(title))
	fmt.Fprintf(&sb, `<defs><linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">`+
		`<stop offset="0" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient>`+
		`<filter id="sh" x="-25%%" y="-25%%" width="150%%" height="150%%">`+
		`<feDropShadow dx="0" dy="%s" stdDeviation="%s" flood-color="%s" flood-opacity="0.3"/>`+
		`</filter></defs>`,
		hexRGB(iconBgTop), hexRGB(iconBgBottom), f(0.012), f(0.010), hexRGB(iconShadow))
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="url(#bg)"/>`, vb, vb)
	card := cardRect(1)
	fmt.Fprintf(&sb, `<rect %s fill="%s" filter="url(#sh)"/>`, rectAttrs(card), hexRGB(iconCardCol))
	for i, b := range cardBars(card, 1) {
		fmt.Fprintf(&sb, `<rect %s fill="%s"/>`, rectAttrs(b), hexRGB(barColor(i)))
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

// faviconICO 把 32px 的 PNG 包成 ICO。
//
// 浏览器不问页面就会去要 /favicon.ico，没有的话控制台永远挂着一条 404。
// ICO 允许直接内嵌 PNG（现代浏览器都认），不必自己写 BMP 那套调色板。
func faviconICO(pngData []byte, size int) []byte {
	var b bytes.Buffer
	// 头 6 字节（保留位、类型=图标、数量=1）+ 目录项 16 字节 + 数据
	b.Write([]byte{0, 0, 1, 0, 1, 0, byte(size), byte(size), 0, 0, 1, 0, 32, 0})
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(pngData)))
	b.Write(n[:])
	binary.LittleEndian.PutUint32(n[:], 22)
	b.Write(n[:])
	b.Write(pngData)
	return b.Bytes()
}

var (
	iconOnce sync.Once
	iconPNGs map[string][]byte
)

// iconFiles 返回要写进站点根目录的图标。位图每次构建都画一遍太浪费
// （512 那张要算两百万个采样点），进程内缓存，三语共用。
//
// favicon.ico 与 icon-32 与 apple-touch-icon 分开列：前两个给浏览器标签页，
// 180 那张是 iOS 加到主屏用的尺寸，缺了它 iOS 就退回截图。
func iconFiles(title string) map[string][]byte {
	iconOnce.Do(func() {
		png32 := encodePNG(iconImage(32, 1))
		iconPNGs = map[string][]byte{
			"icon-32.png":           png32,
			"favicon.ico":           faviconICO(png32, 32),
			"apple-touch-icon.png":  encodePNG(iconImage(180, 1)),
			"icon-192.png":          encodePNG(iconImage(192, 1)),
			"icon-512.png":          encodePNG(iconImage(512, 1)),
			"icon-maskable-512.png": encodePNG(iconImage(512, iconMaskableScale)),
		}
	})
	out := make(map[string][]byte, len(iconPNGs)+1)
	for k, v := range iconPNGs {
		out[k] = v
	}
	out["icon.svg"] = []byte(iconSVG(title))
	return out
}

// ── 小工具 ──

func hexRGB(c rgb) string {
	return fmt.Sprintf("#%02x%02x%02x", clamp8(c.r), clamp8(c.g), clamp8(c.b))
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
