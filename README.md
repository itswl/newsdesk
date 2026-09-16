# newsdesk2

每日简报流水线的 Go + TypeScript 实现。

采集与调度用 Go，渲染与 Worker 用 TS。两半通过文件系统通信——这个缝在 Python 版里
就已经存在（渲染侧只读 `reports/` 和 `state/`，完全不碰 `data/`），换语言只是顺着它切。

```
Go   ──写──→  data/<源>/<日期>/     原始数据 + history.json
              reports/<日期>/*.md    模型产出
              state/*.json           调度状态 + 心跳
TS   ──读──→  reports/ + state/
     ──写──→  site/  site-public/
```

参考实现：`itswl/newsdesk` 的 `python-reference` tag。方法论契约（`prompts/`）与
踩过的坑（`AGENTS.md`）从那边原样带过来——它们跟语言无关，是这个项目最贵的部分。
