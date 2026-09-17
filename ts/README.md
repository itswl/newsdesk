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

## 认证

两条路，**不要把令牌直接敲在命令行里**——它会留在 shell history 里，也会被
任何记录终端的工具留存。

```bash
npx wrangler login              # 首选：浏览器授权一次，凭据存本地并自动轮换
```

必须用 API 令牌时（CI、或不想开浏览器），先落到文件再 source：

```bash
echo 'export CLOUDFLARE_API_TOKEN=...' > ~/.cf-token && chmod 600 ~/.cf-token
source ~/.cf-token && npx wrangler deploy
```

令牌权限要 `Account > Workers Scripts: Edit` 加 `Zone > Workers Routes: Edit`。

⚠️ **别用 wrangler 提示的 `--temporary`**：那会创建一个临时账户并部署过去，
路由和 secret 都不在那儿——看起来成功了，实际上线上一点没变。

⚠️ 别拿 `/user/tokens/verify` 判断令牌有没有效：它只认旧格式，`cfut_`/`cfat_`
新格式在那里报 Invalid 但实际调用完全正常。要验就用真实端点。

## secret 与部署的关系

`ALERT_WEBHOOK` 是 secret，跟脚本分开存。只要 Worker **名字不变**，重新部署
会沿用已有的 secret 和路由。改了名字就得重设——而漏设不会报错，
`selfCheck` 开头就 `if (!env.ALERT_WEBHOOK) return`，定时自查会静默地什么都不做，
正好是这一层专门要防的失效方式。部署后确认一次：

```bash
npx wrangler secret list
```

部署后前几十秒 200/308 混杂是边缘路由同步的过渡态，会自行一致，不要据此回滚。
