//go:build windows

package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsJobKillTerminatesDescendants(t *testing.T) {
	m := newTestManager(t)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := fmt.Sprintf(
		`$p = Start-Process -FilePath 'powershell.exe' -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 60' -PassThru; Set-Content -LiteralPath '%s' -Value $p.Id; Wait-Process -Id $p.Id`,
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
		t.Fatalf("child pid was not written: %v", err)
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
