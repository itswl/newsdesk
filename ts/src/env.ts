// 桶的坐标与告警配置由 wrangler.toml 的 [vars] 与 secret 注入。
// 脚本里不写死任何环境特有值——换个桶、换个域名不该改代码。
export interface Env {
  OCI_REGION: string;
  OCI_NAMESPACE: string;
  OCI_BUCKET: string;

  /** 错误页上的语言入口跟着它走，要与 config.conf 的 SITE_LANGS 一致 */
  SITE_LANGS?: string;
  /** 站点多久没更新算出事（小时） */
  STALE_ALERT_HOURS?: string;

  /** 告警地址。是能往群里发消息的凭据，用 `wrangler secret put` 注入，不写进 toml */
  ALERT_WEBHOOK?: string;
  ALERT_PAYLOAD?: string;
}

export function origin(env: Env): string {
  return `https://objectstorage.${env.OCI_REGION}.oraclecloud.com`
    + `/n/${env.OCI_NAMESPACE}/b/${env.OCI_BUCKET}/o`;
}

export function langs(env: Pick<Env, 'SITE_LANGS'>): string[] {
  return String(env.SITE_LANGS || 'zh-CN').split(',').map(s => s.trim()).filter(Boolean);
}
