import { test } from 'node:test';
import assert from 'node:assert/strict';
import { hkdfSync } from 'node:crypto';
import { b64uDecode, b64uEncode, encrypt, LOG_KEY, LOG_MAX, MAX_SUBS, push, recordPush, route, sendTo, validSub, vapidAuth, WELCOME_LOG_KEY } from '../src/push.ts';
import type { PushLogEntry } from '../src/push.ts';
import type { Env } from '../src/env.ts';
import { envOf, FakeKV, subBody, vapidEnv, withFetch } from './fixtures.ts';

const u8 = (v: ArrayBuffer | Uint8Array) => v instanceof Uint8Array ? v : new Uint8Array(v);

/** 用 RFC 8291 §5 给的一对密钥拼出 CryptoKeyPair：私钥只给了裸标量，只能走 JWK */
async function asKeyPair(asPrivate: string, asPublic: string): Promise<CryptoKeyPair> {
  const pub = b64uDecode(asPublic);
  const jwk = {
    kty: 'EC', crv: 'P-256', d: asPrivate,
    x: b64uEncode(pub.slice(1, 33)), y: b64uEncode(pub.slice(33, 65)), ext: true,
  };
  const privateKey = await crypto.subtle.importKey('jwk', jwk as JsonWebKey,
    { name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']);
  const publicKey = await crypto.subtle.importKey('raw', pub,
    { name: 'ECDH', namedCurve: 'P-256' }, true, []);
  return { privateKey, publicKey };
}

/**
 * RFC 8291 §5 的官方测试向量，逐字节比对。
 *
 * 这条是整块加密唯一真正的锚：派生链里任何一处顺序或常量写错——尤其 key_info 里
 * 「先接收方公钥、后发送方公钥」——都不会报错，推送服务照样回 201，手机上永远
 * 什么都不出现。自己和自己对拍是发现不了的，必须拿规范的向量压。
 */
test('RFC 8291 测试向量逐字节相符', async () => {
  const v = {
    plaintext: 'V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24',
    uaPublic: 'BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4',
    authSecret: 'BTBZMqHH6r4Tts7J_aSIgg',
    asPublic: 'BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8',
    asPrivate: 'yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw',
    salt: 'DGv6ra1nlYgDCS1FRnbzlw',
    body: 'DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLoc'
      + 'InmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLV'
      + 'WGNWQexSgSxsj_Qulcy4a-fN',
  };
  // 明文就是那句众所周知的西瓜
  assert.equal(new TextDecoder().decode(b64uDecode(v.plaintext)),
    'When I grow up, I want to be a watermelon');

  const got = await encrypt(b64uDecode(v.plaintext), b64uDecode(v.uaPublic),
    b64uDecode(v.authSecret), b64uDecode(v.salt), await asKeyPair(v.asPrivate, v.asPublic));
  assert.equal(b64uEncode(got), v.body);
});

/** 报文头的形状：salt(16) | rs(4, 大端) | idlen(1) | 发送方公钥(65) | 密文 */
test('报文头按 RFC 8188 分段', async () => {
  const pair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
  const ua = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
  const uaPub = u8(await crypto.subtle.exportKey('raw', (ua as CryptoKeyPair).publicKey) as unknown as ArrayBuffer);
  const salt = new Uint8Array(16).fill(7);
  const out = await encrypt(new TextEncoder().encode('hi'), uaPub, new Uint8Array(16).fill(9),
    salt, pair as CryptoKeyPair);

  assert.deepEqual(out.slice(0, 16), salt);
  assert.equal(new DataView(out.buffer, out.byteOffset).getUint32(16), 4096, 'rs 应为 4096');
  assert.equal(out[20], 65, 'idlen 应为未压缩点的 65');
  assert.equal(out[21], 0x04, '发送方公钥应是未压缩点，首字节 0x04');
  // 明文 2 + 分隔符 1 + GCM tag 16 = 19
  assert.equal(out.length, 16 + 4 + 1 + 65 + 19);
});

/** 派生链拿 Node 自带的 HKDF 独立算一遍，确认不是我的 HMAC 拼装碰巧自洽 */
test('密钥派生与 Node 的 HKDF 一致', async () => {
  const v = {
    authSecret: 'BTBZMqHH6r4Tts7J_aSIgg', salt: 'DGv6ra1nlYgDCS1FRnbzlw',
    ecdh: 'kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs',
    uaPublic: 'BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4',
    asPublic: 'BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8',
    ikm: 'S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg',
    cek: 'oIhVW04MRdy2XN9CiKLxTg', nonce: '4h_95klXJ5E_qnoN',
  };
  const keyInfo = Buffer.concat([Buffer.from('WebPush: info'), Buffer.from([0]),
    b64uDecode(v.uaPublic), b64uDecode(v.asPublic)]);
  const ikm = hkdfSync('sha256', b64uDecode(v.ecdh), b64uDecode(v.authSecret), keyInfo, 32);
  assert.equal(b64uEncode(new Uint8Array(ikm)), v.ikm, 'IKM 与向量不符');

  const cek = hkdfSync('sha256', new Uint8Array(ikm), b64uDecode(v.salt),
    Buffer.concat([Buffer.from('Content-Encoding: aes128gcm'), Buffer.from([0])]), 16);
  const nonce = hkdfSync('sha256', new Uint8Array(ikm), b64uDecode(v.salt),
    Buffer.concat([Buffer.from('Content-Encoding: nonce'), Buffer.from([0])]), 12);
  assert.equal(b64uEncode(new Uint8Array(cek)), v.cek);
  assert.equal(b64uEncode(new Uint8Array(nonce)), v.nonce);
});

test('载荷超过单记录上限要报错，不能悄悄截断', async () => {
  const pair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
  const ua = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
  const uaPub = u8(await crypto.subtle.exportKey('raw', (ua as CryptoKeyPair).publicKey) as unknown as ArrayBuffer);
  await assert.rejects(
    () => encrypt(new Uint8Array(4080), uaPub, new Uint8Array(16), new Uint8Array(16), pair as CryptoKeyPair),
    /超过单记录上限/);
});

/** VAPID：签名要能被公钥验回来，aud 是推送服务的源，exp 必须在 24 小时内 */
test('VAPID 头可被公钥验证且声明正确', async () => {
  const kp = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true,
    ['sign', 'verify']) as CryptoKeyPair;
  const env = {
    PUSH_VAPID_PRIVATE_KEY: b64uEncode(await crypto.subtle.exportKey('pkcs8', kp.privateKey) as unknown as ArrayBuffer),
    PUSH_VAPID_PUBLIC: b64uEncode(await crypto.subtle.exportKey('raw', kp.publicKey) as unknown as ArrayBuffer),
    PUSH_SUB: 'mailto:someone@example.com',
  } as Env;

  const now = 1790000000000;
  const auth = await vapidAuth(env, 'https://web.push.apple.com/abc/def?x=1', now);
  const m = /^vapid t=([\w-]+)\.([\w-]+)\.([\w-]+), k=([\w-]+)$/.exec(auth);
  assert.ok(m, `头的形状不对: ${auth}`);
  const [, h, c, sig, k] = m!;

  assert.deepEqual(JSON.parse(new TextDecoder().decode(b64uDecode(h!))), { typ: 'JWT', alg: 'ES256' });
  const claims = JSON.parse(new TextDecoder().decode(b64uDecode(c!)));
  assert.equal(claims.aud, 'https://web.push.apple.com', 'aud 只能是源，不带路径');
  assert.equal(claims.sub, 'mailto:someone@example.com');
  assert.ok(claims.exp > now / 1000 && claims.exp <= now / 1000 + 24 * 3600, 'exp 必须在 24 小时内');
  assert.equal(k, env.PUSH_VAPID_PUBLIC, 'k 必须是公钥本身');

  const ok = await crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, kp.publicKey,
    b64uDecode(sig!), new TextEncoder().encode(`${h}.${c}`));
  assert.ok(ok, '签名验不过——投递会被推送服务以 401 拒绝');
});

test('订阅形状校验', () => {
  const good = {
    endpoint: 'https://web.push.apple.com/abc',
    p256dh: b64uEncode(new Uint8Array(65).fill(4)),
    auth: b64uEncode(new Uint8Array(16).fill(1)),
  };
  assert.ok(validSub(good));
  assert.ok(!validSub({ ...good, endpoint: 'http://x.test/a' }), '明文 HTTP 不收');
  assert.ok(!validSub({ ...good, p256dh: b64uEncode(new Uint8Array(64)) }), 'p256dh 必须 65 字节');
  assert.ok(!validSub({ ...good, auth: b64uEncode(new Uint8Array(12)) }), 'auth 必须 16 字节');
  assert.ok(!validSub({ endpoint: good.endpoint }), '缺字段不收');
  assert.ok(!validSub(null));
});

async function envWithKV(): Promise<{ env: Env; kv: FakeKV }> {
  const kv = new FakeKV();
  return { env: await vapidEnv(kv), kv };
}

function call(env: Env, path: string, body: unknown, method = 'POST'): Promise<Response> {
  const url = new URL('https://news.example.com' + path);
  const init = method === 'POST' ? { method, body: JSON.stringify(body) } : { method };
  return route(new Request(url, init), env, url);
}

test('订阅会落库，并立刻回推一条当场验证链路', async () => {
  const { env, kv } = await envWithKV();
  const calls: string[] = [];
  const orig = globalThis.fetch;
  globalThis.fetch = (async (u: string) => { calls.push(String(u)); return new Response(null, { status: 201 }); }) as typeof fetch;
  try {
    const res = await call(env, '/push/subscribe', subBody('https://web.push.apple.com/one'));
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), { ok: true, welcome: 'ok' });
  } finally { globalThis.fetch = orig; }
  assert.equal([...kv.m.keys()].filter(k => k.startsWith('sub:')).length, 1);
  assert.deepEqual(calls, ['https://web.push.apple.com/one'], '应当场向推送服务投递一次');
});

test('推送服务回 410 就把订阅清掉', async () => {
  const { env, kv } = await envWithKV();
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 410 })) as typeof fetch;
  try {
    const res = await call(env, '/push/subscribe', subBody('https://web.push.apple.com/dead'));
    assert.deepEqual(await res.json(), { ok: true, welcome: 'gone' });
  } finally { globalThis.fetch = orig; }
  assert.equal([...kv.m.keys()].filter(k => k.startsWith('sub:')).length, 0, '失效订阅不该留在库里');
});

test('退订与形状不对的请求', async () => {
  const { env, kv } = await envWithKV();
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 201 })) as typeof fetch;
  try {
    await call(env, '/push/subscribe', subBody('https://web.push.apple.com/two'));
    assert.equal([...kv.m.keys()].filter(k => k.startsWith('sub:')).length, 1);
    const res = await call(env, '/push/unsubscribe', { endpoint: 'https://web.push.apple.com/two' });
    assert.equal(res.status, 200);
    assert.equal([...kv.m.keys()].filter(k => k.startsWith('sub:')).length, 0);

    assert.equal((await call(env, '/push/subscribe', { endpoint: 'x' })).status, 400);
    assert.equal((await call(env, '/push/nope', {})).status, 404);
    assert.equal((await call(env, '/push/subscribe', {}, 'GET')).status, 405);
  } finally { globalThis.fetch = orig; }
});

test('订阅数有上限：公开端点不能被灌满', async () => {
  const { env } = await envWithKV();
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 201 })) as typeof fetch;
  try {
    for (let i = 0; i < MAX_SUBS; i++) {
      assert.equal((await call(env, '/push/subscribe', subBody(`https://web.push.apple.com/${i}`))).status, 200);
    }
    const res = await call(env, '/push/subscribe', subBody('https://web.push.apple.com/over'));
    assert.equal(res.status, 429);
    // 已在库里的重复订阅仍然放行（同一台设备会周期性刷新）
    assert.equal((await call(env, '/push/subscribe', subBody('https://web.push.apple.com/0'))).status, 200);
  } finally { globalThis.fetch = orig; }
});

test('没配 KV 时整条推送静默关闭而不是报错', async () => {
  const res = await call({} as Env, '/push/subscribe', subBody('https://web.push.apple.com/x'));
  assert.equal(res.status, 503);
});

const logOf = (kv: FakeKV, key = LOG_KEY) => JSON.parse(kv.m.get(key) || '[]') as PushLogEntry[];

// 失败时要把推送服务给的原因带回来。只留一个状态码的话，「为什么没收到」
// 永远只能回答「失败了」，而 Apple 其实写明了是哪一种（BadJwtToken、BadDeviceToken…）
test('推送失败时记下状态码与推送服务给的原因', async () => {
  const { env, kv } = await envWithKV();
  const orig = globalThis.fetch;
  let n = 0;
  globalThis.fetch = (async () => (n++ === 0
    ? new Response(null, { status: 201 })                                        // 订阅时的欢迎推送
    : new Response('{"reason":"BadJwtToken"}', { status: 403 }))) as typeof fetch;  // 之后的正式推送
  try {
    await call(env, '/push/subscribe', subBody('https://web.push.apple.com/a'));
    const out = await push(env, { title: 't', body: 'b' });
    assert.equal(out.subs, 1);
    assert.equal(out.failed, 1);
    assert.equal(out.sent, 0);
    assert.match(out.errors[0]!, /^web\.push\.apple\.com 403 .*BadJwtToken/);
  } finally { globalThis.fetch = orig; }
  assert.equal([...kv.m.keys()].filter(k => k.startsWith('sub:')).length, 1, '403 不是失效，订阅要留着');
});

test('投递时抛异常也要记下原因，而不是只数一个失败', async () => {
  const { env } = await envWithKV();
  const orig = globalThis.fetch;
  let n = 0;
  globalThis.fetch = (async () => {
    if (n++ === 0) return new Response(null, { status: 201 });
    throw new Error('network down');
  }) as typeof fetch;
  try {
    await call(env, '/push/subscribe', subBody('https://web.push.apple.com/b'));
    const out = await push(env, { title: 't', body: 'b' });
    assert.equal(out.failed, 1);
    assert.match(out.errors[0]!, /network down/);
  } finally { globalThis.fetch = orig; }
});

// sent=0 分不清「没人订」和「全失败」，而前者是「为什么没收到」最常见的答案
test('没有订阅时 subs=0，与全部失败区分开', async () => {
  const { env } = await envWithKV();
  const out = await push(env, { title: 't', body: 'b' });
  assert.deepEqual(out, { subs: 0, sent: 0, gone: 0, failed: 0, errors: [] });
});

test('推送没配置时要说出来，不是安静地返回全零', async () => {
  const out = await push({} as Env, { title: 't', body: 'b' });
  assert.equal(out.subs, 0);
  assert.match(out.errors[0]!, /未配置/);
});

test('订阅时的欢迎推送也记进日志', async () => {
  const { env, kv } = await envWithKV();
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 201 })) as typeof fetch;
  try { await call(env, '/push/subscribe', subBody('https://web.push.apple.com/c')); }
  finally { globalThis.fetch = orig; }
  assert.equal(kv.m.has(LOG_KEY), false, '欢迎推送不进定时推送的日志，免得刷订阅就能把它挤掉');
  const log = logOf(kv, WELCOME_LOG_KEY);
  assert.equal(log.length, 1);
  assert.equal(log[0]!.kind, 'welcome');
  assert.equal(log[0]!.sent, 1);
});

test('推送日志只留最近几条，新的在前', async () => {
  const { env, kv } = await envWithKV();
  for (let i = 0; i < LOG_MAX + 5; i++) {
    await recordPush(env, LOG_KEY, { at: String(i), kind: 'report', task: 'ai', subs: 1, sent: 1, gone: 0, failed: 0, errors: [] });
  }
  const log = logOf(kv);
  assert.equal(log.length, LOG_MAX, '要有上限，否则一个值会无限长大');
  assert.equal(log[0]!.at, String(LOG_MAX + 4), '最新的在最前');
});

const entry = (at: string): PushLogEntry =>
  ({ at, kind: 'report', task: 'ai', subs: 1, sent: 1, gone: 0, failed: 0, errors: [] });

test('一次记几条只写一次 KV，新的在前', async () => {
  const { env, kv } = await envWithKV();
  kv.sameKeyMs = 1000; // 真 KV：同一个键一秒只许写一次
  await recordPush(env, LOG_KEY, entry('1'), entry('2'));
  assert.equal(kv.putsBy.get(LOG_KEY), 1);
  assert.deepEqual(logOf(kv).map(e => e.at), ['2', '1']);
});

// 日志是诊断用的。它写不进去不能反过来拦住推送、基线或订阅
test('日志写不进去也不抛', async () => {
  const { env, kv } = await envWithKV();
  kv.fail = op => op === 'put';
  await recordPush(env, LOG_KEY, entry('1'));
  kv.fail = op => op === 'get';
  await recordPush(env, LOG_KEY, entry('2'));
});

// 有人照着文档用 wrangler 查日志时顺手写了个 null 进去：以前从那以后每一次记录都抛
test('日志里存着合法 JSON 但不是数组，也能接着记', async () => {
  for (const junk of ['null', '{}', '"x"', '7']) {
    const { env, kv } = await envWithKV();
    kv.m.set(LOG_KEY, junk);
    await recordPush(env, LOG_KEY, entry('1'));
    assert.deepEqual(logOf(kv).map(e => e.at), ['1'], `存着 ${junk} 时`);
  }
});

test('读日志失败就不写，不拿一份空的盖掉已有记录', async () => {
  const { env, kv } = await envWithKV();
  await recordPush(env, LOG_KEY, entry('old'));
  kv.fail = op => op === 'get';
  await recordPush(env, LOG_KEY, entry('new'));
  kv.fail = undefined;
  assert.deepEqual(logOf(kv).map(e => e.at), ['old']);
});

// 订阅已经存下、欢迎推送也送到了，日志写不进去不能让页面说「开启失败」——
// 用户再点一次就走进了退订
test('欢迎推送的日志写不进去，订阅照样回 ok', async () => {
  const { env, kv } = await envWithKV();
  kv.fail = (op, k) => op === 'put' && k === WELCOME_LOG_KEY;
  const res = await withFetch(async () => new Response(null, { status: 201 }),
    () => call(env, '/push/subscribe', subBody('https://web.push.apple.com/logfail')));
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { ok: true, welcome: 'ok' });
  assert.equal(kv.subs().length, 1);
});

// 缺钥匙时 subs 以前报 0，照文档「subs 为 0 就是没人订」去查，查的是错的方向
test('缺 VAPID 钥匙但有订阅：subs 如实报出，并写明一条都没发', async () => {
  const { env, kv } = await envWithKV();
  await withFetch(async () => new Response(null, { status: 201 }),
    () => call(env, '/push/subscribe', subBody('https://web.push.apple.com/k')));
  let posted = 0;
  const out = await withFetch(async () => { posted++; return new Response(null, { status: 201 }); },
    () => push(envOf(kv), { title: 't', body: 'b' }));
  assert.equal(out.subs, 1);
  assert.equal(out.sent, 0);
  assert.equal(posted, 0);
  assert.match(out.errors[0]!, /VAPID/);
});

test('缺 VAPID 钥匙时不收订阅：收下也发不出去，页面却会显示已开启', async () => {
  const kv = new FakeKV();
  const res = await call(envOf(kv), '/push/subscribe', subBody('https://web.push.apple.com/nokey'));
  assert.equal(res.status, 503);
  assert.equal(kv.subs().length, 0);
});

test('订阅只存校验过的三项，请求里夹带的东西不进 KV', async () => {
  const { env, kv } = await envWithKV();
  await withFetch(async () => new Response(null, { status: 201 }),
    () => call(env, '/push/subscribe', { ...subBody('https://web.push.apple.com/extra'), junk: 'x'.repeat(5000) }));
  const stored = JSON.parse(kv.m.get(kv.subs()[0]!)!);
  assert.deepEqual(Object.keys(stored).sort(), ['auth', 'endpoint', 'p256dh']);
});

test('同一份订阅重复提交不再写 KV，退订不存在的订阅不耗删除额度', async () => {
  const { env, kv } = await envWithKV();
  await withFetch(async () => new Response(null, { status: 201 }), async () => {
    for (let i = 0; i < 3; i++) await call(env, '/push/subscribe', subBody('https://web.push.apple.com/same'));
  });
  assert.equal(kv.putsBy.get(kv.subs()[0]!), 1, '公开端点，每次都写的话写额度会被刷光');
  await call(env, '/push/unsubscribe', { endpoint: 'https://web.push.apple.com/never-subscribed' });
  assert.equal(kv.deletes, 0);
});

const sub = subBody('https://push.example.net/x');

// endpoint 是任何人都能登记的。回 500 加一个没完没了的正文，以前会被整个读进内存
test('推送服务回一个没完没了的正文：只读开头就停', async () => {
  const { env } = await envWithKV();
  let pulled = 0, cancelled = false;
  const endless = new ReadableStream<Uint8Array>({
    pull(c) { pulled += 4096; c.enqueue(new Uint8Array(4096).fill(0x61)); },
    cancel() { cancelled = true; },
  });
  const o = await withFetch(async () => new Response(endless, { status: 500 }),
    () => sendTo(env, 'sub:x', sub, { title: 't', body: 'b' }));
  assert.equal(o.result, 'fail');
  assert.equal(o.status, 500);
  assert.ok(o.detail!.length <= 200);
  assert.ok(pulled <= 16 * 4096, `读了 ${pulled} 字节`);
  assert.ok(cancelled, '读够了要把流关掉');
});

test('推送服务不回话：超时算失败，不拖住整轮', async () => {
  const { env } = await envWithKV();
  const hang = (_u: string, init?: RequestInit) => new Promise<Response>((_, reject) =>
    init?.signal?.addEventListener('abort', () => reject(init.signal!.reason)));
  const t0 = Date.now();
  const o = await withFetch(hang, () => sendTo(env, 'sub:x', sub, { title: 't', body: 'b' }, 50));
  assert.equal(o.result, 'fail');
  assert.match(o.detail!, /秒没有回应/);
  assert.ok(Date.now() - t0 < 5000);
});

test('投递从不抛异常：钥匙坏了也折成一次失败', async () => {
  const kv = new FakeKV();
  const o = await sendTo(envOf(kv, { PUSH_VAPID_PRIVATE_KEY: 'not-a-key', PUSH_VAPID_PUBLIC: 'x' }),
    'sub:x', sub, { title: 't', body: 'b' });
  assert.equal(o.result, 'fail');
  assert.ok(o.detail);
});
