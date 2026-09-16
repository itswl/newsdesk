import { test } from 'node:test';
import assert from 'node:assert/strict';
import { errorPage } from '../src/errors.ts';

// 纯文本 404 在部分浏览器与内嵌 WebView 里会被当附件下载
test('错误页是 HTML 而不是纯文本', async () => {
  const r = errorPage(404, '页面不存在', '细节', { SITE_LANGS: 'zh-CN' });
  assert.equal(r.status, 404);
  assert.equal(r.headers.get('content-type'), 'text/html; charset=utf-8');
  assert.equal(r.headers.get('x-content-type-options'), 'nosniff');
  assert.match(await r.text(), /<!doctype html>/i);
});

// 只发简体的站点上，指向 /zh-TW/ 与 /en/ 的链接本身就是 404
test('语言入口跟着 SITE_LANGS 走，不写死三语', async () => {
  const one = await errorPage(404, 't', 'd', { SITE_LANGS: 'zh-CN' }).text();
  assert.ok(!one.includes('/zh-TW/'), '只发简体时不该出现繁体入口');
  assert.ok(!one.includes('/en/'), '只发简体时不该出现英文入口');
  assert.ok(one.includes('href="/"'));

  const three = await errorPage(404, 't', 'd', { SITE_LANGS: 'zh-CN,zh-TW,en' }).text();
  assert.ok(three.includes('href="/zh-TW/"'));
  assert.ok(three.includes('href="/en/"'));
  assert.ok(three.includes('href="/"'), '第一个语言在根目录');
});

test('SITE_LANGS 缺省时退回简体', async () => {
  const html = await errorPage(404, 't', 'd', {}).text();
  assert.ok(html.includes('href="/"'));
});

test('错误页不被长时间缓存', () => {
  const r = errorPage(502, 't', 'd', {});
  assert.match(r.headers.get('cache-control') ?? '', /max-age=60/);
});
