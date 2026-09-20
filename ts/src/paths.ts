// 路径处理：补默认文档 + 白名单。
//
// 桶是 ObjectReadWithoutList（能按 URL 取对象，列不出清单），Worker 也不转发
// 列表请求，保持这个边界。白名单则保证只取到本站自己的产出。

/** 对象存储没有「默认文档」概念，访问 / 不会返回 index.html，必须在这里补。 */
export function normalize(pathname: string): string {
  let p = pathname;
  if (p === '' || p === '/') return '/index.html';
  if (p.endsWith('/')) p += 'index.html';
  return p;
}

// 允许一层语言子目录（/en/2026-09-16.html）。只放行单层，不给目录遍历留口子：
// 段里不含 `/`，`..` 因为不带扩展名也进不来。
//
// ⚠️ 站点新增一种对外文件类型时，这里与 contentType、以及 Go 那边的产出
// （site.writeAssets 一带）要一起改。漏了这里的话图标在桶里躺着但取不到——
// 表现是「加到主屏还是老样子」，本地怎么看都看不出问题。
const ALLOW = /^\/(?:[A-Za-z]{2}(?:-[A-Za-z]{2,4})?\/)?[A-Za-z0-9._-]+\.(?:html|xml|json|webmanifest|svg|png|ico)$/;

export function allowed(path: string): boolean {
  return ALLOW.test(path);
}

// 站点每天更新四次，边缘缓存 5 分钟够用。
//
// ⚠️ status.json 必须短：它是给外部监控轮询的，缓存久了「陈旧」这件事本身
// 就被 CDN 藏起来了，监控看到的永远是若干分钟前的健康状态。
const TTL: Record<string, number> = {
  'index.html': 120, 'feed.xml': 120, 'status.json': 30, 'archive.html': 300,
};
const TTL_DEFAULT = 300;

// 图标与清单只有改设计时才变，可以比页面留久一点。
//
// ⚠️ 但别留太久：回源那一跳是缓存的，**404 会被一起缓存**——发布前探过一次的
// 路径，发布之后仍会返回 404，直到这一档过期。实测踩过：三个路径白等一小时。
// 10 分钟足够省掉重复回源，又让这类错误自己愈合。
const TTL_ICON = 600;
const ICON_EXT = /\.(?:png|svg|ico|webmanifest)$/;

export function ttlFor(name: string): number {
  const base = name.split('/').pop()!;
  if (ICON_EXT.test(base)) return TTL_ICON;
  return TTL[base] ?? TTL_DEFAULT;
}

/**
 * 按扩展名给 content-type。
 *
 * ⚠️ 不能无条件打 text/html：feed 打错类型阅读器直接不认，而且不会有任何
 * 报错——只是订阅者再也收不到更新。
 */
export function contentType(name: string): string {
  if (name.endsWith('.xml')) return 'application/atom+xml; charset=utf-8';
  if (name.endsWith('.json')) return 'application/json; charset=utf-8';
  // 清单必须是 manifest+json：打错类型 Chrome 直接忽略，而且不报错，只是
  // 「加到主屏」变成普通书签（没有图标、没有独立窗口）
  if (name.endsWith('.webmanifest')) return 'application/manifest+json; charset=utf-8';
  if (name.endsWith('.svg')) return 'image/svg+xml';
  if (name.endsWith('.png')) return 'image/png';
  if (name.endsWith('.ico')) return 'image/x-icon';
  return 'text/html; charset=utf-8';
}
