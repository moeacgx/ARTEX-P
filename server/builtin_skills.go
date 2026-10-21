package server

import (
	"database/sql"
	"errors"
	"fmt"

	builtinskills "github.com/Autumn-27/artex/skills"
)

const settingBuiltinFOFASkill = "builtin_skill_fofa_search_v1"
const builtinFOFASkillLock int64 = 7337741017

// seedFOFASkill 将内置说明安装到实际 SkillDir，并且只在首次成功时绑定默认 Agent。
// 成功标记保留在数据库中，因此用户后续删除文件或解除绑定不会在重启后恢复。
func (s *Server) seedFOFASkill() error {
	if s.m == nil || s.m.pg == nil {
		return nil
	}
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, builtinFOFASkillLock); err != nil {
		return err
	}
	var value string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, settingBuiltinFOFASkill).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if value == "done" {
		return nil
	}
	if err := builtinskills.InstallFOFA(s.skillDir); err != nil {
		return err
	}
	for _, key := range []string{"worker", "auto", "pentest"} {
		var agentID int64
		if err := tx.QueryRow(`SELECT id FROM agents WHERE key=$1`, key).Scan(&agentID); err != nil {
			return fmt.Errorf("查询 FOFA Skill 默认 Agent %s: %w", key, err)
		}
		if _, err := tx.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled)
VALUES ($1,'fofa-search',true) ON CONFLICT (agent_id,skill_name) DO NOTHING`, agentID); err != nil {
			return fmt.Errorf("绑定 FOFA Skill 到 %s: %w", key, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'done')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, settingBuiltinFOFASkill); err != nil {
		return err
	}
	return tx.Commit()
}
