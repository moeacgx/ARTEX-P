//go:build linux

package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// 测试进程只派生自己的合成子孙，不访问外网。
func TestLinuxDescendantHelper(t *testing.T) {
	role := os.Getenv("NORMA_TEST_DESCENDANT")
	if role == "" {
		return
	}
	if role == "leaf" {
		if err := os.WriteFile(os.Getenv("NORMA_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(2)
		}
		time.Sleep(45 * time.Second)
		os.Exit(0)
	}
	next := "leaf"
	if role == "double" {
		next = "intermediate"
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxDescendantHelper$")
	cmd.Env = append(os.Environ(), "NORMA_TEST_DESCENDANT="+next)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if role != "ordinary" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		os.Exit(3)
	}
	if role == "intermediate" {
		os.Exit(0)
	}
	if role == "double" {
		_ = cmd.Wait()
	}
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(os.Getenv("NORMA_TEST_PID")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if os.Getenv("NORMA_TEST_ROOT_EARLY") != "1" {
		time.Sleep(45 * time.Second)
	}
	os.Exit(0)
}

func linuxDescendant(t *testing.T, m *Manager, mode string, early bool) (*Task, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "leaf.pid")
	earlyValue := "0"
	if early {
		earlyValue = "1"
	}
	spec := SpawnSpec{Command: fmt.Sprintf("'%s' -test.run=^TestLinuxDescendantHelper$", strings.ReplaceAll(os.Args[0], "'", "'\\''")), Env: []string{"NORMA_TEST_DESCENDANT=" + mode, "NORMA_TEST_PID=" + pidFile, "NORMA_TEST_ROOT_EARLY=" + earlyValue}}
	task, err := m.Spawn(spec)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, e := os.ReadFile(pidFile); e == nil {
			pid, e := strconv.Atoi(string(raw))
			if e == nil {
				trackLinuxLeaf(t, pid, pidFile)
				return task, pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("子孙未就绪")
	return nil, 0
}

func trackLinuxLeaf(t *testing.T, pid int, pidFile string) {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err == unix.ESRCH {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if !bytes.Contains(raw, []byte("NORMA_TEST_PID="+pidFile+"\x00")) {
		_ = unix.Close(fd)
		return
	}
	t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
}

func assertLinuxReaped(t *testing.T, pid int) {
	t.Helper()
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); !os.IsNotExist(err) {
		t.Fatalf("任务已宣告退出，但子孙 %d 未回收: %s (err=%v)", pid, raw, err)
	}
}

func TestLinuxTaskDescendants(t *testing.T) {
	for _, mode := range []string{"ordinary", "setsid", "double"} {
		for _, ending := range []string{"kill", "cancel", "timeout", "root-first"} {
			t.Run(mode+"/"+ending, func(t *testing.T) {
				m := newTestManager(t)
				task, pid := linuxDescendant(t, m, mode, ending == "root-first")
				if ending == "cancel" {
					m.rootCancel()
				} else if ending != "root-first" {
					if ending == "timeout" {
						if _, done := m.Wait(context.Background(), task.ID, 10*time.Millisecond); done {
							t.Fatal("尚存活任务提前结束")
						}
					}
					m.Kill(task.ID)
				}
				if _, done := m.Wait(context.Background(), task.ID, 5*time.Second); !done {
					t.Fatal("任务退出未获确认")
				}
				assertLinuxReaped(t, pid)
			})
		}
	}
}

func TestLinuxTaskIsolation(t *testing.T) {
	m := newTestManager(t)
	a, aPID := linuxDescendant(t, m, "double", false)
	b, bPID := linuxDescendant(t, m, "setsid", false)
	m.Kill(a.ID)
	if _, done := m.Wait(context.Background(), a.ID, 5*time.Second); !done {
		t.Fatal("第一个任务未退出")
	}
	assertLinuxReaped(t, aPID)
	if err := syscall.Kill(bPID, 0); err != nil {
		t.Fatalf("误杀另一任务: %v", err)
	}
	m.Kill(b.ID)
	if _, done := m.Wait(context.Background(), b.ID, 5*time.Second); !done {
		t.Fatal("第二个任务未退出")
	}
	assertLinuxReaped(t, bPID)
}

func TestLinuxPTYDescendants(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprint(early), func(t *testing.T) {
			m := newTestManager(t)
			pidFile := filepath.Join(t.TempDir(), "leaf.pid")
			earlyValue := "0"
			if early {
				earlyValue = "1"
			}
			s, err := m.openSession(fmt.Sprintf("'%s' -test.run=^TestLinuxDescendantHelper$", os.Args[0]), "", []string{"NORMA_TEST_DESCENDANT=double", "NORMA_TEST_PID=" + pidFile, "NORMA_TEST_ROOT_EARLY=" + earlyValue}, 24, 80, "interactive", nil)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				raw, e := os.ReadFile(pidFile)
				if e == nil {
					pid, _ = strconv.Atoi(string(raw))
					if pid > 0 {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("PTY 子孙未就绪")
			}
			trackLinuxLeaf(t, pid, pidFile)
			if !early {
				m.retireSession(s.id, s)
				s.kill()
			}
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				t.Fatal("PTY 退出未获确认")
			}
			assertLinuxReaped(t, pid)
		})
	}
}

func TestLinuxForegroundDetachedExit(t *testing.T) {
	for _, ending := range []string{"timeout", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			m := newTestManager(t)
			pidPath := filepath.Join(t.TempDir(), "foreground.pid")
			command := fmt.Sprintf("'%s' -test.run=^TestLinuxDescendantHelper$", os.Args[0])
			env := []string{"NORMA_TEST_DESCENDANT=double", "NORMA_TEST_PID=" + pidPath}
			if ending == "timeout" {
				result, err := runBashManaged(context.Background(), &ToolContext{Tasks: m, Env: env}, command, 300*time.Millisecond, false)
				if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "timed out") {
					t.Fatalf("timeout result=%s err=%v", result.Flatten(), err)
				}
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				finished := make(chan error, 1)
				go func() {
					_, _, _, err := m.RunForeground(ctx, SpawnSpec{Command: command, Env: env}, 10*time.Second)
					finished <- err
				}()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(pidPath); err == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				cancel()
				select {
				case err := <-finished:
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case <-time.After(6 * time.Second):
					t.Fatal("前台取消没有有界返回")
				}
			}
			raw, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			trackLinuxLeaf(t, pid, pidPath)
			assertLinuxReaped(t, pid)
		})
	}
}

func TestLinuxCleanupTimeoutRetainsTask(t *testing.T) {
	m := newTestManager(t)
	task, pid := linuxDescendant(t, m, "double", false)
	if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer task.cmd.Process.Signal(syscall.SIGCONT)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.CleanupAndWait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("暂停中的收养者被误判为已退出: %v", err)
	}
	m.Forget(task.ID)
	if _, ok := m.Get(task.ID); !ok {
		t.Fatal("未退出任务丢失")
	}
	if _, err := os.Stat(task.OutputPath); err != nil {
		t.Fatalf("输出过早删除: %v", err)
	}
	if info, _ := m.Get(task.ID); info.Status.Terminal() {
		t.Fatal("未退出任务提前成为终态")
	}
	if err := task.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := m.CleanupAndWait(ctx2); err != nil {
		t.Fatal(err)
	}
	assertLinuxReaped(t, pid)
}
