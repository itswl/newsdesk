package schedule

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itswl/newsdesk/internal/config"
	"github.com/itswl/newsdesk/internal/task"
)

// confFixture 一个监视着 config.conf 的调度器，告警发到一个只计数的假 webhook。
type confFixture struct {
	t      *testing.T
	l      *Loop
	path   string
	hook   string
	alerts atomic.Int32
}

func newConfFixture(t *testing.T, body string) *confFixture {
	t.Helper()
	f := &confFixture{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { f.alerts.Add(1) }))
	t.Cleanup(srv.Close)
	f.hook = srv.URL
	root := t.TempDir()
	f.path = filepath.Join(root, "config.conf")
	f.write(f.conf(body))
	f.l = loopAt(t, root)
	f.l.WatchConfig(f.path)
	return f
}

// conf 在正文前面补上告警地址，这样读进来的配置也能发告警
func (f *confFixture) conf(body string) string { return "ALERT_WEBHOOK=" + f.hook + "\n" + body }

func (f *confFixture) write(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// settled 连着读两轮：新内容要两轮一样才会生效
func (f *confFixture) settled() (bool, error) {
	f.t.Helper()
	if changed, err := f.l.reloadConfig(); changed || err != nil {
		f.t.Fatalf("第一轮只该记下新内容、等它写稳：changed=%v err=%v", changed, err)
	}
	return f.l.reloadConfig()
}

// loopAt 在 root 上起一个调度器，拿 root/config.conf 当启动时读到的配置
func loopAt(t *testing.T, root string) *Loop {
	t.Helper()
	cfg, err := config.Load(filepath.Join(root, "config.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return NewLoop(root, root, cfg)
}

// 改了 config.conf 要自己生效，不用重启调度器。09-29 就是因为改完没重启，
// 中午那档用旧配置重建站点，把刚上线的通知开关冲掉了。
func TestConfigReloadsOnceTheFileSettles(t *testing.T) {
	f := newConfFixture(t, "AUDIT_AT=23:30\n")
	if changed, _ := f.l.reloadConfig(); changed {
		t.Fatal("文件没动不该重读")
	}
	f.write(f.conf("AUDIT_AT=22:00\nPUSH_VAPID_PUBLIC=BEXAMPLE\n"))
	if changed, err := f.settled(); !changed || err != nil {
		t.Fatalf("写稳之后应当生效：changed=%v err=%v", changed, err)
	}
	if got := f.l.Cfg.Get("PUSH_VAPID_PUBLIC", ""); got != "BEXAMPLE" {
		t.Errorf("新值没生效：%q", got)
	}
	if got := f.l.auditTime(); got != "22:00" {
		t.Errorf("从配置派生的东西也要跟着换：auditTime=%q", got)
	}
}

// 编辑器保存可能是「先清空再写」。读在中间拿到的空配置是合法的（等于只在本机
// 看简报），换上去告警、发布、英文站会一起悄悄消失。
func TestHalfWrittenConfigIsNeverApplied(t *testing.T) {
	f := newConfFixture(t, "PUBLISH_BUCKET=site\n")
	f.write("") // 读在「清空」和「写入」之间
	if changed, _ := f.l.reloadConfig(); changed {
		t.Fatal("刚读到的新内容不能当场生效")
	}
	f.write(f.conf("PUBLISH_BUCKET=site\nSITE_URL=https://news.example.com\n"))
	if changed, _ := f.l.reloadConfig(); changed {
		t.Fatal("内容又变了，还要再等一轮")
	}
	if f.l.Cfg.Get("PUBLISH_BUCKET", "") != "site" {
		t.Fatal("空的那一版被换上去了")
	}
	if changed, err := f.l.reloadConfig(); !changed || err != nil {
		t.Fatalf("写完之后应当生效：changed=%v err=%v", changed, err)
	}
	if f.l.Cfg.Get("SITE_URL", "") == "" {
		t.Error("写完的那一版没生效")
	}
}

// 这台机器是 HFS+，修改时刻只精确到秒。按修改时刻判断的话，同一秒里的第二次
// 保存会被当成已经看过——半截的第一版就此留下，直到下一次落在别的秒里的编辑。
func TestSaveWithUnchangedMtimeIsStillSeen(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.conf("ENGINE=codex\n"))
	if err := os.Chtimes(f.path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.settled(); !changed || err != nil {
		t.Fatalf("修改时刻没变但内容变了，也要生效：changed=%v err=%v", changed, err)
	}
	if got := f.l.Cfg.Get("ENGINE", ""); got != "codex" {
		t.Errorf("ENGINE=%q", got)
	}
}

// 启动时先读配置、后开始监视。两步之间落下的保存不能被当成已经看过。
func TestEditBetweenStartupReadAndWatchIsPickedUp(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "config.conf")
	if err := os.WriteFile(p, []byte("ENGINE=claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := loopAt(t, root) // 读到旧的
	if err := os.WriteFile(p, []byte("ENGINE=api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.WatchConfig(p) // 监视开始时文件已经是新的了
	if changed, _ := l.reloadConfig(); changed {
		t.Fatal("第一轮只记下")
	}
	if changed, err := l.reloadConfig(); !changed || err != nil {
		t.Fatalf("启动那一刻落下的保存应当生效：changed=%v err=%v", changed, err)
	}
	if got := l.Cfg.Get("ENGINE", ""); got != "api" {
		t.Errorf("ENGINE=%q", got)
	}
}

// 加注释、调空白、touch 一下：内容变了、值没变，不该报「配置已重新读取」
func TestNoOpSaveIsQuiet(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	f.write(f.conf("# 只加了一行注释\nENGINE=claude\n"))
	if changed, err := f.settled(); changed || err != nil {
		t.Fatalf("值没变不算改动：changed=%v err=%v", changed, err)
	}
	if changed, _ := f.l.reloadConfig(); changed {
		t.Error("已经核对过的内容不该再读一遍")
	}
}

// 读坏了沿用旧配置、不中断调度，而且同一份坏文件只告警一次——否则每 30 秒一条。
func TestBrokenConfigKeepsOldAndAlertsOnce(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	f.write(f.conf("ENGINE=codex\nENGINE=api\n")) // 重复键：解析器直接报错
	if _, err := f.settled(); err == nil {
		t.Fatal("重复键应当报错")
	}
	if got := f.l.Cfg.Get("ENGINE", ""); got != "claude" {
		t.Errorf("读坏了要沿用旧配置，现在是 %q", got)
	}
	if changed, err := f.l.reloadConfig(); changed || err != nil {
		t.Errorf("同一份坏文件不该再试一次：changed=%v err=%v", changed, err)
	}
	if n := f.alerts.Load(); n != 1 {
		t.Errorf("告警 %d 次，应当正好 1 次", n)
	}

	f.write(f.conf("ENGINE=api\n")) // 修好了
	if changed, err := f.settled(); !changed || err != nil {
		t.Fatalf("修好之后应当重新读取：changed=%v err=%v", changed, err)
	}
	if got := f.l.Cfg.Get("ENGINE", ""); got != "api" {
		t.Errorf("修好后的值没生效：%q", got)
	}
}

// 文件被删或读不了：沿用旧配置并且说出来，同一个错误只说一次，恢复后自己接上。
// 以前 stat 失败一律当「下一轮再说」，删了配置调度器照旧跑、一个字都不提。
func TestUnreadableConfigKeepsOldAndSaysSoOnce(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	body, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.l.reloadConfig(); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("第 %d 轮应当报文件不存在：%v", i+1, err)
		}
	}
	if n := f.alerts.Load(); n != 1 {
		t.Errorf("告警 %d 次，同一个错误应当只告警一次", n)
	}
	if got := f.l.Cfg.Get("ENGINE", ""); got != "claude" {
		t.Errorf("读不了要沿用旧配置，现在是 %q", got)
	}
	f.write(string(body)) // 原样放回来
	if changed, err := f.l.reloadConfig(); changed || err != nil {
		t.Errorf("放回同一份不算改动：changed=%v err=%v", changed, err)
	}
}

// 从一开始就没有配置文件，是「只在本机看简报」的正常形态，不该告警
func TestNoConfigFileAtAllIsQuiet(t *testing.T) {
	l, root := loopOn(t)
	l.WatchConfig(filepath.Join(root, "config.conf"))
	for i := 0; i < 3; i++ {
		if changed, err := l.reloadConfig(); changed || err != nil {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	}
}

// 告警通道从当前配置现取：改了 ALERT_WEBHOOK 就要发往新地址
func TestAlertsFollowTheCurrentWebhook(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	var moved atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { moved.Add(1) }))
	defer srv.Close()
	f.write("ALERT_WEBHOOK=" + srv.URL + "\nENGINE=claude\n")
	if changed, err := f.settled(); !changed || err != nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	f.l.alert("t", "b")
	if f.alerts.Load() != 0 || moved.Load() != 1 {
		t.Errorf("告警应当发往新地址：旧 %d 次，新 %d 次", f.alerts.Load(), moved.Load())
	}
}

// 核对按字符串比时刻。「9:30」原样拿去比的话任何时刻都小于它，核对再也不会触发
func TestAuditTimeIsNormalized(t *testing.T) {
	for in, want := range map[string]string{"9:30": "09:30", "22:00": "22:00", "": "23:30", "晚上": "23:30", "25:00": "23:30"} {
		f := newConfFixture(t, "AUDIT_AT="+in+"\n")
		if got := f.l.auditTime(); got != want {
			t.Errorf("AUDIT_AT=%q → %q，想要 %q", in, got, want)
		}
	}
}

// 一档跑的途中配置改过，它是按旧配置建站发布的，会盖掉期间手动按新配置发布的那版。
// 新配置生效时要重发一次；改了又改回来（或者只动了注释）就不必。
func TestRunOnStaleConfigAsksForRepublish(t *testing.T) {
	f := newConfFixture(t, "PUSH_VAPID_PUBLIC=\n")
	if f.l.configMoved() {
		t.Fatal("文件没动")
	}
	f.write(f.conf("PUSH_VAPID_PUBLIC=BEXAMPLE\n")) // 跑的途中改了
	if !f.l.configMoved() {
		t.Fatal("跑完时文件里的值已经不是这一档用的那份了")
	}
	f.l.republish = true // runOne 在这时候会立这个旗
	if changed, _ := f.settled(); !changed || !f.l.republish {
		t.Fatalf("新配置生效时旗要还在，tick 据此重发：changed=%v republish=%v", changed, f.l.republish)
	}

	g := newConfFixture(t, "ENGINE=claude\n")
	g.write(g.conf("# 只改了注释\nENGINE=claude\n"))
	if g.l.configMoved() {
		t.Error("只改注释不算改了配置")
	}
	g.l.republish = true
	if changed, _ := g.settled(); changed || g.l.republish {
		t.Errorf("值没变就没什么要重发的：changed=%v republish=%v", changed, g.l.republish)
	}
}

// 危险模式开和关都要主动说一声，两个方向都发。
//
// 只在启动时报一次的话，热重读打开的那一次会静默生效——而它关掉的是整套防护
// （沙箱、凭据拒读、发布前自检），跑出来的报告当天就公开发布。
func TestDangerModeAlertsOnAndOff(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	if f.l.Cfg.Danger() {
		t.Fatal("默认必须是关着的")
	}
	f.write(f.conf("ENGINE=claude\nDANGER=1\n"))
	if changed, err := f.settled(); !changed || err != nil {
		t.Fatalf("应当重新读取：changed=%v err=%v", changed, err)
	}
	if !f.l.Cfg.Danger() {
		t.Fatal("DANGER=1 没有生效")
	}
	if n := f.alerts.Load(); n != 1 {
		t.Errorf("打开危险模式应当告警一次，现在 %d 次", n)
	}

	f.write(f.conf("ENGINE=claude\nDANGER=0\n"))
	if changed, err := f.settled(); !changed || err != nil {
		t.Fatalf("应当重新读取：changed=%v err=%v", changed, err)
	}
	if f.l.Cfg.Danger() {
		t.Fatal("DANGER=0 没有生效")
	}
	if n := f.alerts.Load(); n != 2 {
		t.Errorf("关掉也要说一声，累计应当 2 次，现在 %d 次", n)
	}
}

// 认不出来的取值不能猜——猜「开着」是一整套防护静默关掉，猜「关着」是以为开着
// 其实没开，两种都不报错。按坏配置处理：沿用上一份、告警一次、不中断调度。
func TestBadDangerValueKeepsGuards(t *testing.T) {
	f := newConfFixture(t, "ENGINE=claude\n")
	f.write(f.conf("ENGINE=claude\nDANGER=maybe\n"))
	if _, err := f.settled(); err == nil {
		t.Fatal("认不出来的 DANGER 取值应当报错")
	}
	if f.l.Cfg.Danger() {
		t.Error("读坏了绝不能把危险模式当成开着")
	}
	if n := f.alerts.Load(); n != 1 {
		t.Errorf("应当告警一次（沿用上一份配置），现在 %d 次", n)
	}
}

// 按产出校正时，危险模式的标记只能继承**同一天**那次的。
//
// 跨天继承会把昨天无限制跑出来的标签贴到今天的报告上——一份普通跑出来的报告被
// 永久标成「危险模式跑的」，比不标更糟：看的人会以为它当时的防护也是关的。
func TestReconcileDoesNotInheritDangerAcrossDays(t *testing.T) {
	root := t.TempDir()
	l := loopAt(t, root)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	day := now.Format("2006-01-02")
	tk := task.All[0]

	// 报告文件名由任务的 Files 模式决定，这里把通配符去掉取一个具体名字
	p := filepath.Join(root, "reports", day, strings.ReplaceAll(tk.Files[0], "*", ""))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, now, now); err != nil {
		t.Fatal(err)
	}

	// 昨天是危险模式跑的，今天这次是手动跑的
	st := &State{Tasks: map[string]TaskState{
		tk.Key: {Date: "2026-10-16", Status: "ok", Danger: true},
	}}
	if !l.reconcile(tk, now, st) {
		t.Fatal("应当按产出校正为已完成")
	}
	if st.Tasks[tk.Key].Danger {
		t.Error("跨天的危险模式标记不该继承到今天这份报告上")
	}

	// 同一天先失败、后来补出了报告：标记要留着
	st2 := &State{Tasks: map[string]TaskState{
		tk.Key: {Date: day, Status: "failed", Danger: true},
	}}
	if !l.reconcile(tk, now, st2) {
		t.Fatal("应当按产出校正为已完成")
	}
	if !st2.Tasks[tk.Key].Danger {
		t.Error("同一天的危险模式标记应当保留")
	}
}
