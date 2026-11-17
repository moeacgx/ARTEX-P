//go:build windows

package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsDescendantHelper(t *testing.T) {
	role := os.Getenv("NORMA_TEST_WINDOWS_CHILD")
	if role == "" {
		return
	}
	if role == "leaf" {
		// 不允许删除的实际文件句柄用于检验输出生命周期。
		path, _ := windows.UTF16PtrFromString(os.Getenv("NORMA_TEST_WINDOWS_HELD"))
		h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			os.Exit(2)
		}
		defer windows.CloseHandle(h)
		_ = os.WriteFile(os.Getenv("NORMA_TEST_WINDOWS_PID"), []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(40 * time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantHelper$")
	cmd.Env = append(os.Environ(), "NORMA_TEST_WINDOWS_CHILD=leaf")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(3)
	}
	for i := 0; i < 5000; i++ {
		if _, e := os.Stat(os.Getenv("NORMA_TEST_WINDOWS_PID")); e == nil {
			if os.Getenv("NORMA_TEST_WINDOWS_HOLD") == "1" {
				time.Sleep(40 * time.Second)
			}
			os.Exit(0)
		}
		time.Sleep(time.Millisecond)
	}
	os.Exit(4)
}

func TestWindowsRootExitConfirmsJobChildren(t *testing.T) {
	m := newTestManager(t)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	held := filepath.Join(t.TempDir(), "held.output")
	task, err := m.Spawn(SpawnSpec{Command: fmt.Sprintf(`& '%s' '-test.run=^TestWindowsDescendantHelper$'`, strings.ReplaceAll(os.Args[0], "'", "''")), Env: []string{"NORMA_TEST_WINDOWS_CHILD=root", "NORMA_TEST_WINDOWS_PID=" + pidPath, "NORMA_TEST_WINDOWS_HELD=" + held}})
	if err != nil {
		t.Fatal(err)
	}
	if _, done := m.Wait(context.Background(), task.ID, 10*time.Second); !done {
		t.Fatal("Job 子孙退出未获确认")
	}
	if _, err := os.Stat(pidPath); err != nil {
		out, _ := m.Output(task.ID, 0)
		t.Fatalf("子孙未就绪: %v; output=%s", err, out)
	}
	if err := os.Remove(held); err != nil {
		raw, _ := os.ReadFile(pidPath)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		h, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		var state uint32
		if openErr == nil {
			state, _ = windows.WaitForSingleObject(h, 0)
			windows.CloseHandle(h)
		}
		t.Fatalf("done 关闭后子孙仍持有输出: %v; pid=%d state=%d openErr=%v", err, pid, state, openErr)
	}
	m.Forget(task.ID)
	if _, err := os.Stat(task.OutputPath); !os.IsNotExist(err) {
		t.Fatalf("退出确认后输出无法删除: %v", err)
	}
}

func TestWindowsTaskIsolation(t *testing.T) {
	m := newTestManager(t)
	var tasks []*Task
	var handles []windows.Handle
	for i := 0; i < 2; i++ {
		pidPath := filepath.Join(t.TempDir(), "pid")
		task, err := m.Spawn(SpawnSpec{Command: fmt.Sprintf(`& '%s' '-test.run=^TestWindowsDescendantHelper$'`, strings.ReplaceAll(os.Args[0], "'", "''")), Env: []string{"NORMA_TEST_WINDOWS_CHILD=root", "NORMA_TEST_WINDOWS_HOLD=1", "NORMA_TEST_WINDOWS_PID=" + pidPath, "NORMA_TEST_WINDOWS_HELD=" + filepath.Join(t.TempDir(), "output")}})
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			raw, e := os.ReadFile(pidPath)
			if e == nil {
				pid, _ = strconv.Atoi(string(raw))
				if pid > 0 {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		if pid == 0 {
			t.Fatal("隔离测试子孙未就绪")
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = windows.CloseHandle(h) })
		tasks = append(tasks, task)
		handles = append(handles, h)
	}
	m.Kill(tasks[0].ID)
	if _, done := m.Wait(context.Background(), tasks[0].ID, 5*time.Second); !done {
		t.Fatal("第一个 Job 未退出")
	}
	if state, err := windows.WaitForSingleObject(handles[0], 0); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("子孙未退出 state=%d err=%v", state, err)
	}
	if state, err := windows.WaitForSingleObject(handles[1], 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("另一个 Job 被误杀 state=%d err=%v", state, err)
	}
	m.Kill(tasks[1].ID)
	if _, done := m.Wait(context.Background(), tasks[1].ID, 5*time.Second); !done {
		t.Fatal("第二个 Job 未退出")
	}
}

func TestWindowsNoDescendantsReturnsPromptly(t *testing.T) {
	m := newTestManager(t)
	task, err := m.Spawn(SpawnSpec{Command: "exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, done := m.Wait(context.Background(), task.ID, 3*time.Second); !done {
		t.Fatal("无子孙的正常任务未及时返回")
	}
}

func TestWindowsMissingJobNotificationFailsClosed(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantHelper$")
	cmd.Env = append(os.Environ(), "NORMA_TEST_WINDOWS_CHILD=leaf", "NORMA_TEST_WINDOWS_HELD="+filepath.Join(t.TempDir(), "held"), "NORMA_TEST_WINDOWS_PID="+filepath.Join(t.TempDir(), "pid"))
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		tree.close()
		t.Fatal(err)
	}
	if err = tree.attach(cmd.Process); err != nil {
		tree.kill()
		_ = cmd.Wait()
		tree.close()
		t.Fatal(err)
	}
	defer tree.close()
	var message uint32
	var key uintptr
	var pid *windows.Overlapped
	// 人为丢弃实际内核创建通知，检验累计数量不一致时保持未完成。
	err = windows.GetQueuedCompletionStatus(tree.port, &message, &key, &pid, 1000)
	tree.kill()
	_ = cmd.Wait()
	if err != nil || message != 6 {
		t.Fatalf("未取得真实创建通知: msg=%d err=%v", message, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tree.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("缺失归属证据仍宣告完成: %v", err)
	}
	if tree.job == 0 {
		t.Fatal("证据缺失后 Job 已释放")
	}
}

func TestWindowsCleanupUnconfirmedRetainsTask(t *testing.T) {
	m := newTestManager(t)
	task, err := m.Spawn(SpawnSpec{Command: "Start-Sleep -Seconds 40"})
	if err != nil {
		t.Fatal(err)
	}
	var message uint32
	var key uintptr
	var pid *windows.Overlapped
	if err = windows.GetQueuedCompletionStatus(task.tree.port, &message, &key, &pid, 1000); err != nil || message != 6 {
		t.Fatalf("创建通知缺失 msg=%d err=%v", message, err)
	}
	// 暂扣再恢复本测试的真实内核通知，观察清理失败的资源保留及可重试行为。
	defer func() {
		if err := windows.PostQueuedCompletionStatus(task.tree.port, message, key, pid); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := m.CleanupAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.CleanupAndWait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("缺少退出证据仍清理成功: %v", err)
	}
	m.Forget(task.ID)
	if _, ok := m.Get(task.ID); !ok {
		t.Fatal("未确认任务被遗忘")
	}
	if _, err := os.Stat(task.OutputPath); err != nil {
		t.Fatalf("输出过早删除: %v", err)
	}
	if task.tree.job == 0 {
		t.Fatal("未确认 Job 已释放")
	}
	if info, _ := m.Get(task.ID); info.Status.Terminal() {
		t.Fatal("未确认退出提前成为终态")
	}
}

// 内核返回的异步终止状态直接复现评论，避免依赖调度器恰好放大 watch 竞态。
func TestWindowsJobTerminationIsAsynchronous(t *testing.T) {
	observed := false
	for i := 0; i < 20; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantHelper$")
		cmd.Env = append(os.Environ(), "NORMA_TEST_WINDOWS_CHILD=leaf", "NORMA_TEST_WINDOWS_HELD="+filepath.Join(t.TempDir(), "held"), "NORMA_TEST_WINDOWS_PID="+filepath.Join(t.TempDir(), "pid"))
		tree, err := prepareProcessTree(cmd)
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			tree.close()
			t.Fatal(err)
		}
		if err = tree.attach(cmd.Process); err != nil {
			tree.kill()
			_ = cmd.Wait()
			tree.close()
			t.Fatal(err)
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		tree.kill()
		state, err := windows.WaitForSingleObject(h, 0)
		if err != nil {
			t.Fatal(err)
		}
		observed = observed || state == uint32(windows.WAIT_TIMEOUT)
		_ = cmd.Wait()
		tree.close()
		windows.CloseHandle(h)
	}
	t.Logf("实际观察到 TerminateJobObject 返回后进程尚未退出: %v", observed)
	if !observed {
		t.Skip("本次调度未观察到异步窗口，不能据此否定竞态")
	}
}

func TestWindowsJobConfirmationTimeoutRetainsHandle(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantHelper$")
	cmd.Env = append(os.Environ(), "NORMA_TEST_WINDOWS_CHILD=leaf", "NORMA_TEST_WINDOWS_HELD="+filepath.Join(t.TempDir(), "held"), "NORMA_TEST_WINDOWS_PID="+filepath.Join(t.TempDir(), "pid"))
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		tree.close()
		t.Fatal(err)
	}
	if err = tree.attach(cmd.Process); err != nil {
		tree.kill()
		_ = cmd.Wait()
		tree.close()
		t.Fatal(err)
	}
	defer func() {
		tree.kill()
		_ = cmd.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tree.wait(ctx); err != nil {
			t.Error(err)
		}
		tree.close()
	}()
	job := tree.job
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tree.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("存活 Job 必须返回确认超时: %v", err)
	}
	if tree.job != job || tree.job == 0 {
		t.Fatal("超时后 Job 被提前关闭")
	}
}

func TestWindowsJobOpenProcessPermissionRecovery(t *testing.T) {
	testWindowsJobPermissionRecovery(t, false, false)
}

func TestWindowsJobPermissionRecoveryReleasesManager(t *testing.T) {
	testWindowsJobPermissionRecovery(t, false, true)
}

func TestWindowsJobUnavailableIdentityStaysUnconfirmed(t *testing.T) {
	testWindowsJobPermissionRecovery(t, true, false)
}

func testWindowsJobPermissionRecovery(t *testing.T, denyIdentity, useManager bool) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	m := newTestManager(t)
	output, err := os.Create(filepath.Join(m.dir, "permission.output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantHelper$")
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Env = append(os.Environ(),
		"NORMA_TEST_WINDOWS_CHILD=leaf",
		"NORMA_TEST_WINDOWS_HELD="+filepath.Join(t.TempDir(), "held"),
		"NORMA_TEST_WINDOWS_PID="+filepath.Join(t.TempDir(), "pid"),
	)
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		tree.close()
		t.Fatal(err)
	}
	watching := false
	defer func() {
		tree.kill()
		if !watching {
			_ = cmd.Wait()
			tree.close()
		}
	}()
	if err := tree.attach(cmd.Process); err != nil {
		t.Fatal(err)
	}

	processHandle, err := windows.OpenProcess(
		windows.READ_CONTROL|windows.WRITE_DAC|windows.SYNCHRONIZE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(processHandle)
	security, err := windows.GetSecurityInfo(processHandle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	originalDACL, _, err := security.DACL()
	if err != nil {
		t.Fatal(err)
	}
	deniedRights := windows.SYNCHRONIZE
	if denyIdentity {
		deniedRights |= windows.PROCESS_QUERY_LIMITED_INFORMATION
	}
	// 只修改本测试新建的合成进程对象，宿主和其它进程权限保持原样。
	denySD, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:(D;;0x%x;;;WD)(A;;GA;;;WD)", deniedRights))
	if err != nil {
		t.Fatal(err)
	}
	denyDACL, _, err := denySD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(processHandle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, denyDACL, nil); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.SetSecurityInfo(processHandle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, originalDACL, nil); err != nil {
			t.Errorf("恢复合成进程 DACL: %v", err)
		}
	}()

	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		t.Fatal(err)
	}
	defer windows.RevertToSelf()
	thread, err := windows.GetCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	var token windows.Token
	if err := windows.OpenThreadToken(thread, windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, true, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	if err := windows.AdjustTokenPrivileges(token, true, nil, 0, nil, nil); err != nil {
		t.Fatal(err)
	}

	check, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err == nil {
		_ = windows.CloseHandle(check)
		t.Skip("宿主已启用 debug 特权，无法制造 SYNCHRONIZE 拒绝")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("预期 SYNCHRONIZE 访问拒绝，得到 %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err = tree.wait(ctx)
	cancel()
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("首次等待未报告真实权限错误: %v", err)
	}
	t.Logf("真实访问拒绝：seen=%d members=%d Job保留=%t", tree.seen, len(tree.members), tree.job != 0)
	if tree.job == 0 {
		t.Fatal("权限失败后 Job 提前释放")
	}
	var task *Task
	if useManager {
		// 在同一受限线程先消费真实通知，再交给生产 watch 路径验证重试。
		task = &Task{ID: "permission", Status: TaskRunning, OutputPath: output.Name(), cmd: cmd, tree: tree, done: make(chan struct{})}
		m.tasks[task.ID] = task
		m.order = append(m.order, task.ID)
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
		err = m.CleanupAndWait(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("尚未确认时清理未返回超时: %v", err)
		}
		m.Forget(task.ID)
		if info, ok := m.Get(task.ID); !ok || info.Status.Terminal() {
			t.Fatal("权限失败后活动门禁被释放")
		}
		if _, err := os.Stat(task.OutputPath); err != nil {
			t.Fatalf("权限失败后输出被删除: %v", err)
		}
	}

	if err := windows.SetSecurityInfo(processHandle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, originalDACL, nil); err != nil {
		t.Fatal(err)
	}
	check, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("权限恢复后仍无法打开: %v", err)
	}
	_ = windows.CloseHandle(check)

	tree.kill()
	if useManager {
		watching = true
		go m.watch(task, output)
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		err = m.CleanupAndWait(ctx)
		cancel()
		if err != nil {
			t.Fatalf("权限恢复后管理器仍无法完成清理: %v", err)
		}
		if _, err := os.Stat(task.OutputPath); !os.IsNotExist(err) {
			t.Fatalf("真实退出确认后输出仍未清理: %v", err)
		}
		if tree.job != 0 {
			t.Fatal("真实退出确认后 Job 未关闭")
		}
		if _, ok := m.Get(task.ID); !ok {
			t.Fatal("成功清理前任务注册丢失")
		}
		m.Forget(task.ID)
		if _, ok := m.Get(task.ID); ok {
			t.Fatal("恢复退出确认后任务仍不可遗忘")
		}
	} else {
		_ = cmd.Wait()
	}
	state, err := windows.WaitForSingleObject(processHandle, 1000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("合成进程未真实退出: state=%d error=%v", state, err)
	}
	if useManager {
		t.Log("权限恢复后生产 watch 完成，活动门禁及输出清理成功，独立进程句柄 signaled")
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = tree.wait(ctx)
	if denyIdentity {
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || tree.job == 0 {
			t.Fatalf("未固定身份仍被放行: err=%v Job=%d", err, tree.job)
		}
		t.Logf("无法固定身份时权限恢复仍保持未完成：seen=%d err=%v", tree.seen, err)
		return
	}
	if err != nil {
		t.Fatalf("权限恢复且进程真实退出后仍无法确认: seen=%d error=%v", tree.seen, err)
	}
	t.Logf("权限恢复后同一成员退出确认成功：seen=%d members=%d", tree.seen, len(tree.members))
}

func newWindowsJobFixture(t *testing.T) (*exec.Cmd, *processTree) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantHelper$")
	cmd.Env = append(os.Environ(),
		"NORMA_TEST_WINDOWS_CHILD=leaf",
		"NORMA_TEST_WINDOWS_HELD="+filepath.Join(t.TempDir(), "held"),
		"NORMA_TEST_WINDOWS_PID="+filepath.Join(t.TempDir(), "pid"),
	)
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		tree.close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tree.kill()
		_ = cmd.Wait()
		tree.close()
	})
	if err := tree.attach(cmd.Process); err != nil {
		t.Fatal(err)
	}
	return cmd, tree
}

func TestWindowsJobDuplicateNotificationDoesNotCountTwice(t *testing.T) {
	cmd, tree := newWindowsJobFixture(t)
	var message uint32
	var key uintptr
	var pid *windows.Overlapped
	if err := windows.GetQueuedCompletionStatus(tree.port, &message, &key, &pid, 1000); err != nil || message != 6 {
		t.Fatalf("未取得真实创建通知: msg=%d err=%v", message, err)
	}
	for i := 0; i < 2; i++ {
		if err := windows.PostQueuedCompletionStatus(tree.port, message, key, pid); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := tree.wait(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || tree.seen != 1 || len(tree.members) != 1 {
		t.Fatalf("重复通知破坏成员完整性: err=%v seen=%d members=%d", err, tree.seen, len(tree.members))
	}
	tree.kill()
	_ = cmd.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tree.wait(ctx); err != nil {
		t.Fatalf("重复通知使退出确认无法收敛: %v", err)
	}
}

func TestWindowsJobRejectsAnotherJobsProcessIdentity(t *testing.T) {
	_, tree := newWindowsJobFixture(t)
	other, otherTree := newWindowsJobFixture(t)
	err := tree.addMember(uint32(other.Process.Pid))
	if err == nil || tree.seen != 0 {
		t.Fatalf("另一个 Job 的裸 PID 被当作可信成员: err=%v seen=%d", err, tree.seen)
	}
	member := tree.members[uint32(other.Process.Pid)]
	// 仅恢复本测试人为插入的错误记录，不改变真实内核通知。
	delete(tree.members, uint32(other.Process.Pid))
	member.close()
	otherTree.kill()
	_ = other.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := otherTree.wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsJobRejectsChangedCreationTime(t *testing.T) {
	cmd, tree := newWindowsJobFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := tree.wait(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	member := tree.members[uint32(cmd.Process.Pid)]
	if member == nil || member.wait == 0 || !member.known {
		t.Fatal("未固定实际成员身份")
	}
	pin, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	member.pin = pin
	_ = windows.CloseHandle(member.wait)
	member.wait = 0
	original := member.identity.created
	defer func() { member.identity.created = original }()
	// 不强迫系统复用 PID：使用真实进程句柄，注入代次不匹配验证拒绝分支。
	member.identity.created++
	if err := tree.acquireWaitHandle(member); err == nil || member.wait != 0 {
		t.Fatalf("创建时间不一致仍接受替代句柄: err=%v handle=%d", err, member.wait)
	}
	member.identity.created = original
	tree.kill()
	_ = cmd.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tree.wait(ctx); err != nil {
		t.Fatalf("恢复真实身份后无法确认: %v", err)
	}
}

func TestWindowsJobKillTerminatesDescendants(t *testing.T) {
	m := newTestManager(t)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := fmt.Sprintf(
		`$p = Start-Process -WindowStyle Hidden -FilePath 'powershell.exe' -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 60' -PassThru; Set-Content -LiteralPath '%s' -Value $p.Id; Wait-Process -Id $p.Id`,
		strings.ReplaceAll(pidPath, "'", "''"),
	)
	task, err := m.Spawn(SpawnSpec{Command: script})
	if err != nil {
		t.Fatal(err)
	}

	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err == nil && pid > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid <= 0 {
		out, _ := m.Output(task.ID, 0)
		t.Fatalf("child pid was not written: %v; output=%s", err, out)
	}

	if !m.Kill(task.ID) {
		t.Fatal("task was not running")
	}
	waitTerminal(t, m, task.ID)

	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == windows.ERROR_INVALID_PARAMETER {
		return
	}
	if err != nil {
		t.Fatalf("open descendant %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 5000)
	if err != nil {
		t.Fatalf("wait descendant %d: %v", pid, err)
	}
	if state != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant %d survived task termination", pid)
	}
}
