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
 * 定时自查。
 *
 * 这是整套告警里唯一**不在那台 Mac 上**的环节，也是唯一能发现「机器关了三天」
 * 的环节：本机的告警和日终核对都要调度器活着才发得出来，机器关了它们一起沉默。
 * status.json 让状态**可以**被外部监控，但可以被监控 ≠ 正在被监控。
 * Worker 本来就站在站点前面，天然在机器之外，由它来轮询这条线才闭合。
 */
export async function selfCheck(env: Env): Promise<void> {
  if (!env.ALERT_WEBHOOK) return; // 没配就安静跳过

  const threshold = Number(env.STALE_ALERT_HOURS || STALE_HOURS_DEFAULT);
  let verdict: Verdict;
  try {
    // cacheTtl: 0 —— 必须绕开自己的边缘缓存，否则读到的是刚才那份，
    // 「陈旧」这件事正好被自己藏住了
    const res = await fetch(origin(env) + '/status.json', { cf: { cacheTtl: 0 } });
    verdict = res.ok
      ? evaluate(await res.text(), Date.now(), threshold)
      : {
          bad: true,
          title: `❌ newsdesk 取不到状态文件 (HTTP ${res.status})`,
          body: '对象存储没有返回 status.json。桶被改动、被删、或权限变了。',
        };
  } catch (e) {
    verdict = {
      bad: true,
      title: '❌ newsdesk 自查请求失败',
      body: String(e instanceof Error ? e.message : e),
    };
  }

  // 成功也记一行：开了 observability 之后，这是唯一能在后台确认「cron 确实在跑」
  // 的凭据。只在出事时才有日志的话，「一直没日志」和「一切正常」长得一模一样。
  console.log(JSON.stringify({
    check: 'newsdesk', stale: verdict.bad, threshold,
    age_hours: verdict.age !== undefined ? Number(verdict.age.toFixed(2)) : null,
  }));

  if (verdict.bad) await notify(env, verdict.title!, verdict.body!);
}
