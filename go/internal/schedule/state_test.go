package schedule

import (
	"os"
	"path/filepath"
	"testing"
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
	if err := WriteHealth(p, s, []string{"06:30 ai"}); err != nil {
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
