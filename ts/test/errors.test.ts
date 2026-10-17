import { test } from 'node:test';
import assert from 'node:assert/strict';
import { errorPage } from '../src/errors.ts';

// 纯文本 404 在部分浏览器与内嵌 WebView 里会被当附件下载
test('错误页是 HTML 而不是纯文本', async () => {
  const r = errorPage(404, '页面不存在', '细节');
  assert.equal(r.status, 404);
  assert.equal(r.headers.get('content-type'), 'text/html; charset=utf-8');
  assert.equal(r.headers.get('x-content-type-options'), 'nosniff');
  assert.match(await r.text(), /<!doctype html>/i);
});

// 页面上的语言切换器 2026-10-18 去掉了，错误页再留一排语言入口就是两套口径。
// 只留「返回首页」——它是唯一一个不会因为多语言而变的东西。
test('错误页只有返回首页，没有语言入口', async () => {
  const html = await errorPage(404, 't', 'd').text();
  assert.ok(html.includes('href="/"'), '应当有返回首页');
  assert.ok(!html.includes('/zh-TW/'), '不该再有繁体入口');
  assert.ok(!html.includes('/en/'), '不该再有英文入口');
});

test('错误页不被长时间缓存', () => {
  const r = errorPage(502, 't', 'd');
  assert.match(r.headers.get('cache-control') ?? '', /max-age=60/);
});
