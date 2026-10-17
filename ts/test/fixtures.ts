// 测试共用的替身。以前 FakeKV 在两个测试文件里各抄一份，已经长歪了（一份会计数、一份不会）。
import { existsSync, readFileSync } from 'node:fs';
// ⚠️ 用 fileURLToPath 而不是把 URL 直接交给 fs：@cloudflare/workers-types 也声明了
// 一个全局 URL，它的 searchParams 迭代器类型与 node:url 的不兼容，TS 7 下会把
// new URL() 认成前者，于是 existsSync/readFileSync 全部报「不是 PathLike」。
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { b64uEncode } from '../src/push.ts';
import type { Env } from '../src/env.ts';

type Op = 'get' | 'put' | 'delete' | 'list';

/**
 * 内存里的 KV。按需模拟真 KV 的两条限制——恰恰是一个 Map 模拟不出来、线上会出事的：
 *   · 同一个键一秒只能写一次，超了回 429（sameKeyMs = 1000）
 *   · 某些操作直接失败：额度用完、服务出错（fail）
 */
export class FakeKV {
  m = new Map<string, string>();
  puts = 0;
  deletes = 0;
  lists = 0;
  putsBy = new Map<string, number>();
  /** 每个键最后一次写入带的选项（比如 expirationTtl） */
  optsBy = new Map<string, { expirationTtl?: number } | undefined>();
  /** 同一个键两次写入之间至少隔多久（毫秒）。真 KV 是 1000；0 = 不限 */
  sameKeyMs = 0;
  /** 返回 true 的那次操作直接抛错 */
  fail?: (op: Op, key: string) => boolean;
  lastPut = new Map<string, number>();

  async get(k: string) { this.check('get', k); return this.m.get(k) ?? null; }
  async put(k: string, v: string, opts?: { expirationTtl?: number }) {
    this.check('put', k);
    const now = Date.now(), last = this.lastPut.get(k);
    if (this.sameKeyMs && last !== undefined && now - last < this.sameKeyMs) {
      throw new Error(`KV PUT failed: 429 Too Many Requests (${k})`);
    }
    this.lastPut.set(k, now);
    this.puts++;
    this.putsBy.set(k, (this.putsBy.get(k) ?? 0) + 1);
    this.optsBy.set(k, opts);
    this.m.set(k, v);
  }
  async delete(k: string) { this.check('delete', k); this.deletes++; this.m.delete(k); }
  async list({ prefix }: { prefix?: string } = {}) {
    this.check('list', prefix ?? '');
    this.lists++;
    return {
      keys: [...this.m.keys()].filter(k => !prefix || k.startsWith(prefix)).map(name => ({ name })),
      list_complete: true as const, cacheStatus: null,
    };
  }
  subs() { return [...this.m.keys()].filter(k => k.startsWith('sub:')); }
  private check(op: Op, k: string) {
    if (this.fail?.(op, k)) throw new Error(`KV ${op} failed (${k})`);
  }
}

export const envOf = (kv: FakeKV, extra: Partial<Env> = {}): Env =>
  ({ PUSH: kv as unknown as KVNamespace, ...extra } as Env);

/** 带一对真 VAPID 钥匙的 env */
export async function vapidEnv(kv: FakeKV, extra: Partial<Env> = {}): Promise<Env> {
  const kp = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true,
    ['sign', 'verify']) as CryptoKeyPair;
  return envOf(kv, {
    PUSH_VAPID_PRIVATE_KEY: b64uEncode(await crypto.subtle.exportKey('pkcs8', kp.privateKey) as unknown as ArrayBuffer),
    PUSH_VAPID_PUBLIC: b64uEncode(await crypto.subtle.exportKey('raw', kp.publicKey) as unknown as ArrayBuffer),
    ...extra,
  });
}

// p256dh 必须是曲线上的真实点：随手拼 0x04 加一串字节会被 importKey 拒掉
// （这是对的——真浏览器给的一定是真点，但测试得跟着真实）
const uaPair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true,
  ['deriveBits']) as CryptoKeyPair;
const uaRaw = b64uEncode(await crypto.subtle.exportKey('raw', uaPair.publicKey) as unknown as ArrayBuffer);

/** 一份形状合法的订阅，就是浏览器 POST 给 /push/subscribe 的那样 */
export const subBody = (endpoint: string) => ({
  endpoint, p256dh: uaRaw, auth: b64uEncode(new Uint8Array(16).fill(3)),
});

/** 在测试期间把 fetch 换掉，结束后换回来 */
export async function withFetch<T>(fake: (url: string, init?: RequestInit) => Promise<Response>,
                                   body: () => Promise<T>): Promise<T> {
  const orig = globalThis.fetch;
  globalThis.fetch = fake as unknown as typeof fetch;
  try { return await body(); } finally { globalThis.fetch = orig; }
}

// 读 wrangler 配置里的 cron，换算成间隔与相对整点的偏移（毫秒）。只认这个仓库用得到的
// 三种写法：每 N 分钟（星号斜杠 N）、每小时第 M 分钟、每 H 小时的第 M 分钟。认不出就抛，
// 让人来改这里，不猜。文件不存在返回 null：wrangler.toml 不进版本控制，换台机器可能没有。
export function cronOf(file: string): { expr: string; intervalMs: number; offsetMs: number } | null {
  // 不用 new URL()：全局 URL 被 workers-types 声明成另一个形状，TS 7 下与 node 的
  // 不兼容。拿 import.meta.url 这个字符串转成目录再拼，绕开整个类型冲突。
  const path = join(dirname(fileURLToPath(import.meta.url)), '..', file);
  if (!existsSync(path)) return null;
  const expr = /^crons\s*=\s*\[\s*"([^"]+)"\s*\]/m.exec(readFileSync(path, 'utf8'))?.[1];
  if (!expr) throw new Error(`${file} 里没有单条 cron`);
  const bad = () => new Error(`认不出 ${file} 的 cron「${expr}」，改 test/fixtures.ts 的 cronOf`);
  const [min = '', hour = '', ...rest] = expr.trim().split(/\s+/);
  if (rest.length !== 3 || rest.some(f => f !== '*')) throw bad();
  const every = (f: string) => /^\*\/(\d+)$/.exec(f)?.[1];
  const minStep = every(min);
  if (minStep && hour === '*') return { expr, intervalMs: Number(minStep) * 60e3, offsetMs: 0 };
  if (!/^\d+$/.test(min)) throw bad();
  const offsetMs = Number(min) * 60e3;
  if (hour === '*') return { expr, intervalMs: 3.6e6, offsetMs };
  const hourStep = every(hour);
  if (hourStep) return { expr, intervalMs: Number(hourStep) * 3.6e6, offsetMs };
  throw bad();
}
