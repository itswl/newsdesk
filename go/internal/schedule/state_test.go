package schedule

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingIsNotAnError(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(s.Tasks) != 0 {
		t.Fatalf("首次运行没有状态文件是正常的：%v", err)
	}
}

// 文件存在却读不出来 ≠ 没有文件。回空会让当天所有任务被当成没跑过而全部重跑，
// 正好把用量窗口那套间隔毁掉，所以必须报错。
func TestLoadCorruptIsAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("状态文件损坏必须报错，不能静默当成空状态")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	s := &State{Tasks: map[string]TaskState{
		"ai": {Date: "2026-09-17", Status: "ok", Attempts: 1, Engine: "claude"},
	}, Audit: "2026-09-17"}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tasks["ai"].Status != "ok" || got.Tasks["ai"].Engine != "claude" {
		t.Fatalf("状态没存回来：%+v", got.Tasks["ai"])
	}
	if got.Audit != "2026-09-17" {
		t.Fatalf("日终核对标记丢了：%q", got.Audit)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json")
	s := &State{Tasks: map[string]TaskState{"ai": {Date: "2026-09-17", Status: "ok"}}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("临时文件没清掉：%v", ents)
	}
}

// 消费者是跑在 UTC 里的 Worker：只给本地 ISO 串会被当成 UTC 读，
// 算出来的心跳年龄是负的。必须同时给 epoch。
func TestHealthCarriesEpoch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "health.json")
	s := &State{Tasks: map[string]TaskState{"ai": {Date: "2026-09-17", Status: "ok"}}}
	if err := WriteHealth(p, s, []string{"06:30 ai"}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	for _, k := range []string{`"heartbeat_epoch"`, `"pid"`, `"schedule"`, `"tasks"`} {
		if !contains(string(raw), k) {
			t.Fatalf("health.json 缺 %s：\n%s", k, raw)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 任务跑着的时候 health.json 要标明是哪一档、几点开始的；空闲时这两项不出现。
func TestHealthCarriesRunningTask(t *testing.T) {
	p := filepath.Join(t.TempDir(), "health.json")
	s := &State{Tasks: map[string]TaskState{}}
	since := time.Date(2026, 9, 28, 16, 40, 0, 0, time.Local)
	if err := WriteHealth(p, s, []string{"16:40 trending"}, &Running{Key: "trending", Since: since}, "test"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	for _, k := range []string{`"running": "trending"`, `"running_since": "` + since.Format(time.RFC3339)} {
		if !contains(string(raw), k) {
			t.Errorf("health.json 缺 %s：\n%s", k, raw)
		}
	}
	if err := WriteHealth(p, s, []string{"16:40 trending"}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	if contains(string(raw), `"running"`) {
		t.Errorf("空闲时不该留着 running：\n%s", raw)
	}
}

// 主循环阻塞在任务里时，旁路心跳要一直刷新，直到 stop 返回。
func TestHeartbeatWhileKeepsWritingUntilStopped(t *testing.T) {
	l := &Loop{healthPath: filepath.Join(t.TempDir(), "health.json"), sched: []string{"06:30 ai"}}
	s := &State{Tasks: map[string]TaskState{}}
	stop := l.heartbeatWhile(s, "ai", 5*time.Millisecond)
	first, err := os.Stat(l.healthPath)
	if err != nil {
		t.Fatal("启动时就该写一次心跳:", err)
	}
	time.Sleep(60 * time.Millisecond)
	stop()
	after, _ := os.Stat(l.healthPath)
	if !after.ModTime().After(first.ModTime()) {
		t.Error("任务期间心跳没有持续刷新")
	}
	raw, _ := os.ReadFile(l.healthPath)
	if !contains(string(raw), `"running": "ai"`) {
		t.Errorf("心跳里没标正在跑的档：\n%s", raw)
	}
}
