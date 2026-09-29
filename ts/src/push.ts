// Web Push：W3C Push API + VAPID（RFC 8292）+ 载荷加密（RFC 8291）。
//
// 为什么非得有服务端这一层：浏览器订阅之后只交给你一个 endpoint，真正把通知送到
// 手机的是 Apple / Google 的推送服务，而向它投递要用 VAPID 私钥签名、并按 RFC 8291
// 把载荷加密。私钥一旦进页面就等于公开，所以这件事只能在 Worker 里做，订阅也只能
// 存在公网够得着的地方（那台 Mac 在家里的网络后面）。
//
// 加密自己写，与自己写 OCI 签名、自己写 Markdown 渲染器同一个理由：零运行时依赖。
// 派生链在测试里用 Node 的 hkdfSync 交叉验证，再加一次解密回环——这一段错了不会
// 报错，只会让推送静默地永远送不到。
import type { Env } from './env.ts';

const enc = new TextEncoder();

// @cloudflare/workers-types 有三处签名与 WebCrypto 规范对不上。运行时（Workers 与
// Node）都按规范走，所以下面只是把类型掰回来，传参一个字都没改：
//   · exportKey('raw') 被标成可能返回 JWK
//   · generateKey 被标成可能返回单个 CryptoKey
//   · ECDH 的对端公钥字段它叫 $public，规范与两个运行时都认 public
//     （这一条最要命：名字给错不会报错，只会派生出一把对不上的钥匙，
//      推送服务照样回 201，手机上永远什么都不出现。Node 下的测试能当场抓住。）
const asBytes = (v: unknown) => new Uint8Array(v as ArrayBuffer);
const asPair = (v: CryptoKey | CryptoKeyPair) => v as CryptoKeyPair;
const ecdhWith = (pub: CryptoKey) =>
  ({ name: 'ECDH', public: pub } as unknown as SubtleCryptoDeriveKeyAlgorithm);

/** 订阅上限。/push/subscribe 是公开端点，不设上限的话 KV 会被灌满 */
export const MAX_SUBS = 20;
/**
 * 一次投递最多等多久。推送服务正常几百毫秒就回；endpoint 是任何人都能登记的，
 * 一个故意不回话的端点不能拖住整轮定时任务。
 */
export const SEND_TIMEOUT_MS = 10_000;
/** 投递失败时从正文里最多读多少字节找原因。整个读进来的话，回 500 加几百兆正文就能把 Worker 撑爆 */
const DETAIL_BYTES = 1024;
/** 单条记录大小。载荷远小于它，超了直接报错而不是悄悄截断 */
const RECORD_SIZE = 4096;

export function b64uEncode(b: ArrayBuffer | Uint8Array): string {
  const u = b instanceof Uint8Array ? b : new Uint8Array(b);
  return btoa(Array.from(u, c => String.fromCharCode(c)).join('')).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export function b64uDecode(s: string): Uint8Array {
  const t = s.replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(t + '='.repeat((4 - (t.length % 4)) % 4));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

function cat(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((a, p) => a + p.length, 0));
  let o = 0;
  for (const p of parts) { out.set(p, o); o += p.length; }
  return out;
}

async function hmac(key: Uint8Array, data: Uint8Array): Promise<Uint8Array> {
  const k = await crypto.subtle.importKey('raw', key, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  return asBytes(await crypto.subtle.sign('HMAC', k, data));
}

/** HKDF-Expand，输出不超过 32 字节，所以只需要第一个分组 T(1)=HMAC(prk, info||0x01) */
async function expand(prk: Uint8Array, info: Uint8Array, len: number): Promise<Uint8Array> {
  return (await hmac(prk, cat(info, new Uint8Array([1])))).slice(0, len);
}

/**
 * 按 RFC 8291 §3.4 加密一条载荷。
 *
 * salt 与临时密钥对由调用方传进来，测试才能给定值、断出确定的密文。
 */
export async function encrypt(plain: Uint8Array, uaPub: Uint8Array, authSecret: Uint8Array,
                              salt: Uint8Array, pair: CryptoKeyPair): Promise<Uint8Array> {
  const asPub = asBytes(await crypto.subtle.exportKey('raw', pair.publicKey));
  const uaKey = await crypto.subtle.importKey('raw', uaPub, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
  const shared = asBytes(await crypto.subtle.deriveBits(ecdhWith(uaKey), pair.privateKey, 256));

  // 第一段 HKDF 的 salt 是订阅里的 auth secret，info 里**先接收方后发送方**，顺序反了
  // 就得到一把双方对不上的钥匙——而推送服务照样回 201，手机上什么都不会出现
  const prkKey = await hmac(authSecret, shared);
  const ikm = await expand(prkKey, cat(enc.encode('WebPush: info'), new Uint8Array([0]), uaPub, asPub), 32);

  // 第二段是 RFC 8188 的通用派生，salt 走报文头
  const prk = await hmac(salt, ikm);
  const cek = await expand(prk, cat(enc.encode('Content-Encoding: aes128gcm'), new Uint8Array([0])), 16);
  const nonce = await expand(prk, cat(enc.encode('Content-Encoding: nonce'), new Uint8Array([0])), 12);

  const max = RECORD_SIZE - 16 /* GCM tag */ - 1 /* 分隔符 */;
  if (plain.length > max) throw new Error(`载荷 ${plain.length} 字节超过单记录上限 ${max}`);

  const key = await crypto.subtle.importKey('raw', cek, { name: 'AES-GCM' }, false, ['encrypt']);
  // 0x02 是「最后一条记录」的分隔符；给 0x01 的话接收端会等下一条，永远等不到
  const ct = asBytes(
    await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce }, key, cat(plain, new Uint8Array([2]))));

  const rs = new Uint8Array(4);
  new DataView(rs.buffer).setUint32(0, RECORD_SIZE);
  return cat(salt, rs, new Uint8Array([asPub.length]), asPub, ct);
}

/** VAPID 的 Authorization 头。exp 必须在 24 小时内，给 12 小时留足时钟偏差 */
export async function vapidAuth(env: Env, endpoint: string, nowMs = Date.now()): Promise<string> {
  const head = b64uEncode(enc.encode(JSON.stringify({ typ: 'JWT', alg: 'ES256' })));
  const claims = b64uEncode(enc.encode(JSON.stringify({
    aud: new URL(endpoint).origin,
    exp: Math.floor(nowMs / 1000) + 12 * 3600,
    sub: env.PUSH_SUB || 'https://github.com',
  })));
  const key = await crypto.subtle.importKey('pkcs8', b64uDecode(env.PUSH_VAPID_PRIVATE_KEY || ''),
    { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
  // WebCrypto 的 ECDSA 直接给出 r||s，正是 JWS ES256 要的形状（不是 DER）
  const sig = asBytes(await crypto.subtle.sign(
    { name: 'ECDSA', hash: 'SHA-256' }, key, enc.encode(`${head}.${claims}`)));
  return `vapid t=${head}.${claims}.${b64uEncode(sig)}, k=${env.PUSH_VAPID_PUBLIC}`;
}

export interface Sub { endpoint: string; p256dh: string; auth: string }
export interface Msg { title: string; body: string; url?: string; tag?: string }

export function subKey(endpoint: string): string {
  // 用 endpoint 本身做键：同一台设备重复订阅会覆盖而不是堆积。
  // 直接拼进键里即可，KV 的键允许任意 UTF-8，且这不是密钥材料。
  return 'sub:' + b64uEncode(enc.encode(endpoint));
}

/** 收订阅前的校验。公开端点，形状不对的一律拒绝，别把垃圾写进 KV */
export function validSub(v: unknown): v is Sub {
  const s = v as Sub;
  if (!s || typeof s !== 'object') return false;
  if (typeof s.endpoint !== 'string' || !s.endpoint.startsWith('https://') || s.endpoint.length > 1024) return false;
  for (const k of ['p256dh', 'auth'] as const) {
    if (typeof s[k] !== 'string' || !/^[A-Za-z0-9_-]{16,256}$/.test(s[k])) return false;
  }
  // p256dh 必须是 65 字节未压缩点，auth 是 16 字节；长度不对后面加密才炸，不如现在拒
  return b64uDecode(s.p256dh).length === 65 && b64uDecode(s.auth).length === 16;
}

export async function listSubs(env: Env): Promise<{ key: string; sub: Sub }[]> {
  if (!env.PUSH) return [];
  const out: { key: string; sub: Sub }[] = [];
  const res = await env.PUSH.list({ prefix: 'sub:' });
  for (const k of res.keys) {
    const raw = await env.PUSH.get(k.name);
    if (!raw) continue;
    try { out.push({ key: k.name, sub: JSON.parse(raw) as Sub }); } catch { /* 坏记录跳过 */ }
  }
  return out;
}

export type SendResult = 'ok' | 'gone' | 'fail';
/**
 * 一次投递的结果。detail 是推送服务给的原因：Apple 把它写在正文里
 * （如 {"reason":"BadJwtToken"}），只留一个状态码的话失败了也说不清为什么。
 */
export interface SendOutcome { result: SendResult; status?: number; detail?: string }

export const errText = (e: unknown) => String(e instanceof Error ? e.message : e);

/**
 * 投递一条。**从不抛异常**：出了什么事都折成 fail 加原因返回——以前 HTTP 失败走返回值、
 * 其余走异常，每个调用方都得自己再包一层 catch，两处写法还各不相同。
 */
export async function sendTo(env: Env, key: string, sub: Sub, msg: Msg,
                             timeoutMs = SEND_TIMEOUT_MS): Promise<SendOutcome> {
  try {
    const pair = asPair(
      await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']));
    const body = await encrypt(enc.encode(JSON.stringify(msg)), b64uDecode(sub.p256dh),
      b64uDecode(sub.auth), crypto.getRandomValues(new Uint8Array(16)), pair);

    const res = await fetch(sub.endpoint, {
      method: 'POST',
      headers: {
        'content-encoding': 'aes128gcm',
        'content-type': 'application/octet-stream',
        ttl: '86400',
        urgency: 'normal',
        authorization: await vapidAuth(env, sub.endpoint),
      },
      body,
      signal: AbortSignal.timeout(timeoutMs),
    });
    // 404/410 = 这个订阅已经没了（应用被删、系统吊销）。不清掉的话它会永远失败
    if (res.status === 404 || res.status === 410) {
      try { await env.PUSH?.delete(key); } catch { /* 删不掉下次还是 410，到时再删 */ }
      return { result: 'gone', status: res.status };
    }
    if (res.ok) return { result: 'ok', status: res.status };
    return { result: 'fail', status: res.status, detail: (await head(res, DETAIL_BYTES)).slice(0, 200) };
  } catch (e) {
    const timeout = (e as { name?: string } | null)?.name === 'TimeoutError';
    return { result: 'fail', detail: timeout ? `${timeoutMs / 1000} 秒没有回应` : errText(e) };
  }
}

/** 只读正文开头 n 字节，读到超时就用已经读到的 */
async function head(res: Response, n: number): Promise<string> {
  if (!res.body) return '';
  const reader = res.body.getReader();
  const parts: Uint8Array[] = [];
  let got = 0;
  try {
    while (got < n) {
      const { done, value } = await reader.read();
      if (done) break;
      parts.push(value);
      got += value.length;
    }
  } catch { /* 超时或断流：有多少算多少 */ }
  reader.cancel().catch(() => {});
  return new TextDecoder().decode(cat(...parts).slice(0, n));
}

/**
 * 一次扇出的汇总。subs 单列出来：「为什么没收到」最常见的答案是**根本没有订阅**，
 * 而 sent=0 分不清是没人订还是全失败了。
 */
export interface FanOut { subs: number; sent: number; gone: number; failed: number; errors: string[] }

export async function push(env: Env, msg: Msg): Promise<FanOut> {
  const out: FanOut = { subs: 0, sent: 0, gone: 0, failed: 0, errors: [] };
  if (!env.PUSH) {
    out.errors.push('推送未配置（缺 KV 绑定）');
    return out;
  }
  // 先数订阅再看钥匙：subs 要如实反映「有没有人订」，否则缺钥匙会被读成没人订
  const subs = await listSubs(env);
  out.subs = subs.length;
  if (!env.PUSH_VAPID_PRIVATE_KEY || !env.PUSH_VAPID_PUBLIC) {
    out.errors.push('推送未配置（缺 VAPID 钥匙），一条都没发');
    return out;
  }
  for (const { key, sub } of subs) tally(out, sub.endpoint, await sendTo(env, key, sub, msg));
  return out;
}

/** 把一次投递的结果计进汇总。报告推送与欢迎推送共用，「失败」怎么描述只写在这一处 */
function tally(out: FanOut, endpoint: string, o: SendOutcome): void {
  if (o.result === 'ok') out.sent++;
  else if (o.result === 'gone') out.gone++;
  else {
    out.failed++;
    out.errors.push(`${hostOf(endpoint)} ${o.status ?? '-'} ${o.detail ?? ''}`.trim());
  }
}

function hostOf(u: string): string {
  try { return new URL(u).host; } catch { return '?'; }
}

/**
 * 最近几次推送的结果，存在 KV 里。拿现有的 KV 权限就能读，不必再给令牌开日志权限。
 *
 * 欢迎推送单独一个键：订阅是公开端点，谁都能反复提交，和定时推送共用一个键的话，
 * 刷三十次订阅就能把四档简报的记录全挤掉——而那正是查「为什么没收到」要看的。
 * 分开也免得订阅与定时任务在同一秒里写同一个键（KV 同一个键一秒只许写一次）。
 */
export const LOG_KEY = 'log:push';
export const WELCOME_LOG_KEY = 'log:welcome';
export const LOG_MAX = 30;

export interface PushLogEntry extends FanOut {
  at: string;
  kind: 'report' | 'welcome';
  task?: string;
  date?: string;
}

/**
 * 记几条推送结果，一次调用只写一次 KV。
 *
 * 为什么要有：09-29 豆瓣那档跑完，用户说没收到通知。KV 里能看到定时任务发现了
 * 豆瓣、调用了推送，但推送**结果**没有落在任何地方——是没有订阅、Apple 拒了、
 * 还是送达了但手机没弹，一样也分不出来，只能事后复现一次去猜。
 * 同时打一行 console.log，给开了 observability 的场合用。
 *
 * ⚠️ **从不抛异常**。日志是诊断用的，它写不进去（额度用完、同一秒写了两次）不能
 * 拦住后面的事：以前它在推送循环里、基线写入之前，一抛就让基线停在原地，下一轮
 * 把同样几档再推一遍，每 10 分钟一次。
 */
export async function recordPush(env: Env, key: string, ...entries: PushLogEntry[]): Promise<void> {
  for (const e of entries) console.log(JSON.stringify({ push: 'newsdesk', ...e }));
  if (!env.PUSH || entries.length === 0) return;
  try {
    const raw = await env.PUSH.get(key); // 读失败就不写：别拿一份空的盖掉已有的记录
    let prev: unknown = [];
    try { prev = JSON.parse(raw || '[]'); } catch { /* 坏了就重来 */ }
    // 合法 JSON 但不是数组（有人手动写了 null 或 {}）同样重来，不能在这里炸
    const log = [...entries].reverse().concat(Array.isArray(prev) ? prev as PushLogEntry[] : []);
    await env.PUSH.put(key, JSON.stringify(log.slice(0, LOG_MAX)));
  } catch (e) {
    console.log(JSON.stringify({ push: 'newsdesk', logWriteFailed: key, error: errText(e) }));
  }
}

const json = (v: unknown, status = 200) =>
  new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json; charset=utf-8' } });

/**
 * /push/* 的路由。只有订阅与退订两个动作，都不需要鉴权——能拿到 endpoint 的
 * 就是设备自己，这是 Web Push 的设计。防滥用靠形状校验加订阅数上限。
 */
export async function route(request: Request, env: Env, url: URL): Promise<Response> {
  if (request.method !== 'POST') return json({ error: 'POST only' }, 405);
  if (!env.PUSH) return json({ error: 'push not configured' }, 503);

  let payload: unknown;
  try { payload = await request.json(); } catch { return json({ error: 'bad json' }, 400); }

  if (url.pathname === '/push/unsubscribe') {
    const ep = (payload as { endpoint?: string })?.endpoint;
    if (typeof ep !== 'string') return json({ error: 'bad endpoint' }, 400);
    // 先读再删：谁都能调这个端点，拿随便编的 endpoint 刷的话，删除额度（一天 1000 次）
    // 会被白白吃掉；读有十万次
    const k = subKey(ep);
    if (await env.PUSH.get(k)) await env.PUSH.delete(k);
    return json({ ok: true });
  }

  if (url.pathname !== '/push/subscribe') return json({ error: 'not found' }, 404);
  // 缺钥匙时收下订阅也发不出去，页面却会显示「已开启」。直接拒，页面会说开启失败
  if (!env.PUSH_VAPID_PRIVATE_KEY || !env.PUSH_VAPID_PUBLIC) return json({ error: 'push not configured' }, 503);
  if (!validSub(payload)) return json({ error: 'bad subscription' }, 400);

  const key = subKey(payload.endpoint);
  // 只存校验过的三项。原样存请求体的话，夹带的任意字段也会进 KV，之后每次推送都要读一遍
  const value = JSON.stringify({ endpoint: payload.endpoint, p256dh: payload.p256dh, auth: payload.auth });
  const existing = await env.PUSH.get(key);
  if (!existing && (await env.PUSH.list({ prefix: 'sub:' })).keys.length >= MAX_SUBS) {
    return json({ error: 'too many subscriptions' }, 429);
  }
  // 同一份订阅重复提交就不再写一遍：公开端点，每次都写的话写额度会被刷光
  if (existing !== value) await env.PUSH.put(key, value);

  // 立刻回推一条。既是欢迎语，也是唯一能在「点了开启」当场验证整条链路
  // （VAPID 签名、加密、投递、SW 弹窗）的办法——否则要等到下一档才知道通不通。
  // 到这里订阅已经存下了，后面不管出什么事都要回 ok：回错的话页面会说失败，
  // 用户再点一次反倒走进了退订。
  const o = await sendTo(env, key, payload, {
    title: '通知已开启', body: '新简报出炉时会在这里提醒你。', url: '/', tag: 'newsdesk-welcome' });
  // subs 在这里是「这次推给了几个订阅」：欢迎推送只推新订阅的这一个
  const out: FanOut = { subs: 1, sent: 0, gone: 0, failed: 0, errors: [] };
  tally(out, payload.endpoint, o);
  await recordPush(env, WELCOME_LOG_KEY, { at: new Date().toISOString(), kind: 'welcome', ...out });
  return json({ ok: true, welcome: o.result });
}
