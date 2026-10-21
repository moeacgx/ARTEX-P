//go:build unix

package server

import (
	"os"
	"syscall"
)

// 非阻塞防止预检后的 FIFO 替换卡住读取；目录边界由 Root 保证。
func openSkillArchiveFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
