//go:build unix

package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDownloadSkillFIFOOpenDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// 模拟预检后目录或文件已变为 FIFO；没有写端时打开也必须立即返回。
	file, err := openSkillArchiveFile(root, "pipe")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	_, err = (skillArchiveFS{root, context.Background()}).ReadDir("pipe")
	if !errors.Is(err, errSkillArchiveInvalid) {
		t.Fatalf("目录被换成 FIFO 后应拒绝：%v", err)
	}
}
