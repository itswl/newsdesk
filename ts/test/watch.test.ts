import { test } from 'node:test';
import assert from 'node:assert/strict';
import { newReports, snapshot, watchReports } from '../src/watch.ts';
import { staleWatch } from '../src/selfcheck.ts';
import { LOG_KEY } from '../src/push.ts';
import type { Env } from '../src/env.ts';
import { envOf, FakeKV, subBody, vapidEnv, withFetch } from './fixtures.ts';

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

const envKV = (kv: FakeKV) => envOf(kv);

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
  assert.equal(kv.putsBy.get(LOG_KEY), 1, '新简报要留一条推送日志');
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
  const kv = new FakeKV();
  const env = await vapidEnv(kv);
  kv.m.set('seen:reports', JSON.stringify({ date: '2026-09-28', tasks: { ai: 'ok', douban: 'pending' } }));
  // 没有订阅：推送照样尝试，日志要写明 subs=0——这正是「为什么没收到」的答案
  await withFetch(async () => new Response(null, { status: 201 }),
    () => watchReports(env, JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'ok' }))));
  const log = JSON.parse(kv.m.get(LOG_KEY) || '[]');
  assert.equal(log.length, 1);
  assert.equal(log[0].kind, 'report');
  assert.equal(log[0].task, 'douban');
  assert.equal(log[0].date, '2026-09-28');
  assert.equal(log[0].subs, 0);
  assert.deepEqual(log[0].errors, [], '配好了、只是没人订，与「没配置」要分得开');
});

/** 一个订阅、两档同时出炉的现场。status.json 发布失败后被下一档一起带出来就是这样 */
async function twoFresh(kv: FakeKV) {
  const env = await vapidEnv(kv);
  await withFetch(async () => new Response(null, { status: 201 }), async () => {
    const { route } = await import('../src/push.ts');
    const url = new URL('https://news.example.com/push/subscribe');
    await route(new Request(url, { method: 'POST', body: JSON.stringify(subBody('https://web.push.apple.com/me')) }), env, url);
  });
  kv.m.set('seen:reports', JSON.stringify({ date: '2026-09-28', tasks: { ai: 'ok', douban: 'pending', trending: 'pending' } }));
  kv.lastPut.clear(); // 订阅那几次写入不算进这一轮
  return { env, status: JSON.stringify(doc('2026-09-28', { ai: 'ok', douban: 'ok', trending: 'ok' })) };
}

// 以前每推一档写一次日志。KV 同一个键一秒只许写一次，第二次写回 429，整轮中断在
// 基线之前——下一轮把这两档再推一遍，每 10 分钟一次，直到跨日
test('同一轮出两档：各推一次，日志只写一次，基线照样推进，下一轮不重推', async () => {
  const kv = new FakeKV();
  const { env, status } = await twoFresh(kv);
  kv.sameKeyMs = 1000;
  let posted = 0;
  const count = async () => { posted++; return new Response(null, { status: 201 }); };
  await withFetch(count, () => watchReports(env, status));
  assert.equal(posted, 2, '两档各推一次');
  assert.equal(kv.putsBy.get(LOG_KEY), 1, '日志一轮只写一次');
  assert.deepEqual(JSON.parse(kv.m.get(LOG_KEY)!).map((e: { task: string }) => e.task), ['trending', 'douban']);
  assert.deepEqual(JSON.parse(kv.m.get('seen:reports')!).tasks, { ai: 'ok', douban: 'ok', trending: 'ok' });

  await withFetch(count, () => watchReports(env, status));
  assert.equal(posted, 2, '下一轮不该重推');
});

// 推出去了却记不住（当天写额度用完）的话，下一轮会把同样几档再推一遍，每 10 分钟一次。
// 先记基线：记不住就这一轮先不推，下一轮记住了再推，一条都不重复
test('基线写不进去：这一轮一条都不推，下一轮写进去了再推，只推一次', async () => {
  const kv = new FakeKV();
  const { env, status } = await twoFresh(kv);
  let posted = 0;
  const count = async () => { posted++; return new Response(null, { status: 201 }); };
  kv.fail = (op, k) => op === 'put' && k === 'seen:reports';
  await withFetch(count, () => assert.rejects(watchReports(env, status)));
  assert.equal(posted, 0, '基线没记住就不能推');
  kv.fail = undefined;
  await withFetch(count, () => watchReports(env, status));
  await withFetch(count, () => watchReports(env, status));
  assert.equal(posted, 2, '记住之后两档各推一次，此后不再推');
});

test('日志写不进去，基线也要推进，不能下一轮重推', async () => {
  const kv = new FakeKV();
  const { env, status } = await twoFresh(kv);
  kv.fail = (op, k) => op === 'put' && k === LOG_KEY;
  let posted = 0;
  const count = async () => { posted++; return new Response(null, { status: 201 }); };
  await withFetch(count, () => watchReports(env, status));
  await withFetch(count, () => watchReports(env, status));
  assert.equal(posted, 2);
  assert.equal(JSON.parse(kv.m.get('seen:reports')!).tasks.trending, 'ok');
});

// 列订阅失败时一条都还没发出去。宁可漏这一档（日志里写明），也不让它卡住基线
test('读不到订阅列表：这一档记进日志，别的档照推，基线照样推进', async () => {
  const kv = new FakeKV();
  const { env, status } = await twoFresh(kv);
  let lists = 0;
  kv.fail = op => op === 'list' && lists++ === 0; // 只有第一档那次失败
  let posted = 0;
  await withFetch(async () => { posted++; return new Response(null, { status: 201 }); },
    () => watchReports(env, status));
  const log = JSON.parse(kv.m.get(LOG_KEY)!);
  assert.match(log[1].errors[0], /读不到订阅列表/);
  assert.equal(log[0].sent, 1, '后一档照推');
  assert.equal(posted, 1);
  assert.equal(JSON.parse(kv.m.get('seen:reports')!).tasks.douban, 'ok');
});

// 节流状态存不进 KV（比如当天写额度用完）时，以前先写节流、写失败就抛，
// 停更告警一条都发不出去；也不能退成每 10 分钟一条
test('节流状态写不进 KV：停更告警照发，改按钟点每 6 小时一条', async () => {
  const kv = new FakeKV();
  kv.fail = (op, k) => op === 'put' && k === 'alerted:stale';
  const sent: string[] = [];
  const env = { ...envOf(kv), ALERT_WEBHOOK: 'https://open.feishu.cn/x/hook' } as Env;
  const stale = JSON.stringify({ generated_epoch: 1000, date: '2026-09-01', tasks: {} });
  const bucket = 6 * 3.6e6 * 1000; // 某个 6 小时整点
  await withFetch(async (_u, init) => { sent.push(String(init?.body)); return new Response('{"code":0}'); }, async () => {
    await staleWatch(env, { ok: true, status: 200, text: stale }, bucket + 60e3);
    assert.equal(sent.length, 1, '整点后头一轮要发');
    await staleWatch(env, { ok: true, status: 200, text: stale }, bucket + 20 * 60e3);
    await staleWatch(env, { ok: true, status: 200, text: stale }, bucket + 3 * 3.6e6);
    assert.equal(sent.length, 1, '同一个 6 小时里不再发');
    await staleWatch(env, { ok: true, status: 200, text: stale }, bucket + 6 * 3.6e6 + 60e3);
    assert.equal(sent.length, 2, '下一个 6 小时再提醒一次');
  });
});

test('站点恢复时节流清不掉也不抛', async () => {
  const kv = new FakeKV();
  kv.fail = () => true;
  const env = { ...envOf(kv), ALERT_WEBHOOK: 'https://open.feishu.cn/x/hook' } as Env;
  const now = 1790000000000;
  const fresh = JSON.stringify({ generated_epoch: Math.floor(now / 1000), date: '2026-09-28', tasks: {} });
  await staleWatch(env, { ok: true, status: 200, text: fresh }, now);
});

// 两条线各管各的：以前停更自查一抛，新简报推送这一轮就跟着没了；反过来也一样
test('定时任务里一条线出错，不连累另一条', async () => {
  const { tick } = await import('../src/cron.ts');
  const kv = new FakeKV();
  kv.fail = (op, k) => op === 'get' && k === 'seen:reports';
  const env = { ...envOf(kv), OCI_REGION: 'r', OCI_NAMESPACE: 'n', OCI_BUCKET: 'b' } as Env;
  const fresh = JSON.stringify({ generated_epoch: Math.floor(Date.now() / 1000), date: '2026-09-28', tasks: {} });
  await withFetch(async () => new Response(fresh), () => tick(env));
});
