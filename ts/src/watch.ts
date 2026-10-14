// 盯着 status.json，发现某一档出了新报告就推一条。
//
// 为什么由 Worker 来盯而不是发布完由那台 Mac 主动敲：订阅存在 KV 里，只有 Worker
// 够得着；而 Worker 本来就在定时拉 status.json 做停更自查，顺路比对即可，
// 流水线那边一行都不用改。代价是最多一个轮询间隔的延迟。
import type { Env } from './env.ts';
import { errText, push, recordPush, type FanOut, type PushLogEntry } from './push.ts';

export interface TaskRow { status?: string; finished?: string; label?: string }
export interface StatusDoc { date?: string; tasks?: Record<string, TaskRow> }

/** 上次已通知到哪一步。按日期加档位记，跨日要重新算 */
export interface Seen { date?: string; tasks?: Record<string, string> }

const hasReport = (s?: string) => s === 'ok' || s === 'draft';

/**
 * 算出这一轮新出现的报告。
 *
 * ⚠️ 判据是「从没有报告变成有报告」，不是「状态变了」：trending 那一档可能先出草稿
 * 再出正式版（draft → ok），按状态变化算会为同一天的同一档推两条。
 *
 * ⚠️ 跨日必须把基线清空：新的一天各档回到 pending，若拿昨天的 ok 当基线，
 * 今早的 ai 会被看成「没变化」而漏推。
 */
export function newReports(doc: StatusDoc, seen: Seen): { key: string; label: string; finished: string }[] {
  const base = seen.date === doc.date ? (seen.tasks ?? {}) : {};
  const out: { key: string; label: string; finished: string }[] = [];
  for (const [key, row] of Object.entries(doc.tasks ?? {})) {
    if (hasReport(row.status) && !hasReport(base[key])) {
      out.push({ key, label: row.label || key, finished: row.finished || '' });
    }
  }
  return out;
}

export function snapshot(doc: StatusDoc): Seen {
  const tasks: Record<string, string> = {};
  for (const [k, v] of Object.entries(doc.tasks ?? {})) tasks[k] = v.status ?? '';
  return { date: doc.date, tasks };
}

const SEEN_KEY = 'seen:reports';

/**
 * 比对并推送。
 *
 * 第一次跑（KV 里没有基线）只记录不推送：否则刚上线那一刻会把当天已经完成的
 * 几档一次性全推出来。
 *
 * ⚠️ **先记基线、再推送。** 反过来的话，推出去了却记不住（当天 KV 写额度用完、
 * 服务出错），下一轮会把同样几档再推一遍，每轮 cron 一次。先记的话，记不住就这一轮
 * 先不推、下一轮再试，一条都没发出去，重试也不会重复；记住之后推送失败的那一档
 * 就漏了（原因在日志里）——宁可漏一条，不能每轮都重推。
 */
export async function watchReports(env: Env, text: string): Promise<void> {
  if (!env.PUSH) return;
  let doc: StatusDoc;
  try { doc = JSON.parse(text) as StatusDoc; } catch { return; } // 坏 JSON 由停更自查去告警
  if (!doc.date) return;

  const raw = await env.PUSH.get(SEEN_KEY);
  const first = !raw;
  let seen: Seen = {};
  if (raw) { try { seen = JSON.parse(raw) as Seen; } catch { /* 当成没有基线 */ } }

  // 基线推进到当前状态，但**只在真的变了时才写**：无条件写的话每轮 cron 都写一次，
  // 而免费档 KV 一天只有 1000 次写入、整个账号共用。status.json 的键由 Go 按字母序
  // 输出，同样的状态序列化出来逐字相同。有新报告时状态必然变了，所以这里一定会写。
  const next = JSON.stringify(snapshot(doc));
  if (next !== raw) await env.PUSH.put(SEEN_KEY, next); // 写不进去就抛：这一轮不推

  const fresh = first ? [] : newReports(doc, seen);
  const log: PushLogEntry[] = [];
  for (const r of fresh) {
    let out: FanOut;
    try {
      out = await push(env, {
        title: `${r.label} 已更新`,
        body: r.finished ? `${doc.date} ${r.finished} 出炉` : String(doc.date),
        url: `/#${r.key}`,
        // 同一档的通知互相覆盖，不在锁屏上堆成一列
        tag: `newsdesk-${r.key}`,
      });
    } catch (e) {
      // 只可能是读订阅列表那一步出错（投递本身从不抛），这时一条都还没发出去
      out = { subs: 0, sent: 0, gone: 0, failed: 0, errors: [`读不到订阅列表，这一档没有推送：${errText(e)}`] };
    }
    log.push({ at: new Date().toISOString(), kind: 'report', task: r.key, date: doc.date, ...out });
  }
  // 日志一轮只写一次：KV 同一个键一秒只许写一次，以前每推一档写一次，同一轮出两档时
  // 第二次写被拒（429），整轮中断
  await recordPush(env, ...log);
}
