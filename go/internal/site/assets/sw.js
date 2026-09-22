// Service Worker：晚高峰到边缘的线路可能几秒不回包甚至整段不通（实测），
// 没有它的话每次打开都得赌线路。本站页面的样式与脚本全部内联在 HTML 里，
// 所以「看过的页面」可以整份缓存，线路断着也照常可读。
//
// 策略按资源分三类：
//   页面（导航 / *.html）  网络优先：2.5 秒没回来就用缓存，网络结果随后写进缓存
//   图标 / 清单 / feed     先缓存后更新：这次秒开，下次打开就是新的
//   status.json / sw.js    不拦——监控的新鲜度、SW 自己的更新，都不能被缓存挡住
//
// CACHE 版本只在 SW 逻辑变化时升。内容新鲜度靠重验证，不靠清缓存——
// 那会让每天全量重下几十兆，正是要避免的事。
var CACHE = 'newsdesk-v1';
var PAGES = CACHE + '-pages';
var ASSETS = CACHE + '-assets';

self.addEventListener('install', function () {
  self.skipWaiting();
});

self.addEventListener('activate', function (e) {
  e.waitUntil((async function () {
    var keys = await caches.keys();
    await Promise.all(keys.filter(function (k) { return k !== PAGES && k !== ASSETS; })
      .map(function (k) { return caches.delete(k); }));
    await self.clients.claim();
  })());
});

function update(req, store) {
  return caches.open(store).then(function (cache) {
    return fetch(req).then(function (res) {
      if (res && res.ok) cache.put(req, res.clone());
      return res;
    });
  }).catch(function () { return null; });
}

// 页面：有缓存时给网络 2.5 秒，超时先给缓存的，网络回来再补进缓存；
// 没缓存只能等网络，等不到就给一张说明页，别把读者晾在白屏上。
async function pageResponse(req, upd) {
  var cache = await caches.open(PAGES);
  var have = await cache.match(req);
  var fresh = upd;
  if (have) {
    var t = new Promise(function (r) { setTimeout(function () { r(null); }, 2500); });
    var res = await Promise.race([fresh, t]);
    return res || have;
  }
  var res = await fresh;
  return res || offline();
}

// 图标 / 清单 / feed：先给缓存的，后台更新
async function assetResponse(req, upd) {
  var cache = await caches.open(ASSETS);
  var have = await cache.match(req);
  if (have) return have;
  return (await upd) || Response.error();
}

function offline() {
  return new Response('<!doctype html><html lang="zh-CN"><meta charset="utf-8">' +
    '<meta name="viewport" content="width=device-width,initial-scale=1">' +
    '<title>离线 · Offline</title><style>body{margin:0;min-height:100vh;display:flex;' +
    'align-items:center;justify-content:center;background:#16181d;color:#c9ced8;' +
    'font:15px/1.8 -apple-system,"PingFang SC",sans-serif;padding:24px;text-align:center}</style>' +
    '<p>线路暂时不通，这个页面之前没看过，暂无缓存。<br>之前看过的日期仍可从历史记录打开。</p>',
    { status: 503, headers: { 'content-type': 'text/html; charset=utf-8' } });
}

self.addEventListener('fetch', function (e) {
  var req = e.request;
  if (req.method !== 'GET') return;
  var url = new URL(req.url);
  if (url.origin !== location.origin) return;
  if (url.pathname.endsWith('/sw.js')) return;          // 自己的更新不走缓存
  if (url.pathname.endsWith('/status.json')) return;    // 监控读的是新鲜度
  var isPage = req.mode === 'navigate' || url.pathname.endsWith('.html');
  var upd = update(req, isPage ? PAGES : ASSETS);
  e.waitUntil(upd);                                     // 超时回退后，后台更新仍要活着写进缓存
  e.respondWith(isPage ? pageResponse(req, upd) : assetResponse(req, upd));
});
