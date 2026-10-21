// Package skills 将新增内置 Skill 随程序嵌入，支持只有二进制的在线升级。
package skills

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed fofa-search/SKILL.md
var fofaSearchSkill []byte

const fofaSkillV1SHA256 = "c89fdbd0217a5ab62ca8bbca0e86be654c09a97f0b80ed11946f569561144a76"

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
	tmp, err := stageFOFASkill(dir, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
				return nil
			}
		}
		return fmt.Errorf("安装 FOFA Skill: %w", err)
	}
	return nil
}

// UpgradeFOFA 只更新未修改的旧官方正文；用户删除或修改过的文件原样保留。
func UpgradeFOFA(root string) error {
	path := filepath.Join(root, "fofa-search", "SKILL.md")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 FOFA Skill 更新路径: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("FOFA Skill 更新路径不是普通文件")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 FOFA Skill 原文: %w", err)
	}
	normalized := bytes.ReplaceAll(original, []byte("\r\n"), []byte("\n"))
	if fmt.Sprintf("%x", sha256.Sum256(normalized)) != fofaSkillV1SHA256 {
		return nil
	}
	tmp, err := stageFOFASkill(filepath.Dir(path), info.Mode().Perm())
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	// 文件与数据库不能共享事务，发布前再次核对，保护迁移期间的用户编辑。
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("复核 FOFA Skill 更新路径: %w", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("复核 FOFA Skill 原文: %w", err)
	}
	if !os.SameFile(info, currentInfo) || !bytes.Equal(original, current) {
		return fmt.Errorf("FOFA Skill 在更新过程中已变更，保留用户版本")
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("更新 FOFA Skill: %w", err)
	}
	return nil
}

// 在目标目录暂存完整正文；调用方负责发布或删除，失败不留下半写入文件。
func stageFOFASkill(dir string, mode os.FileMode) (path string, err error) {
	tmp, err := os.CreateTemp(dir, ".fofa-skill-*")
	if err != nil {
		return "", fmt.Errorf("暂存 FOFA Skill: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(fofaSearchSkill); err != nil {
		return "", fmt.Errorf("写入 FOFA Skill: %w", err)
	}
	if err = tmp.Chmod(mode); err != nil {
		return "", fmt.Errorf("设置 FOFA Skill 权限: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return "", fmt.Errorf("保存 FOFA Skill: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("关闭 FOFA Skill: %w", err)
	}
	return tmp.Name(), nil
}
