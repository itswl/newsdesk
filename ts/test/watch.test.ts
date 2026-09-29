import { test } from 'node:test';
import assert from 'node:assert/strict';
import { newReports, snapshot, watchReports } from '../src/watch.ts';
import { staleWatch } from '../src/selfcheck.ts';
import type { Env } from '../src/env.ts';

const doc = (date: string, tasks: Record<string, string>) => ({
  date,
  tasks: Object.fromEntries(Object.entries(tasks).map(([k, v]) =>
    [k, { status: v, finished: '06:45', label: k.toUpperCase() }])),
});

test('只有「从没有报告变成有报告」才算新出炉', () => {
  const seen = { date: '2026-09-28', tasks: { ai: 'ok', douban: 'pending' } };
  const got = newReports(doc('2026-09-28', { ai: 'ok', douban: 'ok' }), seen);
  assert.deepEqual(got.map(g => g.key), ['douban'], '已经通知过的 ai 不该再来一次');
  assert.equal(got[0]!.label, 'DOUBAN', '标题用 status.json 里的栏目名');
});

// trending 那一档可能先出草稿再出正式版。按「状态变了」算的话同一天同一档要推两条
test('draft 变 ok 不再推第二条', () => {
  const got = newReports(doc('2026-09-28', { trending: 'ok' }),
    { date: '2026-09-28', tasks: { trending: 'draft' } });
  assert.deepEqual(got, []);
});

test('草稿本身算一次出炉', () => {
  const got = newReports(doc('2026-09-28', { trending: 'draft' }),
    { date: '2026-09-28', tasks: { trending: 'pending' } });
  assert.deepEqual(got.map(g => g.key), ['trending']);
});

// 新的一天各档回到 pending。拿昨天的 ok 当基线的话，今早第一档会被看成「没变化」而漏推
test('跨日要把基线清空，否则第二天第一档漏推', () => {
  const got = newReports(doc('2026-09-29', { ai: 'ok', douban: 'pending' }),
    { date: '2026-09-28', tasks: { ai: 'ok', douban: 'ok' } });
  assert.deepEqual(got.map(g => g.key), ['ai']);
});

test('失败与跳过不推送（那些走飞书）', () => {
  const got = newReports(doc('2026-09-28', { ai: 'failed', douban: 'skipped', trending: 'retrying' }),
    { date: '2026-09-28', tasks: {} });
  assert.deepEqual(got, []);
});

test('snapshot 只记状态', () => {
  assert.deepEqual(snapshot(doc('2026-09-28', { ai: 'ok' })), { date: '2026-09-28', tasks: { ai: 'ok' } });
});

class FakeKV {
  m = new Map<string, string>();
  puts = 0; deletes = 0; putsBy = new Map<string, number>();
  async get(k: string) { return this.m.has(k) ? this.m.get(k)! : null; }
  async put(k: string, v: string) {
    this.puts++; this.putsBy.set(k, (this.putsBy.get(k) ?? 0) + 1); this.m.set(k, v);
  }
  async delete(k: string) { this.deletes++; this.m.delete(k); }
  async list({ prefix }: { prefix?: string } = {}) {
    return {
      keys: [...this.m.keys()].filter(k => !prefix || k.startsWith(prefix)).map(name => ({ name })),
      list_complete: true as const, cacheStatus: null,
    };
  }
}
const envKV = (kv: FakeKV) => ({ PUSH: kv as unknown as KVNamespace } as Env);

// 刚上线那一刻当天可能已经跑完几档；没有这条会把它们一次性全推出来
test('第一次跑只记基线，不补推当天已完成的几档', async () => {
  const kv = new FakeKV();
  let posted = 0;
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => { posted++; return new Response(null, { status: 201 }); }) as typeof fetch;
  try {
    await watchReports(envKV(kv), JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'ok' })));
  } finally { globalThis.fetch = orig; }
  assert.equal(posted, 0, '第一次不该推');
  assert.equal(JSON.parse(kv.m.get('seen:reports')!).tasks.ai, 'ok', '但基线要记下来');
});

test('第二轮出现新报告才推，且基线跟着推进', async () => {
  const kv = new FakeKV();
  kv.m.set('seen:reports', JSON.stringify({ date: '2026-09-28', tasks: { ai: 'ok', douban: 'pending' } }));
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 201 })) as typeof fetch;
  try {
    // 没有订阅，所以不会真的发；这里验的是基线推进与不重复判定
    await watchReports(envKV(kv), JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'ok' })));
  } finally { globalThis.fetch = orig; }
  assert.deepEqual(JSON.parse(kv.m.get('seen:reports')!).tasks, { ai: 'ok', douban: 'ok' });
});

// cron 为了及时发现新简报提到了 10 分钟一次；不节流的话机器关一天就是 144 条飞书消息
test('同一次停更按小时节流，恢复后立刻解除', async () => {
  const kv = new FakeKV();
  const sent: string[] = [];
  const orig = globalThis.fetch;
  globalThis.fetch = (async (_u: string, init: RequestInit) => {
    sent.push(String(init.body)); return new Response('{"code":0}', { status: 200 });
  }) as unknown as typeof fetch;
  const env = { ...envKV(kv), ALERT_WEBHOOK: 'https://open.feishu.cn/x/hook' } as Env;
  const stale = JSON.stringify({ generated_epoch: 1000, date: '2026-09-01', tasks: {} });
  const t0 = 1000 * 1000 + 100 * 3.6e6; // 停更 100 小时
  try {
    await staleWatch(env, { ok: true, status: 200, text: stale }, t0);
    assert.equal(sent.length, 1, '第一次要告警');
    await staleWatch(env, { ok: true, status: 200, text: stale }, t0 + 3.6e6);
    assert.equal(sent.length, 1, '一小时后还在节流窗口内，不该再推');
    await staleWatch(env, { ok: true, status: 200, text: stale }, t0 + 7 * 3.6e6);
    assert.equal(sent.length, 2, '超过节流窗口要再提醒一次');

    const fresh = JSON.stringify({ generated_epoch: Math.floor(t0 / 1000), date: '2026-09-28', tasks: {} });
    await staleWatch(env, { ok: true, status: 200, text: fresh }, t0);
    assert.equal(sent.length, 2, '恢复了不告警');
    assert.equal(kv.m.has('alerted:stale'), false, '恢复后要清掉节流，下次出事立刻响');
  } finally { globalThis.fetch = orig; }
});

// cron 每 10 分钟一次。基线没变还照写的话一天就是 144 次写入，而免费档 KV 一天
// 只有 1000 次；额度耗尽时推送与订阅会静默失败
test('基线没变就不写 KV', async () => {
  const kv = new FakeKV();
  const d = JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'pending' }));
  const seenPuts = () => kv.putsBy.get('seen:reports') ?? 0;
  await watchReports(envKV(kv), d);            // 第一次：记基线
  assert.equal(seenPuts(), 1);
  await watchReports(envKV(kv), d);            // 同样的状态再来两轮
  await watchReports(envKV(kv), d);
  assert.equal(kv.puts, 1, '状态没变不该写任何东西');
  // 状态变了：基线写一次；这是一档新简报，所以另外还有一条推送日志
  await watchReports(envKV(kv), JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'ok' })));
  assert.equal(seenPuts(), 2, '基线在状态变了时写且只写一次');
  assert.equal(kv.putsBy.get('log:push'), 1, '新简报要留一条推送日志');
});

test('停更自查：站点正常时不去删一个本来就不存在的节流键', async () => {
  const kv = new FakeKV();
  const env = { ...envKV(kv), ALERT_WEBHOOK: 'https://open.feishu.cn/x/hook' } as Env;
  const now = 1790000000000;
  const fresh = JSON.stringify({ generated_epoch: Math.floor(now / 1000), date: '2026-09-28', tasks: {} });
  for (let i = 0; i < 5; i++) await staleWatch(env, { ok: true, status: 200, text: fresh }, now);
  assert.equal(kv.deletes, 0, '每轮都 delete 一天就是 144 次，吃删除额度');
});

// 这条就是 09-29 缺的东西：定时任务发现了新报告、调用了推送，但结果没落在任何地方，
// 事后只能复现一次去猜是没订阅、被拒了还是送达了没弹
test('每推一档都在 KV 里记下结果：几个订阅、成功几个、失败原因', async () => {
  const kp = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true,
    ['sign', 'verify']) as CryptoKeyPair;
  const { b64uEncode } = await import('../src/push.ts');
  const kv = new FakeKV();
  const env = {
    ...envKV(kv),
    PUSH_VAPID_PRIVATE_KEY: b64uEncode(await crypto.subtle.exportKey('pkcs8', kp.privateKey) as unknown as ArrayBuffer),
    PUSH_VAPID_PUBLIC: b64uEncode(await crypto.subtle.exportKey('raw', kp.publicKey) as unknown as ArrayBuffer),
  } as Env;
  kv.m.set('seen:reports', JSON.stringify({ date: '2026-09-28', tasks: { ai: 'ok', douban: 'pending' } }));

  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 201 })) as typeof fetch;
  try {
    // 没有订阅：推送照样尝试，日志要写明 subs=0——这正是「为什么没收到」的答案
    await watchReports(env, JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'ok' })));
  } finally { globalThis.fetch = orig; }
  const log = JSON.parse(kv.m.get('log:push') || '[]');
  assert.equal(log.length, 1);
  assert.equal(log[0].kind, 'report');
  assert.equal(log[0].task, 'douban');
  assert.equal(log[0].date, '2026-09-28');
  assert.equal(log[0].subs, 0);
});
