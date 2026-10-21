package server

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	builtinskills "github.com/Autumn-27/artex/skills"
)

// 在独立 schema 中验证首次安装与升级，避免改变测试数据库中的 Agent 偏好。
func builtinSkillTestServer(t *testing.T) *Server {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("未配置隔离测试数据库: %v", err)
	}
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	pg.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pg.Close() })
	schema := fmt.Sprintf("fofa_skill_%d", time.Now().UnixNano())
	if _, err := pg.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	if _, err := pg.Exec(`SET search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`
CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL, updated_at timestamptz DEFAULT now());
CREATE TABLE agents (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, key text UNIQUE NOT NULL);
CREATE TABLE agent_skill_visibility (agent_id bigint REFERENCES agents(id), skill_name text NOT NULL, enabled boolean NOT NULL, PRIMARY KEY(agent_id,skill_name));
INSERT INTO agents(key) VALUES ('worker'),('auto'),('pentest'),('mainagent'),('planner');`); err != nil {
		t.Fatal(err)
	}
	return &Server{m: &Manager{pg: &db.DB{DB: pg}}, skillDir: filepath.Join(t.TempDir(), "skills")}
}

func TestBuiltinFOFASkillContentMigrationPreservesChoices(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("file_present_%v", present), func(t *testing.T) {
			s := builtinSkillTestServer(t)
			if err := s.m.pg.SetSetting(settingBuiltinFOFASkill, "done"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.skillDir, "fofa-search", "SKILL.md")
			const userBody = "用户保留的 FOFA 操作流程"
			if present {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(userBody), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.seedFOFASkill(); err != nil {
				t.Fatal(err)
			}
			if value, ok, err := s.m.pg.GetSetting(settingBuiltinFOFASkillContentV2); err != nil || !ok || value != "done" {
				t.Fatalf("迁移未记录完成: value=%q present=%v err=%v", value, ok, err)
			}
			if present {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != userBody {
					t.Fatal("内容迁移覆盖了用户版本")
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("内容迁移恢复了用户删除的 Skill")
			}
			var bindings int
			if err := s.m.pg.QueryRow(`SELECT count(*) FROM agent_skill_visibility WHERE skill_name='fofa-search'`).Scan(&bindings); err != nil || bindings != 0 {
				t.Fatalf("内容迁移恢复了用户解绑: count=%d err=%v", bindings, err)
			}
		})
	}
}

func TestBuiltinFOFASkillContentMigrationFailureCanRetry(t *testing.T) {
	s := builtinSkillTestServer(t)
	if err := s.m.pg.SetSetting(settingBuiltinFOFASkill, "done"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.skillDir, "fofa-search", "SKILL.md")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err == nil {
		t.Fatal("错误路径应使内容迁移失败")
	}
	if _, ok, err := s.m.pg.GetSetting(settingBuiltinFOFASkillContentV2); err != nil || ok {
		t.Fatalf("失败迁移不能记录完成: present=%v err=%v", ok, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := builtinskills.InstallFOFA(s.skillDir); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := s.m.pg.GetSetting(settingBuiltinFOFASkillContentV2); err != nil || !ok || value != "done" {
		t.Fatalf("重试迁移未记录完成: value=%q present=%v err=%v", value, ok, err)
	}
	var bindings int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM agent_skill_visibility WHERE skill_name='fofa-search'`).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("重试迁移恢复了用户解绑: count=%d err=%v", bindings, err)
	}
}

func assertFOFASkillSeeded(t *testing.T, s *Server) {
	t.Helper()
	var names string
	if err := s.m.pg.QueryRow(`SELECT string_agg(a.key,',' ORDER BY a.key) FROM agent_skill_visibility v JOIN agents a ON a.id=v.agent_id WHERE v.skill_name='fofa-search' AND v.enabled`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if names != "auto,pentest,worker" {
		t.Fatalf("默认绑定=%q，期望 auto,pentest,worker", names)
	}
	if value, ok, err := s.m.pg.GetSetting(settingBuiltinFOFASkill); err != nil || !ok || value != "done" {
		t.Fatalf("成功标记: value=%q ok=%v err=%v", value, ok, err)
	}
}

func TestBuiltinFOFASkillFreshInstallAndRestart(t *testing.T) {
	s := builtinSkillTestServer(t)
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	assertFOFASkillSeeded(t, s)
	path := filepath.Join(s.skillDir, "fofa-search", "SKILL.md")
	const custom = "用户定制 Skill"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`DELETE FROM agent_skill_visibility WHERE skill_name='fofa-search'`); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != custom {
		t.Fatalf("重启覆盖了用户修改: content=%q err=%v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("重启恢复了用户删除的 Skill: %v", err)
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM agent_skill_visibility WHERE skill_name='fofa-search'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("重启恢复了用户解绑: count=%d err=%v", count, err)
	}
}

func TestBuiltinFOFASkillUpgradePreservesExistingPreferences(t *testing.T) {
	s := builtinSkillTestServer(t)
	path := filepath.Join(s.skillDir, "fofa-search", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const custom = "已有用户提供的 FOFA Skill"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO agent_skill_visibility SELECT id,'fofa-search',false FROM agents WHERE key='auto'`); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != custom {
		t.Fatalf("升级覆盖已有 Skill: content=%q err=%v", got, err)
	}
	var enabled bool
	if err := s.m.pg.QueryRow(`SELECT enabled FROM agent_skill_visibility v JOIN agents a ON a.id=v.agent_id WHERE a.key='auto' AND skill_name='fofa-search'`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("升级覆盖已有禁用偏好: enabled=%v err=%v", enabled, err)
	}
	var names string
	if err := s.m.pg.QueryRow(`SELECT string_agg(a.key,',' ORDER BY a.key) FROM agent_skill_visibility v JOIN agents a ON a.id=v.agent_id WHERE v.skill_name='fofa-search' AND v.enabled`).Scan(&names); err != nil || names != "pentest,worker" {
		t.Fatalf("升级未补齐缺失绑定: names=%q err=%v", names, err)
	}
}

func TestBuiltinFOFASkillFailureCanRetry(t *testing.T) {
	s := builtinSkillTestServer(t)
	if err := os.WriteFile(s.skillDir, []byte("阻止创建目录"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err == nil {
		t.Fatal("安装失败应返回错误")
	}
	if _, ok, err := s.m.pg.GetSetting(settingBuiltinFOFASkill); err != nil || ok {
		t.Fatalf("安装失败不应记成功: ok=%v err=%v", ok, err)
	}
	if err := os.Remove(s.skillDir); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	assertFOFASkillSeeded(t, s)
}

func TestBuiltinFOFASkillBindingFailureRollsBack(t *testing.T) {
	s := builtinSkillTestServer(t)
	// 最后一个 Agent 缺失时，前两个已执行的绑定也必须整体回滚。
	if _, err := s.m.pg.Exec(`DELETE FROM agents WHERE key='pentest'`); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err == nil {
		t.Fatal("缺少默认 Agent 时不应记成功")
	}
	if _, ok, err := s.m.pg.GetSetting(settingBuiltinFOFASkill); err != nil || ok {
		t.Fatalf("绑定失败不应记成功: ok=%v err=%v", ok, err)
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM agent_skill_visibility`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("绑定失败未回滚: count=%d err=%v", count, err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO agents(key) VALUES ('pentest')`); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFASkill(); err != nil {
		t.Fatal(err)
	}
	assertFOFASkillSeeded(t, s)
}
