// 定时任务的编排：一次取回 status.json，喂给两条互不相干的线。
//
// 分成两条是有意的：**停更告警走飞书，新简报走推送**。前者是「机器没了」，
// 要去人一定看得到的地方；后者是内容提醒，弹在手机上正好。
import { origin, type Env } from './env.ts';
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
    return { ok: false, status: 0, text: '', error: String(e instanceof Error ? e.message : e) };
  }
}

export async function tick(env: Env): Promise<void> {
  const got = await fetchStatus(env);
  await staleWatch(env, got);
  if (got.ok) await watchReports(env, got.text);
}
