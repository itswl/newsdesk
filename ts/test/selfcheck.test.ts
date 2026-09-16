import { test } from 'node:test';
import assert from 'node:assert/strict';
import { evaluate, STALE_HOURS_DEFAULT } from '../src/selfcheck.ts';

const NOW = Date.parse('2026-09-17T12:00:00Z');

function doc(extra: Record<string, unknown>) {
  return JSON.stringify({ date: '2026-09-17', tasks: { ai: { status: 'ok' } }, ...extra });
}

test('新鲜的站点不告警', () => {
  const v = evaluate(doc({ generated_epoch: NOW / 1000 - 3600 }), NOW, STALE_HOURS_DEFAULT);
  assert.equal(v.bad, false);
  assert.ok(Math.abs(v.age! - 1) < 0.01);
});

test('超过阈值就告警并带上最后已知状态', () => {
  const v = evaluate(doc({ generated_epoch: NOW / 1000 - 30 * 3600, generated: 'x' }),
                     NOW, STALE_HOURS_DEFAULT);
  assert.equal(v.bad, true);
  assert.match(v.title!, /停更 30\.0 小时/);
  assert.match(v.body!, /ai: ok/);
});

// ⚠️ 不带偏移量的 ISO 会被 Worker（跑在 UTC）当成 UTC，北京时间算出来差整整
// 8 小时——刚生成的站点会被报成「未来」或「停更」。epoch 必须优先。
test('epoch 优先于可能有时区歧义的 ISO', () => {
  const raw = doc({
    generated_epoch: NOW / 1000 - 3600,
    generated: '2026-09-17T20:00:00', // 不带偏移量，按 UTC 读会差 8 小时
  });
  const v = evaluate(raw, NOW, STALE_HOURS_DEFAULT);
  assert.equal(v.bad, false, 'epoch 是新鲜的，就不该因为 ISO 的歧义报警');
});

test('带偏移量的 ISO 在没有 epoch 时可用', () => {
  const v = evaluate(doc({ generated: '2026-09-17T19:00:00+08:00' }), NOW, STALE_HOURS_DEFAULT);
  assert.equal(v.bad, false);
});

test('没有可用时间戳要报出来，而不是当成正常', () => {
  assert.equal(evaluate(doc({}), NOW, STALE_HOURS_DEFAULT).bad, true);
  assert.equal(evaluate(doc({ generated: '不是时间' }), NOW, STALE_HOURS_DEFAULT).bad, true);
});

test('status.json 不是合法 JSON 也要告警', () => {
  const v = evaluate('<html>502</html>', NOW, STALE_HOURS_DEFAULT);
  assert.equal(v.bad, true);
  assert.match(v.body!, /只发布了一半/);
});

// 相邻两档最长间隔 8.75h，夜里合盖次日中午补跑约 19h —— 26 小时不会误报
test('默认阈值不会被正常的夜间停顿触发', () => {
  const v = evaluate(doc({ generated_epoch: NOW / 1000 - 19 * 3600 }), NOW, STALE_HOURS_DEFAULT);
  assert.equal(v.bad, false);
});
