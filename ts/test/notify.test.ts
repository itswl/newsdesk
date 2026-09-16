import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildPayload, redact, verdictOf } from '../src/notify.ts';

// 正文里一个换行或引号就能把 payload 撑坏，而撑坏的结果是告警发不出去，
// 恰好在最需要它的时候。
test('{{TEXT}} 按 JSON 规则转义后再代入', () => {
  const tpl = '{"msg_type":"text","content":{"text":"{{TEXT}}"}}';
  const out = buildPayload('标题', '第一行\n带"引号"和\\反斜杠', tpl);
  const parsed = JSON.parse(out) as { content: { text: string } };
  assert.equal(parsed.content.text, '标题\n第一行\n带"引号"和\\反斜杠');
});

test('没有模板时用默认体', () => {
  const parsed = JSON.parse(buildPayload('T', 'B')) as { title: string; text: string };
  assert.equal(parsed.title, 'T');
  assert.equal(parsed.text, 'T\nB');
});

test('模板里出现多次占位都要替换', () => {
  const out = buildPayload('T', 'B', '{"a":"{{TITLE}}","b":"{{TITLE}}","c":"{{TEXT}}"}');
  const p = JSON.parse(out) as Record<string, string>;
  assert.equal(p.a, 'T');
  assert.equal(p.b, 'T');
});

// ⚠️ 飞书/Lark、钉钉、企业微信错误也返回 HTTP 200，失败写在 body 的 code 里。
// 只看状态码的话通道断了也一直以为发出去了——告警通道静默失效比没有告警更糟。
test('HTTP 200 但 body 里 code 非 0 算失败', () => {
  assert.ok(verdictOf(200, true, '{"code":19024,"msg":"invalid token"}'));
  assert.ok(verdictOf(200, true, '{"errcode":300001,"errmsg":"bad"}'));
  assert.equal(verdictOf(200, true, '{"code":0,"msg":"success"}'), null);
  assert.equal(verdictOf(200, true, '{"StatusCode":0}'), null);
});

test('非 JSON 响应按成功处理（Slack 返回纯文本 ok）', () => {
  assert.equal(verdictOf(200, true, 'ok'), null);
});

test('HTTP 层失败也要认出来', () => {
  assert.match(verdictOf(404, false, '') ?? '', /HTTP 404/);
});

// 日志会被 Cloudflare 收走存 3 天；webhook 是能往群里发消息的凭据
test('日志里抹掉 webhook', () => {
  const env = { ALERT_WEBHOOK: 'https://hook.example.com/abc123' };
  const out = redact('请求 https://hook.example.com/abc123 失败', env);
  assert.ok(!out.includes('abc123'));
  assert.ok(out.includes('<ALERT_WEBHOOK>'));
});

test('没配 webhook 时 redact 不炸', () => {
  assert.equal(redact('x', {}), 'x');
});
