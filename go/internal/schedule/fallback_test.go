package schedule

import (
	"path/filepath"
	"testing"

	"github.com/itswl/newsdesk/internal/engine"
	"github.com/itswl/newsdesk/internal/task"
	"github.com/itswl/newsdesk/internal/translate"
)

// 主引擎失效、兜底顶上：段开始告警一次，回来再告警一次，中间的档不响。
//
// 兜底成功时报告照出、站点照发、status 全绿——2026-10-15 到 10-19 连续 17 档跑在
// 兜底上一条告警都没有，因为以前只有终局失败才告警。每档都推又等于教人忽略告警。
func TestFallbackStreakAlertsOnceEachWay(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\nENGINE_FALLBACK=api\nFALLBACK_MODEL=glm-5.3\nFALLBACK_BASE_URL=https://example.invalid\n")
	st := &State{Tasks: map[string]TaskState{}}
	onFallback := TaskState{Date: "2026-10-15", Status: "ok", Engine: "api",
		Fallback: true, FallbackWhy: "quota", Finished: "2026-10-15T06:35:00+08:00"}

	f.l.noteFallback(st, task.All[0], onFallback)
	if n := f.alerts.Load(); n != 1 || st.FallbackSince == "" {
		t.Fatalf("切到兜底应当告警一次并记下起点：告警 %d 次，since=%q", n, st.FallbackSince)
	}
	// 重启之后这段不能被当成已经结束：起点要落盘
	if got, err := Load(f.l.statePath); err != nil || got.FallbackSince != st.FallbackSince {
		t.Fatalf("兜底段起点没落盘：%v %+v", err, got)
	}

	f.l.noteFallback(st, task.All[1], onFallback)
	f.l.noteFallback(st, task.All[2], onFallback)
	if n := f.alerts.Load(); n != 1 {
		t.Errorf("同一段里后面的档不该再告警，现在 %d 次", n)
	}

	back := TaskState{Date: "2026-10-19", Status: "ok", Engine: "claude", Finished: "2026-10-19T11:38:00+08:00"}
	f.l.noteFallback(st, task.All[1], back)
	if n := f.alerts.Load(); n != 2 || st.FallbackSince != "" {
		t.Errorf("回到主引擎应当再告警一次并清掉起点：告警 %d 次，since=%q", n, st.FallbackSince)
	}
	f.l.noteFallback(st, task.All[2], back)
	if n := f.alerts.Load(); n != 2 {
		t.Errorf("主引擎正常时不该告警，现在 %d 次", n)
	}
}

// 兜底标记与用量都要进状态文件：日志会滚，事后只能从这里回答「这份是谁写的、花了多少」。
func TestSaveLoadKeepsFallbackAndUsage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	s := &State{Tasks: map[string]TaskState{
		"ai": {Date: "2026-10-19", Status: "ok", Engine: "api", Fallback: true, FallbackWhy: "quota",
			Usage:     &engine.Usage{Input: 42679, Output: 24294, CacheRead: 75392, Turns: 6, CostUSD: 0.67},
			Translate: &translate.Usage{Input: 8905, Output: 8012}},
		"douban": {Date: "2026-10-19", Status: "ok", Engine: "claude"},
	}, FallbackSince: "2026-10-15T06:35:00+08:00"}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	ai := got.Tasks["ai"]
	if !ai.Fallback || ai.FallbackWhy != "quota" || ai.Usage == nil || ai.Usage.Input != 42679 || ai.Usage.Turns != 6 {
		t.Errorf("兜底标记或用量没存回来：%+v", ai)
	}
	if ai.Translate == nil || ai.Translate.Input != 8905 || ai.Translate.Output != 8012 {
		t.Errorf("翻译用量没存回来：%+v", ai.Translate)
	}
	if got.Tasks["douban"].Usage != nil || got.Tasks["douban"].Fallback {
		t.Errorf("没有的字段不该无中生有：%+v", got.Tasks["douban"])
	}
	if got.FallbackSince != s.FallbackSince {
		t.Errorf("兜底段起点丢了：%q", got.FallbackSince)
	}
}
