package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/catalog"
)

// CatalogTool 只含仓库可管理的字段，不允许覆盖启用状态、绑定或实例凭据。
type CatalogTool struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Schema      json.RawMessage `json:"schema"`
	Exec        json.RawMessage `json:"exec"`
	Deferred    bool            `json:"deferred"`
}

type CatalogAgent struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

// CatalogPublicExec 在冲突摘要与预览中排除实例头部、环境变量、代理凭据。
func CatalogPublicExec(raw json.RawMessage) json.RawMessage {
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`{}`)
	}
	delete(v, "headers")
	delete(v, "env")
	delete(v, "proxy")
	b, _ := json.Marshal(v)
	return b
}

func catalogMergeExec(next, old json.RawMessage) json.RawMessage {
	var n, o map[string]json.RawMessage
	_ = json.Unmarshal(next, &n)
	_ = json.Unmarshal(old, &o)
	if n == nil {
		n = map[string]json.RawMessage{}
	}
	for _, k := range []string{"headers", "env", "proxy"} {
		if v, ok := o[k]; ok {
			n[k] = v
		}
	}
	b, _ := json.Marshal(n)
	return b
}

func CatalogToolPayload(t *Tool) []byte {
	b, _ := json.Marshal(CatalogTool{t.Key, t.Description, t.Kind, t.Schema, CatalogPublicExec(t.Exec), t.Deferred})
	return catalog.Canonical(b)
}
func CatalogAgentPayload(a *Agent, prompt string) []byte {
	b, _ := json.Marshal(CatalogAgent{a.Key, a.Name, a.Description, prompt})
	return b
}

// CatalogApply 与目录安装记录共用事务。工具/Agent变更失败不会留下半安装记录。
// expectedHash 在行锁内复核，防止用户在预览后编辑了资源。
func (d *DB) CatalogApply(ctx context.Context, kind, key, expectedHash string, payload []byte, tools, skills []string, recordKey, recordValue, sourceValue string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 所有目录写入串行，且与源配置的更新互斥。
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741042)`); err != nil {
		return err
	}
	var currentSource string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='catalog.source'`).Scan(&currentSource)
	if err == sql.ErrNoRows {
		b, _ := json.Marshal(catalog.DefaultSource())
		currentSource = string(b)
	} else if err != nil {
		return err
	}
	if currentSource != sourceValue {
		return fmt.Errorf("目录源已变化，请重新预览")
	}
	if kind == "tool" {
		old, err := scanTool(tx.QueryRowContext(ctx, `SELECT `+toolCols+` FROM tools WHERE key=$1 FOR UPDATE`, key))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		h := "absent"
		if err == nil {
			if old.System {
				return fmt.Errorf("内置工具不能由仓库覆盖")
			}
			h = catalog.Digest(CatalogToolPayload(old))
		}
		if h != expectedHash {
			return fmt.Errorf("本地工具已变化，请重新预览")
		}
		var t CatalogTool
		if err = json.Unmarshal(payload, &t); err != nil {
			return err
		}
		if h == "absent" {
			_, err = tx.ExecContext(ctx, `INSERT INTO tools(key,system,description,schema,agents,enabled,kind,exec,deferred) VALUES($1,false,$2,$3,'[]'::jsonb,false,$4,$5,$6)`, key, t.Description, t.Schema, t.Kind, t.Exec, t.Deferred)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE tools SET description=$2,schema=$3,kind=$4,exec=$5,deferred=$6 WHERE key=$1 AND NOT system`, key, t.Description, t.Schema, t.Kind, catalogMergeExec(t.Exec, old.Exec), t.Deferred)
		}
		if err != nil {
			return err
		}
	} else if kind == "agent" {
		old, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE key=$1 FOR UPDATE`, key))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		h := "absent"
		var id int64
		if err == nil {
			if old.Builtin {
				return fmt.Errorf("内置Agent不能由仓库覆盖")
			}
			id = old.ID
			var p string
			err = tx.QueryRowContext(ctx, `SELECT p.template_text FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, id).Scan(&p)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			h = catalog.Digest(CatalogAgentPayload(old, p))
		}
		if h != expectedHash {
			return fmt.Errorf("本地Agent已变化，请重新预览")
		}
		var a CatalogAgent
		if err = json.Unmarshal(payload, &a); err != nil {
			return err
		}
		if h == "absent" {
			err = tx.QueryRowContext(ctx, `INSERT INTO agents(key,name,description,role,builtin,enabled) VALUES($1,$2,$3,'assistant',false,false) RETURNING id`, key, a.Name, a.Description).Scan(&id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE agents SET name=$2,description=$3 WHERE id=$1`, id, a.Name, a.Description)
		}
		if err != nil {
			return err
		}
		var pid int64
		err = tx.QueryRowContext(ctx, `INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) SELECT $1,COALESCE(max(version),0)+1,$2,'在线目录更新','catalog' FROM agent_prompts WHERE agent_id=$1 RETURNING id`, id, a.Prompt).Scan(&pid)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agents SET current_prompt_id=$2 WHERE id=$1`, id, pid); err != nil {
			return err
		}
		for _, toolKey := range tools {
			res, e := tx.ExecContext(ctx, `UPDATE tools SET agents=(CASE WHEN jsonb_typeof(agents)='array' THEN agents ELSE '[]'::jsonb END) || to_jsonb($1::text) WHERE key=$2 AND NOT ((CASE WHEN jsonb_typeof(agents)='array' THEN agents ELSE '[]'::jsonb END) ? $1)`, key, toolKey)
			if e != nil {
				return e
			}
			_ = res
			var exists bool
			if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tools WHERE key=$1)`, toolKey).Scan(&exists); e != nil {
				return e
			}
			if !exists {
				return fmt.Errorf("依赖工具不存在：%s", toolKey)
			}
		}
		for _, skill := range skills {
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES($1,$2,true) ON CONFLICT(agent_id,skill_name) DO NOTHING`, id, skill); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("不支持的目录类型")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, recordKey, recordValue); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) CatalogSetSource(ctx context.Context, value string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741042)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('catalog.source',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, value); err != nil {
		return err
	}
	return tx.Commit()
}
