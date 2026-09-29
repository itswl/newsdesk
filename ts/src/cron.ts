// 定时任务的编排：一次取回 status.json，喂给两条互不相干的线。
//
// 分成两条是有意的：**停更告警走飞书，新简报走推送**。前者是「机器没了」，
// 要去人一定看得到的地方；后者是内容提醒，弹在手机上正好。
import { origin, type Env } from './env.ts';
import { redact } from './notify.ts';
import { errText } from './push.ts';
import { staleWatch } from './selfcheck.ts';
import { watchReports } from './watch.ts';

export interface Fetched { ok: boolean; status: number; text: string; error?: string }

export async function fetchStatus(env: Env): Promise<Fetched> {
  try {
    // cacheTtl: 0 —— 必须绕开自己的边缘缓存，否则读到的是刚才那份，
    // 「陈旧」这件事正好被自己藏住了
    const res = await fetch(origin(env) + '/status.json', { cf: { cacheTtl: 0 } });
    return { ok: res.ok, status: res.status, text: res.ok ? await res.text() : '' };
  } catch (e) {
    return { ok: false, status: 0, text: '', error: errText(e) };
  }
}

export async function tick(env: Env): Promise<void> {
  const got = await fetchStatus(env);
  // 两条线互不牵连：一条抛了异常（多半是 KV 出错），另一条照跑。以前停更自查
  // 一抛，新简报推送这一轮就跟着没了
  await staleWatch(env, got).catch(e => console.log('停更自查异常', redact(errText(e), env)));
  if (got.ok) await watchReports(env, got.text).catch(e => console.log('新简报推送异常', errText(e)));
}
