# newsdesk

每日简报流水线。四路数据源定时采集，交给大模型分析成报告，合成三语静态站发布。

```
采集（联网）→ data/<源>/<日期>/      原始数据 + 跨期比对
分析（沙箱内，离线）→ reports/<日期>/*.md
渲染 → site/                        三语静态站 + Atom feed
发布 → 对象存储 → Cloudflare Worker → 自己的域名
```

| 时间 | 任务 | 内容 |
|---|---|---|
| 06:30 | `ai` | 15 个 RSS/Atom 源 + HN Algolia，36 小时窗 |
| 11:35 | `douban` | 豆瓣热门电影，评分与评价人数的跨期漂移 |
| 16:40 | `trending` | GitHub Trending，星标速率、停更判定、许可证 |
| 21:45 | `momoyu` | 13 个中文热榜聚合，含各源滞后标注 |

**零第三方依赖。** Go 标准库跑完整条本地流水线——Markdown 渲染、繁体转换、
对象存储签名、HTTP 采集全是自己的。TypeScript 只用于 Cloudflare Worker，
运行时同样没有依赖。

## 开始

```bash
make build                          # 产出 bin/newsdesk
cp config.example.conf config.conf  # 按注释填，全部项都可留空
./bin/newsdesk doctor               # 逐条实测配置、时间表、沙箱边界
```

跑一档看看（需要本机装了 `claude` 或 `codex` 并已登录）：

```bash
./bin/newsdesk fetch douban     # 只采集
./bin/newsdesk run douban       # 采集 → 模型 → 建站
open site/index.html
```

常驻：

```bash
./bin/newsdesk schedule         # 睡过某一档，唤醒后会补跑
./bin/newsdesk status
```

只在本机看的话到此为止——不用配桶、不用配域名，`site/` 用浏览器直接打开。

## 订阅

站点提供 Atom feed，三种语言各一份。任何阅读器（NetNewsWire、Reeder、Inoreader、
Feedly、Miniflux…）都能订。全文推送，不用点进网页。

| 语言 | 地址 |
|---|---|
| 简体中文 | `https://<你的域名>/feed.xml` |
| 繁體中文 | `https://<你的域名>/zh-TW/feed.xml` |
| English | `https://<你的域名>/en/feed.xml` |

**想三种都订：** 分别添加上面三个地址即可，它们是三个独立的 feed，条目 `id` 互不
相同，阅读器不会把它们当成重复内容。想都订又不想看三遍，多数阅读器支持把它们
放进同一个文件夹折叠显示。

**只想要某一天的网页：** `https://<你的域名>/2026-09-17.html`，繁体与英文分别在
`/zh-TW/` 和 `/en/` 下。首页是最新一天，`/archive.html` 是全部索引。

线上默认只展示最近 7 天（`PUBLIC_DAYS`），超窗的页面会从公开桶删除。
原始报告与本地站点不受影响，是全量的。

## 对外发布（可选）

需要一个对象存储桶和一个 Cloudflare 域名。

```bash
# 1. 填 config.conf 的 BACKUP_BUCKET / PUBLISH_BUCKET / SITE_URL / SITE_LANGS
./bin/newsdesk publish --dry-run

# 2. Worker
cd ts && cp wrangler.example.toml wrangler.toml
printf '%s' "$WEBHOOK" | npx wrangler secret put ALERT_WEBHOOK
npx wrangler deploy
```

公开桶设成 `ObjectReadWithoutList`（可读不可列表）。Worker 负责默认文档、边缘缓存、
三语错误页，以及**定时自查**——每 6 小时拉一次 `status.json`，超过 26 小时没有新产出
就告警。这条是唯一不在本机上的监控：机器关了、调度器挂了，本机的告警一起沉默。

## 语言

简体是模型直接产出；繁体由简体现转（台湾用词，转换表编译进二进制，零调用）；
英文预先翻好，走按量付费端点，不占订阅的用量窗口。

`SITE_LANGS` 里列出来还不够，依赖不齐的语言会被剔除并说明原因，其余照常发布：

```bash
./bin/newsdesk langs
```

## 开发

```bash
make test     # Go 110 + TS 34
make check    # 加上 fmt / vet / tsc
```

改动前先读 [AGENTS.md](AGENTS.md)——里面是每一条约束背后的原因，以及踩过的坑。

```
go/internal/fetch      四个采集器，所有联网都在这里
go/internal/engine     调用模型，每个引擎恰好一层沙箱
go/internal/sandbox    拒绝清单的唯一事实来源
go/internal/schedule   调度器，核心是睡眠补跑
go/internal/markdown   自己写的渲染器，不放行原始 HTML
go/internal/site       三语页面、归档、Atom feed、状态文件
go/internal/oci        对象存储客户端，标准库实现请求签名
ts/src                 Cloudflare Worker
prompts/               四个任务的分析契约——方法论的坑最终落在这里
```

## 前身

这套东西最早是 Python 写的：[itswl/newsdesk](https://github.com/itswl/newsdesk)，
已归档在 tag `python-reference`。功能相同，那边的 `tests/` 是行为规格，
`AGENTS.md` 是同一批约束的另一种写法。

它的 README 末尾有一节把「换语言到底换到了什么」算了细账：真正由语言带来的只有
依赖归零（这个版本是单个二进制，那个要 `.venv` 加一百多兆的 `oci` CLI），其余收益
来自「重写」而不是「换语言」，而重写本身引入了三个新 bug、行数翻了一倍。

那节给的判据是：换语言划算的情形只有一种——**依赖链本身就是故障源，而这东西要长期
无人值守地跑**。不符合就别照抄。

## 授权

代码 [MIT](LICENSE)。

报告正文中引用的第三方标题、简介、链接归各自权利人所有，仅作事实性引用与评述。
公开站点每页页脚给出联系方式，权利人可要求移除。
