// 桶的坐标与告警配置由 wrangler.toml 的 [vars] 与 secret 注入。
// 脚本里不写死任何环境特有值——换个桶、换个域名不该改代码。
export interface Env {
  OCI_REGION: string;
  OCI_NAMESPACE: string;
  OCI_BUCKET: string;

  /** 站点多久没更新算出事（小时） */
  STALE_ALERT_HOURS?: string;

  /** 告警地址。是能往群里发消息的凭据，用 `wrangler secret put` 注入，不写进 toml */
  ALERT_WEBHOOK?: string;
  ALERT_PAYLOAD?: string;

  /**
   * 推送订阅的存放处。没有它整条推送链路静默关闭（页面上的开关也不会出现）。
   *
   * ⚠️ 它和 [triggers] 的 cron 频率是一对：新简报提醒靠轮询 status.json 发现，
   * 所以 cron 比停更自查本身需要的密（每小时）；而停更告警的节流状态也存在这里。
   * 没有 KV 就别把 cron 调密——那会让同一次停更每轮都往飞书推一条。
   */
  PUSH?: KVNamespace;
  /** VAPID 公钥（未压缩点 base64url）。公开值：页面里也有一份，订阅时要用 */
  PUSH_VAPID_PUBLIC?: string;
  /** VAPID 私钥（PKCS8 base64url）。`wrangler secret put`，绝不进 toml */
  PUSH_VAPID_PRIVATE_KEY?: string;
  /** VAPID 的 sub 声明，推送服务出问题时它们照这个联系你。mailto: 或 https: */
  PUSH_SUB?: string;
}

export function origin(env: Env): string {
  return `https://objectstorage.${env.OCI_REGION}.oraclecloud.com`
    + `/n/${env.OCI_NAMESPACE}/b/${env.OCI_BUCKET}/o`;
}
