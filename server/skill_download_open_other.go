//go:build !unix

package server

import "os"

// Windows 的 Root 限制设备名及目录边界，打开后仍须检查常规文件类型。
func openSkillArchiveFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
