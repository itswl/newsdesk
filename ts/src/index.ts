// 自有子域 → OCI 对象存储 的反向代理。
//
// 存在的理由有两个：
//  1. 对象存储没有「默认文档」概念，访问 / 不会返回 index.html，必须在这里补。
//  2. 桶在 us-phoenix-1，亚洲直连慢；走 Cloudflare 边缘缓存后只有回源那次慢。
//
// 桶是 ObjectReadWithoutList：能按 URL 取对象，列不出清单。Worker 也不转发
// 列表请求，保持这个边界。

import { origin, type Env } from './env.ts';
import { errorPage } from './errors.ts';
import { allowed, contentType, normalize, ttlFor } from './paths.ts';
import { tick } from './cron.ts';
import { route as pushRoute } from './push.ts';

export { evaluate, selfCheck, STALE_HOURS_DEFAULT } from './selfcheck.ts';
export { allowed, contentType, normalize, ttlFor } from './paths.ts';
export { errorPage } from './errors.ts';
export { buildPayload, redact, verdictOf } from './notify.ts';
export { newReports, snapshot } from './watch.ts';
export { b64uDecode, b64uEncode, encrypt, validSub, vapidAuth } from './push.ts';

export async function handle(request: Request, env: Env): Promise<Response> {
  // 推送的订阅/退订走 POST，必须排在下面那道「只收 GET/HEAD」之前。
  // 它不经过桶的白名单：这几个路径不是站点产出，不该被 normalize/allowed 过一遍。
  const u = new URL(request.url);
  if (u.pathname.startsWith('/push/')) return pushRoute(request, env, u);

  if (request.method !== 'GET' && request.method !== 'HEAD') {
    const r = errorPage(405, '方法不被允许 · Method not allowed', 'GET / HEAD only.', env);
    r.headers.set('Allow', 'GET, HEAD');
    return r;
  }

  const path = normalize(u.pathname);
  if (!allowed(path)) {
    return errorPage(404, '页面不存在 · Page not found',
      '这个地址不在本站的产出范围内。 This path is not served by this site.', env);
  }

  const name = path.slice(1);
  const ttl = ttlFor(name);
  const upstream = `${origin(env)}/${encodeURIComponent(name)}`;

  const res = await fetch(upstream, {
    method: request.method,
    cf: { cacheTtl: ttl, cacheEverything: true },
  });

  if (!res.ok) {
    return res.status === 404
      ? errorPage(404, '页面不存在 · Page not found',
          '这一天可能已超出保留期。 This day may be outside the retention window.', env)
      : errorPage(502, '上游暂时不可用 · Upstream unavailable',
          '对象存储没有正常响应，稍后再试。 Object storage did not respond; please retry.', env);
  }

  const h = new Headers(res.headers);
  h.set('content-type', contentType(name));
  // 客户端缓存不能比边缘长，否则给 status.json 设的短 TTL 被浏览器/监控端的
  // max-age 抵消掉——监控看到的永远是一分钟前的健康状态。
  h.set('cache-control', `public, max-age=${Math.min(60, ttl)}, s-maxage=${ttl}`);
  h.set('x-content-type-options', 'nosniff');
  h.set('referrer-policy', 'no-referrer');
  // 别把对象存储的内部标识透出去
  for (const k of ['x-amz-version-id', 'opc-request-id', 'opc-client-request-id']) h.delete(k);

  return new Response(res.body, { status: res.status, headers: h });
}

export default {
  fetch: handle,

  // 由 wrangler.toml 的 [triggers] 驱动。见 selfcheck.ts 里为什么这条必须在
  // Cloudflare 上跑、而不是在那台 Mac 上。一次取回 status.json，喂给停更自查
  // （飞书）与新简报推送（手机）两条线，见 cron.ts。
  scheduled(_event: ScheduledEvent, env: Env, ctx: ExecutionContext) {
    ctx.waitUntil(tick(env));
  },
};
