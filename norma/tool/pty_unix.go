//go:build !windows

package tool

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startPTY launches cmd attached to a new pseudo-terminal of the given size and
// returns the master file (read output / write input). Unix implementation via
// creack/pty. ptySupported reports platform support.
func startPTY(cmd *exec.Cmd, rows, cols int) (*os.File, *processTree, error) {
	tree, err := preparePTYTree(cmd)
	if err != nil {
		return nil, nil, err
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		if tree != nil {
			tree.close()
		}
		return nil, nil, err
	}
	if tree != nil {
		if err = tree.attach(cmd.Process); err != nil {
			tree.kill()
			return f, tree, err
		}
	}
	return f, tree, nil
}

func ptySupported() bool { return true }
