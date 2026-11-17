//go:build windows

package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP

// Windows JOBOBJECT_BASIC_ACCOUNTING_INFORMATION，字段布局来自 winnt.h。
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime int64
	PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
}

// processTree owns a Windows Job Object for the complete descendant tree. The
// process is created suspended, assigned to the job, then resumed, so no child
// can escape between process creation and assignment.
type processTree struct {
	mu      sync.Mutex
	job     windows.Handle
	port    windows.Handle
	proc    *os.Process
	members []windows.Handle
	seen    uint32
}

type jobCompletionPort struct {
	Key  uintptr
	Port windows.Handle
}

func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | createNewProcessGroup

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure process job: %w", err)
	}
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("创建 Job 进程通知端口: %w", err)
	}
	association := jobCompletionPort{Key: 1, Port: port}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectAssociateCompletionPortInformation, uintptr(unsafe.Pointer(&association)), uint32(unsafe.Sizeof(association))); err != nil {
		_ = windows.CloseHandle(port)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("绑定 Job 进程通知: %w", err)
	}
	return &processTree{job: job, port: port}, nil
}

func (t *processTree) attach(p *os.Process) error {
	if t == nil || p == nil {
		return nil
	}
	t.mu.Lock()
	t.proc = p
	t.mu.Unlock()
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(p.Pid),
	)
	if err != nil {
		return fmt.Errorf("open child process: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(t.job, handle); err != nil {
		return fmt.Errorf("assign child to process job: %w", err)
	}
	if err := resumeProcess(uint32(p.Pid)); err != nil {
		return fmt.Errorf("resume child process: %w", err)
	}
	return nil
}

func resumeProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err = windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	found := false
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return openErr
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			found = true
		}
		err = windows.Thread32Next(snapshot, &entry)
		if err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return err
		}
	}
	if !found {
		return fmt.Errorf("no thread found for process %d", pid)
	}
	return nil
}

func (t *processTree) kill() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
	if t.proc != nil {
		// If assignment or resume failed, the root process may still be
		// suspended and therefore not owned by the job yet.
		_ = t.proc.Kill()
	}
}

func (t *processTree) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
	for _, h := range t.members {
		_ = windows.CloseHandle(h)
	}
	t.members = nil
	if t.port != 0 {
		_ = windows.CloseHandle(t.port)
		t.port = 0
	}
}

// NEW_PROCESS 通知可能丢失，必须用累计创建数核对完整性；缺失时保守返回未完成。
// 打开后保留的进程句柄既防 PID 复用，也能证明退出，不能用 Job 活动数替代。
func (t *processTree) collectMembers() error {
	for batch := 0; batch < 1024; batch++ {
		var message uint32
		var key uintptr
		var pidValue *windows.Overlapped
		err := windows.GetQueuedCompletionStatus(t.port, &message, &key, &pidValue, 0)
		if err == windows.WAIT_TIMEOUT {
			return nil
		}
		if err != nil {
			return err
		}
		if key != 1 || message != 6 {
			continue
		} // JOB_OBJECT_MSG_NEW_PROCESS
		pid := uint32(uintptr(unsafe.Pointer(pidValue)))
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
		if err == windows.ERROR_INVALID_PARAMETER {
			t.seen++
			continue
		} // 进程对象已消失。
		if err != nil {
			return err
		}
		// 进程退出期间可能已不在 Job 活动表中，但句柄尚未收到退出信号。
		// 延迟通知遇到 PID 复用时只会保守延长等待，绝不用这些句柄发出 kill。
		t.members = append(t.members, h)
		t.seen++
	}
	return nil
}

// TerminateJobObject 会先移除活动成员再完成异步退出，因此还需逐个等待进程句柄。
func (t *processTree) wait(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		t.mu.Lock()
		var info jobAccounting
		err := t.collectMembers()
		if err == nil {
			err = windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
		}
		confirmed := err == nil && info.ActiveProcesses == 0 && info.TotalProcesses == t.seen
		if confirmed {
			for _, h := range t.members {
				var state uint32
				state, err = windows.WaitForSingleObject(h, 0)
				if err != nil || state != windows.WAIT_OBJECT_0 {
					confirmed = false
					break
				}
			}
		}
		t.mu.Unlock()
		if err != nil {
			return fmt.Errorf("确认 Job 子孙退出失败: %w", err)
		}
		if confirmed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Job 子孙尚未退出: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// treeKill remains as a fallback for callers that do not own a Job Object.
// Task execution uses processTree so descendants cannot escape.
func treeKill(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
