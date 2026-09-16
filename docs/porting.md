# 从 Python 版移植

参考实现：`itswl/newsdesk` 的 `python-reference` tag。

## 切分

渲染侧只读 `reports/` 和 `state/`，完全不碰 `data/`——这个缝在 Python 版里就存在，
换语言只是顺着它切，不是硬凑。

| | 语言 | 模块 |
|---|---|---|
| 采集 / 调度 | Go | `fetch_*` `scheduler` `history` `leakcheck` `alert` `prune` |
| 渲染 / 边缘 | TS | `build_site` `i18n` `render` `worker` |
| 不动 | — | `prompts/` `AGENTS.md` 沙箱那套 shell |

沙箱不移植：它是平台特定（seatbelt / bubblewrap）且安全关键，拿它练手不划算。

## 已移植

- [x] `config` — 重复键现在**报错**。Python 版有六份解析器加三处 shell source，
      重复键时 Python 取第一个、shell 取最后一个，换网关时两个组件因此读到不同端点，
      而且完全静默。
- [x] `fetch douban` — 与 Python 版产出对拍：77 个顶层字段完全一致，
      73/77 值逐字节相同，不同的 4 个都是随时间变的计数器。

## 待移植

`fetch momoyu / trending / ainews` · `history` · `scheduler` · `alert` · `leakcheck`
· `prune`（Go）；`render` · `i18n` · `build_site` · `worker`（TS）

## 怎么验证移植对了

`itswl/newsdesk` 里的 `bin/difftest.sh` 跑两个实现比对输出：`out` 比 stdout，
`dir` 比产出目录。这个项目的产出是确定性的，所以「新实现正确」可以被证明。

移植时有意改掉的地方（不做忠实照搬）要记在这里，否则对拍失败时分不清是移植错了
还是有意为之。
