// 测试共用的替身。以前 FakeKV 在两个测试文件里各抄一份，已经长歪了（一份会计数、一份不会）。
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
  putsBy = new Map<string, number>();
  /** 同一个键两次写入之间至少隔多久（毫秒）。真 KV 是 1000；0 = 不限 */
  sameKeyMs = 0;
  /** 返回 true 的那次操作直接抛错 */
  fail?: (op: Op, key: string) => boolean;
  lastPut = new Map<string, number>();

  async get(k: string) { this.check('get', k); return this.m.get(k) ?? null; }
  async put(k: string, v: string) {
    this.check('put', k);
    const now = Date.now(), last = this.lastPut.get(k);
    if (this.sameKeyMs && last !== undefined && now - last < this.sameKeyMs) {
      throw new Error(`KV PUT failed: 429 Too Many Requests (${k})`);
    }
    this.lastPut.set(k, now);
    this.puts++;
    this.putsBy.set(k, (this.putsBy.get(k) ?? 0) + 1);
    this.m.set(k, v);
  }
  async delete(k: string) { this.check('delete', k); this.deletes++; this.m.delete(k); }
  async list({ prefix }: { prefix?: string } = {}) {
    this.check('list', prefix ?? '');
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
