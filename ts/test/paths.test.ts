import { test } from 'node:test';
import assert from 'node:assert/strict';
import { allowed, contentType, normalize, ttlFor } from '../src/paths.ts';

// 对象存储没有「默认文档」概念，访问 / 不会返回 index.html
test('根路径补上默认文档', () => {
  assert.equal(normalize('/'), '/index.html');
  assert.equal(normalize(''), '/index.html');
  assert.equal(normalize('/en/'), '/en/index.html');
});

test('白名单放行站点自己的产出', () => {
  for (const p of ['/index.html', '/2026-09-17.html', '/feed.xml', '/status.json',
                   '/en/2026-09-17.html', '/zh-TW/archive.html']) {
    assert.ok(allowed(p), `${p} 应放行`);
  }
});

test('白名单放行图标与 webapp 清单', () => {
  for (const p of ['/icon.svg', '/icon-192.png', '/apple-touch-icon.png', '/favicon.ico',
                   '/manifest.webmanifest', '/en/icon-512.png', '/zh-TW/manifest.webmanifest']) {
    assert.ok(allowed(p), `${p} 应放行`);
  }
});

test('白名单挡住目录遍历与桶里的其他前缀', () => {
  for (const p of ['/../secrets.json', '/a/b/c.html', '/reports/2026-09-17/ai-news.md',
                   '/index.php', '/.env', '/en/../../x.html', '/data/douban/list.json',
                   '/icon.js', '/en/sub/icon.png', '/apple-touch-icon.webp']) {
    assert.ok(!allowed(p), `${p} 不该放行`);
  }
});

// 只放行单层语言子目录，多层就是遍历
test('语言子目录只允许一层', () => {
  assert.ok(allowed('/en/a.html'));
  assert.ok(!allowed('/en/sub/a.html'));
});

// feed 打错类型阅读器直接不认，而且不会有任何报错——只是订阅者再也收不到更新
test('按扩展名给 content-type', () => {
  assert.equal(contentType('feed.xml'), 'application/atom+xml; charset=utf-8');
  assert.equal(contentType('status.json'), 'application/json; charset=utf-8');
  assert.equal(contentType('index.html'), 'text/html; charset=utf-8');
});

// 这几种打错类型同样是静默失败：清单类型不对 Chrome 不装成应用（只是普通书签），
// 图标类型不对则直接不显示，两者都不会报错、页面也照常打开。
test('图标与清单的 content-type 不能退回 text/html', () => {
  assert.equal(contentType('manifest.webmanifest'), 'application/manifest+json; charset=utf-8');
  assert.equal(contentType('icon.svg'), 'image/svg+xml');
  assert.equal(contentType('apple-touch-icon.png'), 'image/png');
  assert.equal(contentType('favicon.ico'), 'image/x-icon');
  assert.equal(contentType('en/manifest.webmanifest'), 'application/manifest+json; charset=utf-8');
});

// 每个页面都引用图标，站点每天重建四次，没必要每次回源；但也不能留太久——
// 回源跳会把 404 一起缓存住，发布后得等它过期才自愈
test('图标缓存比页面久一点，但别超过十分钟', () => {
  assert.ok(ttlFor('icon-192.png') > ttlFor('2026-09-17.html'));
  assert.ok(ttlFor('icon-192.png') <= 600, '太长的话误缓存的 404 要等很久才消失');
  assert.equal(ttlFor('en/apple-touch-icon.png'), ttlFor('apple-touch-icon.png'));
});

// status.json 是给外部监控轮询的，缓存久了「陈旧」本身就被 CDN 藏起来了
test('status.json 的缓存必须比别的短得多', () => {
  assert.ok(ttlFor('status.json') <= 30);
  assert.ok(ttlFor('status.json') < ttlFor('2026-09-17.html'));
  assert.equal(ttlFor('en/status.json'), ttlFor('status.json'));
});


// Service Worker 只在根目录产出一份，注册的是根路径；任意 .js 不能跟着沾光
test('sw.js 只在根放行，任意 .js 不放行', () => {
  assert.ok(allowed('/sw.js'));
  assert.ok(!allowed('/en/sw.js'), 'SW 只有一份，语言目录里没有');
  assert.ok(!allowed('/icon.js'));
  assert.ok(!allowed('/evil.js'));
  assert.ok(!allowed('/en/evil.js'));
});

// SW 的更新检查读 sw.js：类型打错浏览器拒绝装、且不报错；缓存留久了新 SW 上不了线
test('sw.js 的类型与缓存', () => {
  assert.equal(contentType('sw.js'), 'text/javascript; charset=utf-8');
  assert.ok(ttlFor('sw.js') <= 60, '新 SW 要能及时上线，误缓存的 404 也要快点自愈');
  assert.ok(ttlFor('sw.js') < ttlFor('2026-09-17.html'));
});
