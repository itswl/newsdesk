package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// daemonize 把自己重新拉起来跑在后台，然后父进程退出。
//
// 不用 nohup + & 是因为那只解决「关掉终端不被杀」，解决不了「输出去哪」和
// 「怎么停」——前者会把日志喷进终端或黑洞，后者只能靠 pgrep 猜 pid。
//
// Setsid 让它脱离当前终端会话：终端关了不会收到 SIGHUP，写 stdout 也不会
// 因为 tty 没了而报 EIO。
func daemonize(root string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "logs", "scheduler.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	cmd := exec.Command(exe, "schedule")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "NEWSDESK_DAEMON=1")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不等它退出，但要确认它没有立刻失败（时间表不自洽、已有实例在跑、
	// 配置解析不了——这些都会在头一秒内退出）。直接返回的话用户看到
	// 「已启动」然后什么都没发生。
	time.Sleep(1200 * time.Millisecond)
	if pid, ok := readPID(root); ok && pid == cmd.Process.Pid {
		fmt.Printf("调度器已在后台启动，pid %d\n", pid)
		fmt.Printf("  日志: %s\n", logPath)
		fmt.Printf("  停止: newsdesk stop\n")
		return nil
	}
	return fmt.Errorf("后台进程启动后立刻退出了，原因见 %s 末尾", logPath)
}

func pidPath(root string) string { return filepath.Join(root, "state", "scheduler.pid") }

func readPID(root string) (int, bool) {
	raw, err := os.ReadFile(pidPath(root))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// cmdStop 停掉后台调度器。
//
// 发 SIGTERM 而不是 SIGKILL：调度器要把手上的任务收干净、删掉 pid 文件。
// 正在跑模型的话那个子进程会跟着被杀，但**不会**记成任务失败——
// 重启后按「今天还没跑」正常补跑。
func cmdStop(ctx context.Context, args []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	pid, ok := readPID(root)
	if !ok {
		fmt.Println("调度器没在运行（没有 pid 文件）")
		return nil
	}
	if !alive(pid) {
		fmt.Printf("调度器没在运行（pid %d 已不存在），清掉残留的 pid 文件\n", pid)
		return os.Remove(pidPath(root))
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("发送停止信号给 pid %d: %w", pid, err)
	}
	// 等它自己收尾。正在跑模型时子进程要花一两秒才退干净。
	for i := 0; i < 100; i++ {
		if !alive(pid) {
			fmt.Printf("已停止（pid %d）\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid %d 收到 SIGTERM 后 10 秒仍未退出，可能卡在某个子进程上；"+
		"确认无妨后用 kill -9 %d", pid, pid)
}
