# newsdesk Worker

把公开桶反代成自己的域名，并在 Cloudflare 上跑站点的存活自查。

## 为什么需要它

对象存储没有「默认文档」概念，访问 `/` 不会返回 `index.html`；桶在 us-phoenix-1，
亚洲直连慢，边缘缓存之后只有回源那一次慢。

另一半理由在 `src/selfcheck.ts`：**这是整条告警链里唯一不在那台机器上的环节。**
本机的告警和日终核对都要调度器活着才发得出来——机器关了，它们一起沉默。
Worker 本来就站在站点前面，天然在机器之外，由它定时轮询 `status.json`，
「整机不在」这一类故障才有人知道。

## 目录

```
src/env.ts        桶坐标与配置读取
src/paths.ts      默认文档、路径白名单、缓存时长、content-type
src/errors.ts     三语错误页（语言入口跟随 SITE_LANGS）
src/notify.ts     告警发送，含「HTTP 200 但 body 里 code 非 0」的判定
src/selfcheck.ts  定时自查：站点多久没更新算出事
src/index.ts      入口，导出 fetch 与 scheduled
```

## 开发

```bash
npm install          # 只装开发依赖，运行时不需要
npm test             # node 内置测试器直接跑 .ts，34 个用例
npm run typecheck
```

测试不依赖网络，也不依赖 workerd —— 回源被替换成假的，只验 Worker 自己的行为。

## 部署

```bash
cp wrangler.example.toml wrangler.toml   # 填桶、域名、语言
printf '%s' "$WEBHOOK" | npx wrangler secret put ALERT_WEBHOOK
npx wrangler deploy
```

令牌需要 `Account > Workers Scripts: Edit` 加 `Zone > Workers Routes: Edit`，
走环境变量 `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID` 传入。

部署后前几十秒 200/308 混杂是边缘路由同步的过渡态，会自行一致，不要据此回滚。
