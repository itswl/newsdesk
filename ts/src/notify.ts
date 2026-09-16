import type { Env } from './env.ts';

/**
 * 日志会被 Cloudflare 收走存 3 天。ALERT_WEBHOOK 是能往群里发消息的凭据，
 * 异常信息里一旦带上它就等于把凭据写进了日志库，所以统一抹掉再打印。
 */
export function redact(text: unknown, env: Pick<Env, 'ALERT_WEBHOOK'>): string {
  const s = String(text);
  return env.ALERT_WEBHOOK ? s.split(env.ALERT_WEBHOOK).join('<ALERT_WEBHOOK>') : s;
}

/**
 * 构造告警体。
 *
 * {{TEXT}} 落在 JSON 字符串内部，必须按 JSON 规则转义——正文里一个换行或
 * 引号就能把 payload 撑坏，而撑坏的结果是告警发不出去，恰好在最需要它的时候。
 */
export function buildPayload(title: string, text: string, tpl?: string): string {
  const body = `${title}\n${text}`;
  if (!tpl) return JSON.stringify({ title, text: body });
  const inner = (s: string) => JSON.stringify(s).slice(1, -1);
  return tpl.split('{{TEXT}}').join(inner(body)).split('{{TITLE}}').join(inner(title));
}

/**
 * 判断一次告警发送是否真的成功。
 *
 * ⚠️ 光看状态码不够：飞书/Lark、钉钉、企业微信**错误也返回 HTTP 200**，
 * 失败写在 body 的 code/errcode 里。只看状态码的话通道断了也一直以为发出去了——
 * 告警通道静默失效比没有告警更糟，因为你以为自己有。
 */
export function verdictOf(status: number, ok: boolean, text: string): string | null {
  if (!ok) return `HTTP ${status}`;
  try {
    const d = JSON.parse(text) as Record<string, unknown>;
    const code = d.code ?? d.errcode ?? d.StatusCode;
    if (code !== undefined && Number(code) !== 0) return `服务端拒绝: ${text.slice(0, 200)}`;
  } catch {
    // 非 JSON（Slack 返回纯文本 ok）按成功处理
  }
  return null;
}

export async function notify(env: Env, title: string, text: string): Promise<void> {
  if (!env.ALERT_WEBHOOK) return;
  try {
    const r = await fetch(env.ALERT_WEBHOOK, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: buildPayload(title, text, env.ALERT_PAYLOAD),
    });
    const bad = verdictOf(r.status, r.ok, await r.text());
    if (bad) console.log('告警发送失败', redact(bad, env));
  } catch (e) {
    // 自查失败不能反过来影响站点
    console.log('告警发送异常', redact(e instanceof Error ? e.message : e, env));
  }
}
