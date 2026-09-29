package schedule

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/newsdesk/internal/task"
)

func loopOn(t *testing.T) (*Loop, string) {
	t.Helper()
	root := t.TempDir()
	return loopAt(t, root), root
}

// report 在 reports/<day>/ 下放一份报告，并把 mtime 设成指定时刻。
func report(t *testing.T, root, day, name string, at time.Time) {
	t.Helper()
	dir := filepath.Join(root, "reports", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("# 报告\n\n正文。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// 被中断过的档会留下一条失败记录。产出明明已经有了，照着那条记录重跑等于
// 白烧一次模型调用，还会覆盖掉好的报告。
func TestReconcileClearsStaleFailureWhenArtifactExists(t *testing.T) {
	l, root := loopOn(t)
	now := at("2026-09-17 10:30")
	ai, _ := task.Find("ai") // 06:30
	report(t, root, "2026-09-17", "ai-news.md", at("2026-09-17 10:11"))

	st := &State{Tasks: map[string]TaskState{
		"ai": {Date: "2026-09-17", Status: "failed", Attempts: 1, Reason: "已取消"},
	}}
	if !l.reconcile(ai, now, st) {
		t.Fatal("产出已存在，应当校正为已完成")
	}
	if got := st.Tasks["ai"]; got.Status != "ok" {
		t.Fatalf("状态没被校正：%+v", got)
	}
	// 校正过之后判定也得跟着变，否则等于没修
	if d := Decide(ai, now, st.Tasks["ai"]); d.Action != ActWait {
		t.Fatalf("校正后不该再跑，得到 %s（%s）", d.Action, d.Reason)
	}
}

// 手动跑过的档在调度状态里根本没有记录。
func TestReconcilePicksUpManualRun(t *testing.T) {
	l, root := loopOn(t)
	now := at("2026-09-17 12:00")
	douban, _ := task.Find("douban") // 11:35
	report(t, root, "2026-09-17", "douban.md", at("2026-09-17 11:50"))

	st := &State{Tasks: map[string]TaskState{}}
	if !l.reconcile(douban, now, st) {
		t.Fatal("手动跑过的档应当被认出来")
	}
	if st.Tasks["douban"].Status != "ok" {
		t.Fatalf("%+v", st.Tasks["douban"])
	}
}

// ⚠️ 判据是「产出不早于预定时刻」，不是「今天有这个文件」。
// Trending 有早间草稿：文件在，但那是上一轮的产物，16:40 那次仍然要跑。
// 只看文件存在会把整档跳过，而且不报错。
func TestReconcileIgnoresArtifactOlderThanSlot(t *testing.T) {
	l, root := loopOn(t)
	now := at("2026-09-17 16:45")
	tr, _ := task.Find("trending") // 16:40
	report(t, root, "2026-09-17", "github-trending_draft0917.md", at("2026-09-17 09:00"))

	st := &State{Tasks: map[string]TaskState{}}
	if l.reconcile(tr, now, st) {
		t.Fatal("早间草稿不该让 16:40 那次被跳过")
	}
	if d := Decide(tr, now, st.Tasks[tr.Key]); d.Action != ActRun {
		t.Fatalf("正式那次仍应运行，得到 %s", d.Action)
	}
}

func TestReconcileNoArtifactIsNoop(t *testing.T) {
	l, _ := loopOn(t)
	momoyu, _ := task.Find("momoyu")
	st := &State{Tasks: map[string]TaskState{}}
	if l.reconcile(momoyu, at("2026-09-17 22:00"), st) {
		t.Fatal("没有产出时不该校正")
	}
	if len(st.Tasks) != 0 {
		t.Fatalf("不该凭空写状态：%+v", st.Tasks)
	}
}

// 昨天的产出不能让今天这一档被跳过。
func TestReconcileDoesNotLookAtOtherDays(t *testing.T) {
	l, root := loopOn(t)
	ai, _ := task.Find("ai")
	report(t, root, "2026-09-16", "ai-news.md", at("2026-09-16 07:00"))
	st := &State{Tasks: map[string]TaskState{}}
	if l.reconcile(ai, at("2026-09-17 07:00"), st) {
		t.Fatal("昨天的报告不能算今天的")
	}
}
