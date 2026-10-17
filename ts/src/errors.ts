
/**
 * 错误页要给出 content-type 与可读内容。
 *
 * `new Response('Not Found', {status:404})` 看着够用，实际有两个问题：
 * Workers 会补 text/plain，而纯文本页在部分浏览器与内嵌 WebView 里会被当附件
 * 下载；用户看到的也只有两个英文单词，不知道该往哪走。
 */
export function errorPage(status: number, title: string, detail: string): Response {
  // 只有「返回首页」：页面上的语言切换器 2026-10-18 去掉了，错误页再留一排语言
  // 入口就是两套口径。三种语言仍各自发布、hreflang 互链照旧，只是不给入口。
  const links = '<a href="/">返回首页 · Home</a>';

  const body = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>${status} · ${esc(title)}</title><style>
:root{--mut:#8f96a3;color-scheme:dark light}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
 background:#16181d;color:#c9ced8;font:15px/1.8 -apple-system,BlinkMacSystemFont,"PingFang SC",
 "Hiragino Sans GB","Microsoft YaHei",system-ui,sans-serif;padding:24px}
@media (prefers-color-scheme:light){:root{--mut:#6b7280}body{background:#fbfbf9;color:#3d4148}}
.b{max-width:420px;text-align:center}
/* 大字 AA 是 3.0：.35 的透明字合成后只有 2.4，提到 .55 */
h1{margin:0 0 6px;font-size:40px;font-weight:600;letter-spacing:.02em;opacity:.55}
/* 辅助文字 12px 也要 4.5：浅色模式原来用深色的 #8f96a3（3.0），透明度截断更糟 */
p{margin:0 0 6px;color:var(--mut)}
.l{margin-top:22px;display:flex;gap:8px;justify-content:center;flex-wrap:wrap}
.l a{padding:8px 14px;border-radius:7px;background:#232730;color:#c9ced8;text-decoration:none;font-size:13.5px}
@media (prefers-color-scheme:light){.l a{background:#eceae5;color:#3d4148}}
.l a:hover{background:#2f343e}
</style></head><body><div class="b">
<h1>${status}</h1>
<p>${esc(title)}</p>
<p style="font-size:13px;color:var(--mut)">${esc(detail)}</p>
<div class="l">${links}</div>
</div></body></html>`;

  return new Response(body, {
    status,
    headers: {
      'content-type': 'text/html; charset=utf-8',
      'cache-control': 'public, max-age=60', // 别让错误页被长时间缓存
      'x-content-type-options': 'nosniff',
      'referrer-policy': 'no-referrer',
    },
  });
}

function esc(s: string): string {
  return s.replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]!));
}
