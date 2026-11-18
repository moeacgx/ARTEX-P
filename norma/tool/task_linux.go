//go:build linux

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const supervisorArg = "--norma-private-task-supervisor"

type supervisorSpec struct {
	Command []string
	ProofFD int
}

// 每个任务独立收养孤儿，避免在宿主设置全局 subreaper 后混淆并发任务。
// 该入口先于应用 main 执行，子命令不携带入口参数。
func init() {
	if len(os.Args) == 3 && os.Args[1] == supervisorArg {
		var spec supervisorSpec
		if json.Unmarshal([]byte(os.Args[2]), &spec) != nil || len(spec.Command) < 2 || spec.ProofFD < 3 {
			os.Exit(125)
		}
		proof := os.NewFile(uintptr(spec.ProofFD), "task-exit-proof")
		unix.CloseOnExec(spec.ProofFD)
		os.Exit(superviseTask(spec.Command, proof))
	}
}

type processTree struct {
	mu                    sync.Mutex
	proc                  *os.Process
	proofRead, proofWrite *os.File
	proven                bool
	ready, stop           bool
	setupDone, proofDone  chan struct{}
	setupErr, proofErr    error
}

func preparePTYTree(cmd *exec.Cmd) (*processTree, error) {
	tree, err := prepareProcessTree(cmd)
	if err == nil {
		cmd.SysProcAttr.Setpgid = false
	} // PTY 使用 setsid 建立自己的会话。
	return tree, err
}

func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args, err := json.Marshal(supervisorSpec{Command: append([]string{cmd.Path}, cmd.Args...), ProofFD: 3 + len(cmd.ExtraFiles)})
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, writer)
	cmd.Path, cmd.Args = exe, []string{exe, supervisorArg, string(args)}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	tree := &processTree{proofRead: reader, proofWrite: writer, setupDone: make(chan struct{}), proofDone: make(chan struct{})}
	go tree.readProof()
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { tree.kill(); return nil }
	}
	return tree, nil
}

func (t *processTree) attach(p *os.Process) error {
	t.mu.Lock()
	t.proc = p
	if t.ready && t.stop {
		_ = p.Signal(syscall.SIGTERM)
	}
	t.mu.Unlock()
	_ = t.proofWrite.Close()
	select {
	case <-t.setupDone:
		return t.setupErr
	case <-time.After(5 * time.Second):
		return fmt.Errorf("等待任务收养者就绪超时")
	}
}
func (t *processTree) kill() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stop = true
	if t.ready && t.proc != nil {
		_ = t.proc.Signal(syscall.SIGTERM)
	}
}
func (t *processTree) close() { _ = t.proofRead.Close(); _ = t.proofWrite.Close() }

// 根 Wait 后还必须收到收养者的 ECHILD 证明；被外部强杀不能成为退出证明。
func (t *processTree) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.proofDone:
	}
	if !t.proven {
		return fmt.Errorf("任务子孙回收证明缺失: %w", t.proofErr)
	}
	return nil
}

func (t *processTree) readProof() {
	defer close(t.proofDone)
	var message [1]byte
	_, err := io.ReadFull(t.proofRead, message[:])
	if err != nil {
		t.setupErr = err
		t.proofErr = err
		close(t.setupDone)
		return
	}
	if message[0] != 0 && message[0] != 1 {
		t.setupErr = fmt.Errorf("任务收养者就绪证明无效")
		t.proofErr = t.setupErr
		close(t.setupDone)
		return
	}
	t.mu.Lock()
	t.ready = true
	if t.stop && t.proc != nil {
		_ = t.proc.Signal(syscall.SIGTERM)
	}
	t.mu.Unlock()
	close(t.setupDone)
	if message[0] == 0 {
		_, err = io.ReadFull(t.proofRead, message[:])
	}
	if err != nil {
		t.proofErr = err
		return
	}
	if message[0] != 1 {
		t.proofErr = fmt.Errorf("任务子孙回收证明无效")
		return
	}
	t.proven = true
}

func superviseTask(args []string, proof *os.File) int {
	confirm := func(code int) int { _, _ = proof.Write([]byte{1}); _ = proof.Close(); return code }
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "任务收养边界初始化失败:", err)
		return confirm(125)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(stop)
	// 宿主确认信号处理已就绪后才允许发送取消，避免启动瞬间杀掉收养者。
	if _, err := proof.Write([]byte{0}); err != nil {
		return 125
	}
	cmd := &exec.Cmd{Path: args[0], Args: args[1:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Env: os.Environ(), SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if _, err := unix.IoctlGetTermios(0, unix.TCGETS); err == nil {
		// PTY 的读写者必须成为前台组，收养者只负责等待和回收。
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = 0
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return confirm(127)
	}
	rootDone := make(chan struct{})
	go func() {
		// WNOWAIT 保留根 PID，组终止与根退出同时发生时也不会误杀复用的组号。
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if err == nil {
				close(rootDone)
				return
			}
			if err != syscall.EINTR {
				fmt.Fprintln(os.Stderr, "等待任务根退出失败:", err)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	select {
	case <-rootDone:
	case <-stop:
		treeKill(cmd.Process)
		<-rootDone
	}
	_ = cmd.Wait()
	// root 已回收。只操作本收养者的直接子进程，未 wait 的 PID 不会复用。
	// 杀死中间进程后，内核会把更深层子孙原子转交给本收养者。
	for {
		if err := killAdoptedChildren(); err != nil {
			// 无法读取归属时保持 supervisor 存活，调用方超时，禁止伪报退出。
			fmt.Fprintln(os.Stderr, "任务子孙归属确认失败:", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if err == syscall.ECHILD {
				ec := cmd.ProcessState.ExitCode()
				if ec < 0 {
					return confirm(1)
				}
				return confirm(ec)
			}
			if err == syscall.EINTR {
				continue
			}
			if err != nil || pid == 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func killAdoptedChildren() error {
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	for _, thread := range threads {
		raw, err := os.ReadFile(filepath.Join("/proc/self/task", thread.Name(), "children"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, s := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(s)
			if err != nil {
				return err
			}
			if err = syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return err
			}
		}
	}
	return nil
}

// 保留组终止回退；只在根进程尚未 Wait 时使用，避免 PID 回收后的误杀。
func treeKill(p *os.Process) {
	if p != nil {
		if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
			_ = p.Kill()
		}
	}
}
