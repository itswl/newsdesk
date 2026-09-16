import { test, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import { handle } from '../src/index.ts';

const ENV = {
  OCI_REGION: 'us-phoenix-1', OCI_NAMESPACE: 'ns', OCI_BUCKET: 'site',
  SITE_LANGS: 'zh-CN,zh-TW,en',
};

const realFetch = globalThis.fetch;
let seen: string[] = [];

/** 把回源换成假的，只看 Worker 自己的行为 */
function stub(reply: (url: string) => Response) {
  seen = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    seen.push(url);
    return reply(url);
  }) as typeof fetch;
}

beforeEach(() => stub(() => new Response('<html>ok</html>', { status: 200 })));
afterEach(() => { globalThis.fetch = realFetch; });

function get(path: string, method = 'GET') {
  return handle(new Request('https://news.example.com' + path, { method }), ENV);
}

test('根路径回源取 index.html', async () => {
  const r = await get('/');
  assert.equal(r.status, 200);
  assert.ok(seen[0]!.endsWith('/o/index.html'), seen[0]);
  assert.equal(r.headers.get('content-type'), 'text/html; charset=utf-8');
});

test('语言子目录正常回源', async () => {
  await get('/en/2026-09-17.html');
  assert.ok(seen[0]!.includes('en%2F2026-09-17.html'), seen[0]);
});

// 不在产出范围内的路径根本不该打到桶上
test('白名单外的路径不回源', async () => {
  const r = await get('/data/douban/list.json');
  assert.equal(r.status, 404);
  assert.equal(seen.length, 0, '不该产生回源请求');
});

test('feed 用 atom 类型而不是 html', async () => {
  const r = await get('/feed.xml');
  assert.equal(r.headers.get('content-type'), 'application/atom+xml; charset=utf-8');
});

// 客户端缓存不能比边缘长，否则给 status.json 设的短 TTL 被浏览器的 max-age 抵消
test('客户端缓存不超过边缘缓存', async () => {
  const cc = (await get('/status.json')).headers.get('cache-control') ?? '';
  const max = Number(/max-age=(\d+)/.exec(cc)?.[1]);
  const s = Number(/s-maxage=(\d+)/.exec(cc)?.[1]);
  assert.ok(max <= s, cc);
  assert.ok(s <= 30, `status.json 的边缘缓存要短：${cc}`);
});

test('上游 404 给出「可能超出保留期」的提示页', async () => {
  stub(() => new Response('', { status: 404 }));
  const r = await get('/2020-01-01.html');
  assert.equal(r.status, 404);
  assert.match(await r.text(), /保留期/);
});

test('上游故障归为 502 而不是原样透出', async () => {
  stub(() => new Response('oops', { status: 500 }));
  const r = await get('/index.html');
  assert.equal(r.status, 502);
});

test('只接受 GET 和 HEAD', async () => {
  const r = await get('/index.html', 'POST');
  assert.equal(r.status, 405);
  assert.equal(r.headers.get('Allow'), 'GET, HEAD');
  const h = await get('/index.html', 'HEAD');
  assert.equal(h.status, 200);
});

// 桶的内部标识不该透给访问者
test('抹掉上游的内部响应头', async () => {
  stub(() => new Response('x', { status: 200, headers: {
    'opc-request-id': 'abc', 'x-amz-version-id': 'v1',
  } }));
  const r = await get('/index.html');
  assert.equal(r.headers.get('opc-request-id'), null);
  assert.equal(r.headers.get('x-amz-version-id'), null);
  assert.equal(r.headers.get('x-content-type-options'), 'nosniff');
});
