package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/skill"
)

func TestInstallFOFAFromEmbeddedBinary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime-skills")
	if err := InstallFOFA(root); err != nil {
		t.Fatal(err)
	}
	reg, err := skill.LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := reg.Get("fofa-search")
	if !ok || s.Description == "" || !strings.Contains(s.Instructions, "fofa_search") {
		t.Fatalf("内置 Skill 未能从运行目录加载: %#v", s)
	}
	for _, text := range []string{"系统设置", "不会自动分页", "待验证线索", "任务范围", "服务端"} {
		if !strings.Contains(s.Instructions, text) {
			t.Fatalf("内置 Skill 缺少使用边界 %q", text)
		}
	}
}

func TestInstallFOFAPreservesExistingSkill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fofa-search")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	const custom = "---\nname: fofa-search\ndescription: 用户维护版本\n---\n用户修改的查询流程\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallFOFA(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != custom {
		t.Fatalf("现有 Skill 被覆盖: content=%q err=%v", got, err)
	}
}

func TestInstallFOFARejectsDirectoryAsSkillFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fofa-search", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallFOFA(root); err == nil {
		t.Fatal("目标是目录时应返回安装错误")
	}
}
