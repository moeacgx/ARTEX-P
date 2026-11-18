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

var isProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

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
	members map[uint32]*processMember
	seen    uint32
}

type processIdentity struct {
	pid     uint32
	created uint64
}

type processMember struct {
	identity processIdentity
	known    bool
	gone     bool
	pin      windows.Handle
	wait     windows.Handle
	err      error
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
	return &processTree{
		job:     job,
		port:    port,
		members: make(map[uint32]*processMember),
	}, nil
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
	for _, member := range t.members {
		member.close()
	}
	t.members = nil
	if t.port != 0 {
		_ = windows.CloseHandle(t.port)
		t.port = 0
	}
}

func (m *processMember) close() {
	if m.pin != 0 {
		_ = windows.CloseHandle(m.pin)
		m.pin = 0
	}
	if m.wait != 0 {
		_ = windows.CloseHandle(m.wait)
		m.wait = 0
	}
}

func processCreationTime(handle windows.Handle) (uint64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

func (t *processTree) addMember(pid uint32) error {
	if member, exists := t.members[pid]; exists {
		// 持有句柄时固定原对象；消失对象无法区分代次，只能保守去重。
		return member.err
	}
	member := &processMember{identity: processIdentity{pid: pid}}
	t.members[pid] = member
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err == windows.ERROR_INVALID_PARAMETER {
		// 通知已证明该 PID 曾属于 Job，当前已不存在相应进程对象。
		member.gone = true
		t.seen++
		return nil
	}
	if err != nil {
		member.err = err
		return err
	}
	member.pin = h
	return t.acquireWaitHandle(member)
}

func (t *processTree) acquireWaitHandle(member *processMember) error {
	if member.gone || member.wait != 0 || member.pin == 0 {
		return member.err
	}
	if !member.known {
		// 失败后仅从已固定的对象重试查询，绝不重新相信裸 PID。
		var inJob int32
		ok, _, err := isProcessInJob.Call(uintptr(member.pin), uintptr(t.job), uintptr(unsafe.Pointer(&inJob)))
		if ok == 0 {
			member.err = err
			return err
		}
		if inJob == 0 {
			member.err = fmt.Errorf("PID %d 无法确认属于原 Job", member.identity.pid)
			return member.err
		}
		created, err := processCreationTime(member.pin)
		if err != nil {
			member.err = err
			return err
		}
		member.identity.created = created
		member.known = true
		t.seen++
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, member.identity.pid)
	if err != nil {
		member.err = err
		return err
	}
	created, err := processCreationTime(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		member.err = err
		return err
	}
	if created != member.identity.created {
		_ = windows.CloseHandle(h)
		member.err = fmt.Errorf("PID %d 已复用，拒绝替代原 Job 成员", member.identity.pid)
		return member.err
	}
	member.wait = h
	member.err = nil
	if member.pin != 0 {
		_ = windows.CloseHandle(member.pin)
		member.pin = 0
	}
	return nil
}

// 每条 NEW_PROCESS 通知先保存为成员或待解析项；失败不丢失恢复状态。
// 查询句柄固定进程对象并记录创建时间，防止重试时把复用 PID 当作原成员。
func (t *processTree) collectMembers() error {
	var firstErr error
	for batch := 0; batch < 1024; batch++ {
		var message uint32
		var key uintptr
		var pidValue *windows.Overlapped
		err := windows.GetQueuedCompletionStatus(t.port, &message, &key, &pidValue, 0)
		if err == windows.WAIT_TIMEOUT {
			break
		}
		if err != nil {
			return err
		}
		if key != 1 || message != 6 {
			continue
		} // JOB_OBJECT_MSG_NEW_PROCESS
		pid := uint32(uintptr(unsafe.Pointer(pidValue)))
		if err := t.addMember(pid); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, member := range t.members {
		if err := t.acquireWaitHandle(member); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
			for _, member := range t.members {
				if member.gone {
					continue
				}
				var state uint32
				state, err = windows.WaitForSingleObject(member.wait, 0)
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
