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
const ALLOW = /^\/(?:[A-Za-z]{2}(?:-[A-Za-z]{2,4})?\/)?[A-Za-z0-9._-]+\.(?:html|xml|json)$/;

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

export function ttlFor(name: string): number {
  return TTL[name.split('/').pop()!] ?? TTL_DEFAULT;
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
  return 'text/html; charset=utf-8';
}
