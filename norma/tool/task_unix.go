//go:build !windows && !linux

package tool

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

type processTree struct {
	proc *os.Process
}

func preparePTYTree(cmd *exec.Cmd) (*processTree, error) { return nil, nil }

// prepareProcessTree puts the child in its own process group so it survives the
// per-turn context and can be killed as a whole tree.
func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return &processTree{}, nil
}

func (t *processTree) attach(p *os.Process) error {
	t.proc = p
	return nil
}
func (t *processTree) kill() {
	if t != nil && t.proc != nil {
		treeKill(t.proc)
	}
}
func (t *processTree) close()                         {}
func (t *processTree) wait(ctx context.Context) error { return nil }

// treeKill terminates the process group, falling back to the lone process.
func treeKill(p *os.Process) {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		_ = p.Kill()
	}
}
