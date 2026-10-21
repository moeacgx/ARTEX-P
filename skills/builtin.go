// Package skills 将新增内置 Skill 随程序嵌入，支持只有二进制的在线升级。
package skills

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed fofa-search/SKILL.md
var fofaSearchSkill []byte

// InstallFOFA 仅创建缺失的 Skill，不覆盖用户已有内容；是否已安装由调用方持久化。
func InstallFOFA(root string) error {
	dir := filepath.Join(root, "fofa-search")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 FOFA Skill 目录: %w", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("FOFA Skill 路径不是普通文件")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 FOFA Skill: %w", err)
	}

	// 先完整写入同目录临时文件，再以硬链接原子发布，避免崩溃留下半个 Skill
	// 或覆盖安装期间用户刚创建的版本。临时文件仅在本次安装中存在。
	tmp, err := os.CreateTemp(dir, ".fofa-skill-*")
	if err != nil {
		return fmt.Errorf("暂存 FOFA Skill: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(fofaSearchSkill); err != nil {
		return fmt.Errorf("写入 FOFA Skill: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("设置 FOFA Skill 权限: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("保存 FOFA Skill: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 FOFA Skill: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if os.IsExist(err) {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
				return nil
			}
		}
		return fmt.Errorf("安装 FOFA Skill: %w", err)
	}
	return nil
}
