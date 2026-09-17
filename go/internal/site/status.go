package site

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/itswl/newsdesk/internal/store"
	"github.com/itswl/newsdesk/internal/task"
)

// TaskState 是调度器写进 state/schedule.json 的每任务状态。
type TaskState struct {
	Date     string `json:"date"`
	Status   string `json:"status"` // ok | failed | retrying | skipped
	Attempts int    `json:"attempts,omitempty"`
	Finished string `json:"finished,omitempty"`
	Engine   string `json:"engine,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type health struct {
	Heartbeat      string `json:"heartbeat"`
	HeartbeatEpoch int64  `json:"heartbeat_epoch"`
}

type dayState struct {
	Status   string // ok | draft | failed | retrying | skipped | pending
	At       string // HH:MM
	Attempts int
	Planned  string
}

// todayState 今天各任务的状态。
//
// **以「今天这份报告有没有产出」为准**，而不是只读 schedule.json。原因是顺序：
// 任务在自己末尾重建站点，而调度器要等它退出之后才把状态写成 ok——站点永远早
// 一步生成，刚跑完的那个任务的 chip 一直是旧的，要等下一档才变绿。
// 顺带也修好了手动跑的情况：那时 schedule.json 里根本没有记录。
//
// schedule.json 仍然要读——「失败 / 重试中 / 跳过」是文件表达不了的，
// 没有产出文件时才轮到它说话。
func (b *Builder) todayState() map[string]dayState {
	today := time.Now().Format("2006-01-02")
	sched := map[string]TaskState{}
	if raw, err := os.ReadFile(filepath.Join(b.o.Root, "state", "schedule.json")); err == nil {
		_ = json.Unmarshal(raw, &sched)
	}
	out := map[string]dayState{}
	for _, t := range task.All {
		// 状态条表达的是「任务今天跑没跑」，不是「翻译完没完」——
		// 所以固定看简体原文，英文版缺翻译不该显示成任务没跑。
		zh := *b
		zh.o.Lang = "zh-CN"
		if path, ok := zh.reportFile(today, t); ok {
			st, _ := os.Stat(path)
			at := st.ModTime().Format("15:04")
			status := "ok"
			if strings.Contains(filepath.Base(path), "_draft") {
				status = "draft"
			}
			out[t.Key] = dayState{Status: status, At: at}
			continue
		}
		rec := sched[t.Key]
		if rec.Date == today && rec.Status != "" && rec.Status != "ok" {
			out[t.Key] = dayState{Status: rec.Status, Attempts: rec.Attempts}
			continue
		}
		out[t.Key] = dayState{Status: "pending", Planned: t.At}
	}
	return out
}

// statusBar 无人值守的东西必须把失败摆在看得见的地方——只写进日志等于没有告警。
// 顺序按触发时刻排：状态条读起来是一条时间线，左边已跑过、右边还没到。
// 用板块名顺序的话，跑完的会被没跑的隔开（✓ · · ✓），看着像出了错。
func (b *Builder) statusBar() string {
	state := b.todayState()
	mark := map[string][2]string{
		"ok": {"ok", "✓"}, "draft": {"warn", "✎"}, "failed": {"bad", "✘"},
		"retrying": {"warn", "…"}, "skipped": {"warn", "⊘"}, "pending": {"pend", "·"},
	}
	var sb strings.Builder
	bad := false
	for _, t := range task.All {
		s := state[t.Key]
		m := mark[s.Status]
		var tip string
		switch s.Status {
		case "ok", "draft":
			tip = b.T("st."+s.Status, s.At)
		case "failed":
			tip = b.T("st.failed", fmt.Sprint(s.Attempts))
			bad = true
		case "retrying":
			tip = b.T("st.retry", fmt.Sprint(s.Attempts))
		case "skipped":
			tip = b.T("st.skipped")
			bad = true
		default:
			tip = b.T("st.pending")
			if s.Planned != "" {
				tip += b.T("st.planned", s.Planned)
			}
		}
		fmt.Fprintf(&sb, `<span class="st %s" title="%s"><i class="m">%s</i>%s</span>`,
			m[0], esc(tip), m[1], esc(b.T("short."+t.Key)))
	}
	alert := ""
	if bad {
		alert = " alert"
	}
	return fmt.Sprintf(`<div class="status%s">%s</div>`, alert, sb.String())
}

type statusDoc struct {
	Generated      string               `json:"generated"`
	GeneratedEpoch int64                `json:"generated_epoch"`
	Date           string               `json:"date"`
	Heartbeat      string               `json:"heartbeat,omitempty"`
	StaleHours     *float64             `json:"stale_hours"`
	Tasks          map[string]statusRow `json:"tasks"`
	OK             bool                 `json:"ok"`
}

type statusRow struct {
	Status   string `json:"status"`
	Finished string `json:"finished,omitempty"`
}

// writeStatus 给外部监控用的机器可读状态。
//
// 本机的调度器报不了自己的死：机器关了、进程被杀，它没机会发任何告警。
// 这个文件随站点发布，外部监控轮询它、看时间戳是否变陈旧，才能覆盖「整机不在」。
//
// ⚠️ 时间戳带时区并另给 epoch。消费者是跑在 UTC 里的 Worker——不带偏移量的 ISO
// 会被 JS 当成它自己的本地时间（即 UTC），北京时间算出来差整整 8 小时，
// 结果是机器关了 26 小时它只看到 18 小时，监控永远晚一个时区才响。
func (b *Builder) writeStatus() error {
	var h health
	if raw, err := os.ReadFile(filepath.Join(b.o.Root, "state", "health.json")); err == nil {
		_ = json.Unmarshal(raw, &h)
	}
	now := time.Now()
	doc := statusDoc{
		Generated:      now.Format(time.RFC3339),
		GeneratedEpoch: now.Unix(),
		Date:           now.Format("2006-01-02"),
		Heartbeat:      h.Heartbeat,
		Tasks:          map[string]statusRow{},
	}
	if h.HeartbeatEpoch > 0 {
		s := float64(now.Unix()-h.HeartbeatEpoch) / 3600
		s = float64(int(s*100+0.5)) / 100
		doc.StaleHours = &s
	}
	// 没有心跳文件 = 从没跑过调度器（只手动跑），这不是故障，不该永远 ok=false。
	// 有心跳但已经陈旧才是故障：进程还在写状态却停止工作了。
	allGood := doc.StaleHours == nil || *doc.StaleHours < 1
	for k, v := range b.todayState() {
		doc.Tasks[k] = statusRow{Status: v.Status, Finished: v.At}
		if v.Status != "ok" && v.Status != "draft" && v.Status != "pending" {
			allGood = false
		}
	}
	doc.OK = allGood
	if err := store.WriteJSON(filepath.Join(b.o.Out, "status.json"), doc, true); err != nil {
		return err
	}
	stale := "调度器未运行"
	if doc.StaleHours != nil {
		stale = fmt.Sprintf("心跳 %.2f 小时前", *doc.StaleHours)
	}
	b.logf("  status.json  (%s, ok=%v)\n", stale, doc.OK)
	return nil
}
