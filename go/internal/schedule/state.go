// Package schedule 是调度器：纯标准库常驻进程，不依赖 launchd 或 cron。
//
// 关键行为是**睡眠补跑**：不是「到点触发」，而是每 30 秒判定「今天这个任务
// 该跑了吗、跑过了吗」——合盖睡过某一档，唤醒后会立即补跑。
package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type TaskState struct {
	Date     string `json:"date"`
	Status   string `json:"status"` // ok | failed | retrying | skipped
	Attempts int    `json:"attempts,omitempty"`
	Finished string `json:"finished,omitempty"`
	Engine   string `json:"engine,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Danger 记下这一档是不是在危险模式（DANGER=1）下跑的。
	//
	// 报告公开发布、沙箱与凭据拒读当时都是关着的——事后要能回答「这份是哪一种
	// 跑出来的」，而日志会滚、status.json 只反映当下。落在状态文件里才留得住。
	Danger      bool    `json:"danger,omitempty"`
	LastAttempt int64   `json:"last_attempt_ts,omitempty"`
	LateHours   float64 `json:"late_hours,omitempty"`
}

type State struct {
	Tasks map[string]TaskState `json:"-"`
	Audit string               `json:"-"`
}

// Load 读调度状态。
//
// ⚠️ 不能对任何异常都回空：文件还没生成是正常的（首次运行），但文件存在却读
// 不出来是另一回事——回空意味着当天所有任务被当成没跑过、会全部重跑一遍，
// 正好把用量窗口那套间隔毁掉。两种情况必须分开，后者要留痕。
func Load(path string) (*State, error) {
	s := &State{Tasks: map[string]TaskState{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("状态文件存在却读不出来（%w）——当日任务会被当成未跑过而重跑: %s", err, path)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return s, fmt.Errorf("状态文件损坏（%w）——当日任务会被当成未跑过而重跑: %s", err, path)
	}
	for k, v := range m {
		if k == "_audit" {
			_ = json.Unmarshal(v, &s.Audit)
			continue
		}
		var ts TaskState
		if json.Unmarshal(v, &ts) == nil {
			s.Tasks[k] = ts
		}
	}
	return s, nil
}

// Save 原子写：进程被杀时不留半个文件（半个 JSON 下次读出来就是解析失败）。
func (s *State) Save(path string) error {
	m := map[string]any{}
	for k, v := range s.Tasks {
		m[k] = v
	}
	if s.Audit != "" {
		m["_audit"] = s.Audit
	}
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type Health struct {
	Heartbeat      string            `json:"heartbeat"`
	HeartbeatEpoch int64             `json:"heartbeat_epoch"`
	PID            int               `json:"pid"`
	Schedule       []string          `json:"schedule"`
	Tasks          map[string]string `json:"tasks"`
	// 正在跑的档与它的开始时刻。一档一跑就是十几二十分钟，这期间主循环阻塞在
	// pipeline 里；没有这两项，status 分不清「还没跑」和「跑到一半」。
	Running      string `json:"running,omitempty"`
	RunningSince string `json:"running_since,omitempty"`
	// 调度器进程的构建版本。重新编译之后不重启，它会一直跑旧代码；status 拿这一项
	// 和自己的版本比，对不上就提醒重启。
	Version string `json:"version,omitempty"`
	// Danger 是这台机器当前跑不跑危险模式（DANGER=1）。带进 site/status.json，
	// 让站点和外部监控也能看到「站上的东西是无限制跑出来的」。
	Danger bool `json:"danger,omitempty"`
}

// Running 正在执行的任务。nil 表示空闲。
type Running struct {
	Key   string
	Since time.Time
}

// WriteHealth 把调度器存活写进 state/health.json。
//
// 本机的调度器报不了自己的死——机器关了、进程挂了，它没机会发任何东西。
// 这个文件由站点生成器带进 site/ 一起发布，外部监控轮询它、看时间戳是否
// 变陈旧，才是唯一能覆盖「整机不在」的办法。
//
// ⚠️ 时间戳带时区并另给 epoch：消费者是跑在 UTC 里的 Worker。
func WriteHealth(path string, s *State, sched []string, run *Running, version string, danger bool) error {
	now := time.Now()
	h := Health{
		Heartbeat: now.Format(time.RFC3339), HeartbeatEpoch: now.Unix(),
		PID: os.Getpid(), Schedule: sched, Tasks: map[string]string{}, Version: version,
		Danger: danger,
	}
	if run != nil {
		h.Running, h.RunningSince = run.Key, run.Since.Format(time.RFC3339)
	}
	for k, v := range s.Tasks {
		h.Tasks[k] = v.Date + " " + v.Status
	}
	b, err := json.MarshalIndent(h, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
