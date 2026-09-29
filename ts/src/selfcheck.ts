import { origin, type Env } from './env.ts';
import { notify } from './notify.ts';

/**
 * 站点多久没更新算出事。
 *
 * 四个任务里相邻两档最长间隔 8.75 小时（21:45 → 次日 06:30），机器夜里合盖、
 * 次日中午才补跑也就约 19 小时，所以 26 小时不会误报，又能在「整整一天没动静」
 * 时叫出来。
 */
export const STALE_HOURS_DEFAULT = 26;

export interface Verdict {
  bad: boolean;
  age?: number;
  title?: string;
  body?: string;
}

interface StatusDoc {
  generated?: string;
  generated_epoch?: number;
  date?: string;
  tasks?: Record<string, { status?: string; finished?: string }>;
}

export function evaluate(text: string, nowMs: number, thresholdHours: number): Verdict {
  let doc: StatusDoc;
  try {
    doc = JSON.parse(text) as StatusDoc;
  } catch {
    return {
      bad: true,
      title: '❌ newsdesk 状态文件读不出来',
      body: `status.json 不是合法 JSON，站点可能只发布了一半。\n收到：${text.slice(0, 200)}`,
    };
  }

  // ⚠️ epoch 优先：它没有时区歧义。退回 ISO 时依赖字段自带偏移量——不带偏移量的
  // ISO 会被 Worker（跑在 UTC）当成 UTC，北京时间算出来差整整 8 小时，
  // 结果是刚生成的站点被报成「未来」或「停更」。
  const ts = doc.generated_epoch ? doc.generated_epoch * 1000 : Date.parse(doc.generated ?? '');
  if (!ts || Number.isNaN(ts)) {
    return {
      bad: true,
      title: '❌ newsdesk 状态文件没有可用时间戳',
      body: 'generated_epoch 与 generated 都取不到，无法判断新旧。',
    };
  }

  const age = (nowMs - ts) / 3.6e6;
  if (age <= thresholdHours) return { bad: false, age };

  const tasks = Object.entries(doc.tasks ?? {})
    .map(([k, v]) => `  ${k}: ${v.status}${v.finished ? ' ' + v.finished : ''}`).join('\n');
  return {
    bad: true, age,
    title: `❌ newsdesk 已停更 ${age.toFixed(1)} 小时`,
    body: `站点最后一次更新是 ${doc.generated}（${doc.date}），已超过 ${thresholdHours} 小时没有新产出。\n`
      + '常见原因：那台机器关机或长时间睡眠、调度器进程没了。\n\n'
      + `最后一次已知的任务状态：\n${tasks}`,
  };
}

/**
 * 同一次停更多久才再告警一次。
 *
 * cron 为了及时发现新简报提到了 10 分钟一次，但停更是**一个持续状态**：不节流的话
 * 机器关一天就是 144 条飞书消息，而告警一旦变噪声就会被忽略，那比迟钝危险得多。
 * 6 小时沿用提频之前的节奏。
 */
export const RENOTIFY_HOURS = 6;
const ALERTED_KEY = 'alerted:stale';
/** wrangler.toml 里 cron 的间隔。只在节流状态存不进 KV 时用来退回按钟点节流 */
const CRON_EVERY_MS = 10 * 60e3;

/**
 * 停更自查。
 *
 * 这是整套告警里唯一**不在那台 Mac 上**的环节，也是唯一能发现「机器关了三天」
 * 的环节：本机的告警和日终核对都要调度器活着才发得出来，机器关了它们一起沉默。
 * status.json 让状态**可以**被外部监控，但可以被监控 ≠ 正在被监控。
 * Worker 本来就站在站点前面，天然在机器之外，由它来轮询这条线才闭合。
 *
 * ⚠️ 这条走飞书，不走推送：停更意味着机器可能已经不在了，要去人一定会看的地方。
 * 手机推送只用来报「新简报出了」。
 */
export async function staleWatch(env: Env, got: { ok: boolean; status: number; text: string; error?: string },
                                 nowMs = Date.now()): Promise<void> {
  if (!env.ALERT_WEBHOOK) return; // 没配就安静跳过

  const threshold = Number(env.STALE_ALERT_HOURS || STALE_HOURS_DEFAULT);
  let verdict: Verdict;
  if (got.error !== undefined) {
    verdict = { bad: true, title: '❌ newsdesk 自查请求失败', body: got.error };
  } else if (!got.ok) {
    verdict = {
      bad: true,
      title: `❌ newsdesk 取不到状态文件 (HTTP ${got.status})`,
      body: '对象存储没有返回 status.json。桶被改动、被删、或权限变了。',
    };
  } else {
    verdict = evaluate(got.text, nowMs, threshold);
  }

  // 成功也记一行：开了 observability 之后，这是唯一能在后台确认「cron 确实在跑」
  // 的凭据。只在出事时才有日志的话，「一直没日志」和「一切正常」长得一模一样。
  console.log(JSON.stringify({
    check: 'newsdesk', stale: verdict.bad, threshold,
    age_hours: verdict.age !== undefined ? Number(verdict.age.toFixed(2)) : null,
  }));

  if (!verdict.bad) {
    // 恢复了就清掉节流，下次出事立刻响。先读再删：cron 每 10 分钟一次，无条件
    // delete 一天就是 144 次，而免费档 KV 的删除额度一天只有 1000 次（读有十万次）
    if (env.PUSH) {
      try {
        if (await env.PUSH.get(ALERTED_KEY)) await env.PUSH.delete(ALERTED_KEY);
      } catch (e) {
        console.log('停更节流清不掉，下次停更可能晚响一阵', e instanceof Error ? e.message : e);
      }
    }
    return;
  }
  if (env.PUSH && !(await claimAlert(env.PUSH, nowMs))) return;
  await notify(env, verdict.title!, verdict.body!);
}

/**
 * 这一轮该不该发停更告警；该发就顺手记下时刻。
 *
 * ⚠️ KV 读写失败（比如当天写额度用完）时绝不能因此把告警吞掉——以前先写节流、
 * 写失败就抛，停更告警一条都发不出去。也不能退成每 10 分钟一条，于是退回按钟点
 * 节流：每 RENOTIFY_HOURS 的头一个 cron 间隔里才发。
 */
async function claimAlert(kv: KVNamespace, nowMs: number): Promise<boolean> {
  const every = RENOTIFY_HOURS * 3.6e6;
  try {
    const last = Number(await kv.get(ALERTED_KEY)) || 0;
    if (last && nowMs - last < every) return false;
    await kv.put(ALERTED_KEY, String(nowMs));
    return true;
  } catch (e) {
    console.log('停更节流读写失败，改按钟点节流', e instanceof Error ? e.message : e);
    return nowMs % every < CRON_EVERY_MS;
  }
}

/** 自己取一次再自查。给不经 cron 的调用方用（cron 走 cron.ts，只取一次喂两条线）。 */
export async function selfCheck(env: Env): Promise<void> {
  const { fetchStatus } = await import('./cron.ts');
  await staleWatch(env, await fetchStatus(env));
}
