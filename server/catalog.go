package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/catalog"
	"github.com/Autumn-27/artex/db"
)

var catalogMu sync.Mutex
var skillMutationMu sync.Mutex
var catalogClient = catalog.NewClient()

type catalogRecord struct {
	Source      catalog.Source  `json:"source"`
	Commit      string          `json:"commit"`
	Item        catalog.Item    `json:"item"`
	Hash        string          `json:"hash"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Backup      string          `json:"backup,omitempty"`
	Previous    *catalogRecord  `json:"previous,omitempty"`
	InstalledAt time.Time       `json:"installed_at"`
}

type catalogView struct {
	catalog.Item
	InstalledVersion string `json:"installed_version,omitempty"`
	Status           string `json:"status"`
	LocalHash        string `json:"local_hash"`
	CanRollback      bool   `json:"can_rollback"`
}

type catalogRequest struct {
	Kind               string `json:"kind"`
	ID                 string `json:"id"`
	Commit             string `json:"commit"`
	ExpectedLocalHash  string `json:"expected_local_hash"`
	ExpectedSourceHash string `json:"expected_source_hash"`
}

type catalogPreview struct {
	Item                catalog.Item      `json:"item"`
	Commit              string            `json:"commit"`
	SourceHash          string            `json:"source_hash"`
	LocalHash           string            `json:"local_hash"`
	Conflict            bool              `json:"conflict"`
	Changes             []string          `json:"changes"`
	AffectedAgents      []string          `json:"affected_agents"`
	MissingDependencies []string          `json:"missing_dependencies"`
	Contents            map[string]string `json:"contents"`
	LocalContents       map[string]string `json:"local_contents"`
}

func catalogSource(pg *db.DB) (catalog.Source, string, error) {
	v, ok, err := pg.GetSetting("catalog.source")
	if err != nil {
		return catalog.Source{}, "", err
	}
	s := catalog.DefaultSource()
	if ok {
		if err = json.Unmarshal([]byte(v), &s); err != nil {
			return s, "", fmt.Errorf("目录源配置无效")
		}
	}
	if err = s.Validate(); err != nil {
		return s, "", err
	}
	b, _ := json.Marshal(s)
	return s, string(b), nil
}

func catalogRecordKey(kind, id string) string { return "catalog.installed." + kind + "." + id }
func loadCatalogRecord(pg *db.DB, kind, id string) (*catalogRecord, error) {
	v, ok, err := pg.GetSetting(catalogRecordKey(kind, id))
	if err != nil || !ok {
		return nil, err
	}
	var rec catalogRecord
	if err = json.Unmarshal([]byte(v), &rec); err != nil {
		return nil, fmt.Errorf("安装记录损坏，请先检查备份")
	}
	return &rec, nil
}

func (s *Server) catalogSourceGet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	source, _, err := catalogSource(pg)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, source)
}

func (s *Server) catalogSourcePut(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var source catalog.Source
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := decode(r, &source); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	source.Repository = strings.TrimSpace(source.Repository)
	source.Ref = strings.TrimSpace(source.Ref)
	source.Directory = strings.TrimSpace(source.Directory)
	if err := source.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b, _ := json.Marshal(source)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if err := pg.CatalogSetSource(r.Context(), string(b)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, source)
}

func (s *Server) catalogLocal(pg *db.DB, kind, id string) (string, []byte, map[string][]byte, []string, error) {
	if kind == "skill" {
		dir := filepath.Join(s.skillDir, id)
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			return "absent", nil, nil, []string{}, nil
		} else if err != nil {
			return "", nil, nil, nil, err
		}
		files, err := catalog.ReadTree(dir)
		if err != nil {
			return "", nil, nil, nil, err
		}
		agents := []string{}
		ids, err := pg.SkillAgents(id)
		if err != nil {
			return "", nil, nil, nil, err
		}
		all, err := pg.ListAgents()
		if err != nil {
			return "", nil, nil, nil, err
		}
		for _, a := range all {
			for _, agentID := range ids {
				if a.ID == agentID {
					agents = append(agents, a.Key)
				}
			}
		}
		return catalog.FileHash(files), nil, files, agents, nil
	}
	if kind == "tool" {
		t, err := pg.GetTool(id)
		if err != nil {
			return "", nil, nil, nil, err
		}
		if t == nil {
			return "absent", nil, nil, []string{}, nil
		}
		if t.System {
			return "protected", nil, nil, t.Agents, nil
		}
		b := db.CatalogToolPayload(t)
		return catalog.Digest(b), b, map[string][]byte{"tool.json": b}, t.Agents, nil
	}
	if kind == "agent" {
		a, err := pg.GetAgentByKey(id)
		if err != nil {
			return "", nil, nil, nil, err
		}
		if a == nil {
			return "absent", nil, nil, []string{}, nil
		}
		if a.Builtin {
			return "protected", nil, nil, []string{id}, nil
		}
		p, err := pg.CurrentPrompt(a.ID)
		if err != nil {
			return "", nil, nil, nil, err
		}
		b := db.CatalogAgentPayload(a, p)
		return catalog.Digest(b), b, map[string][]byte{"agent.json": b}, []string{id}, nil
	}
	return "", nil, nil, nil, fmt.Errorf("无效的目录类型")
}

func (s *Server) catalogList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	pg := s.pg(w)
	if pg == nil {
		return
	}
	source, _, err := catalogSource(pg)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m, commit, err := catalogClient.Manifest(r.Context(), source, "")
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	kind := r.URL.Query().Get("kind")
	items := []catalogView{}
	for _, i := range m.Items {
		if kind != "" && i.Kind != kind {
			continue
		}
		rec, err := loadCatalogRecord(pg, i.Kind, i.ID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		h, _, _, _, localErr := s.catalogLocal(pg, i.Kind, i.ID)
		v := catalogView{Item: i, Status: "available", LocalHash: h}
		if localErr != nil {
			v.Status = "conflict"
		} else if h != "absent" {
			v.Status = "conflict"
			if rec != nil {
				v.InstalledVersion = rec.Item.Version
				v.CanRollback = rec.Previous != nil
				if rec.Hash == h && rec.Source == source {
					v.Status = "update"
					if rec.Item.Version == i.Version && sameCatalogFiles(rec.Item.Files, i.Files) {
						v.Status = "installed"
					}
				}
			}
		}
		items = append(items, v)
	}
	writeJSON(w, 200, map[string]any{"source": source, "commit": commit, "items": items})
}

func sameCatalogFiles(a, b map[string]string) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func strictCatalogJSON(b []byte, v any) error {
	if !json.Valid(b) {
		return fmt.Errorf("模板必须为单个有效JSON文档")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("模板字段无效：%w", err)
	}
	return nil
}

func validateCatalogPayload(i catalog.Item, files map[string][]byte) ([]byte, error) {
	if i.Kind == "skill" {
		if skillNameFromFrontmatter(files["SKILL.md"]) != i.ID {
			return nil, fmt.Errorf("SKILL.md name 必须与目录ID一致")
		}
		return nil, nil
	}
	if i.Kind == "tool" {
		var t db.CatalogTool
		if err := strictCatalogJSON(files["tool.json"], &t); err != nil {
			return nil, err
		}
		if t.Key != i.ID || !reToolKey.MatchString(t.Key) || (t.Kind != "command" && t.Kind != "script" && t.Kind != "http" && t.Kind != "shell") || strings.TrimSpace(t.Description) == "" {
			return nil, fmt.Errorf("工具模板key、描述或类型无效")
		}
		var schema, exec map[string]json.RawMessage
		if json.Unmarshal(t.Schema, &schema) != nil || schema == nil || json.Unmarshal(t.Exec, &exec) != nil || exec == nil {
			return nil, fmt.Errorf("工具schema和exec必须为JSON对象")
		}
		if t.Kind == "http" && !hasSchemaProps(t.Schema) {
			return nil, fmt.Errorf("HTTP工具必须提供参数schema")
		}
		b, _ := json.Marshal(t)
		return catalog.Canonical(b), nil
	}
	var a db.CatalogAgent
	if err := strictCatalogJSON(files["agent.json"], &a); err != nil {
		return nil, err
	}
	if a.Key != i.ID || !reAgentKey.MatchString(a.Key) || strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Prompt) == "" {
		return nil, fmt.Errorf("Agent模板key、名称或prompt无效")
	}
	b, _ := json.Marshal(a)
	return b, nil
}

func (s *Server) catalogPrepare(ctx context.Context, pg *db.DB, req catalogRequest) (catalog.Source, string, catalogPreview, map[string][]byte, []byte, *catalogRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var preview catalogPreview
	source, sourceJSON, err := catalogSource(pg)
	if err != nil {
		return source, "", preview, nil, nil, nil, err
	}
	m, commit, err := catalogClient.Manifest(ctx, source, req.Commit)
	if err != nil {
		return source, "", preview, nil, nil, nil, err
	}
	var item *catalog.Item
	for _, i := range m.Items {
		if i.Kind == req.Kind && i.ID == req.ID {
			v := i
			item = &v
			break
		}
	}
	if item == nil {
		return source, "", preview, nil, nil, nil, fmt.Errorf("目录中没有该条目")
	}
	files, err := catalogClient.Files(ctx, source, commit, *item)
	if err != nil {
		return source, "", preview, nil, nil, nil, err
	}
	payload, err := validateCatalogPayload(*item, files)
	if err != nil {
		return source, "", preview, nil, nil, nil, err
	}
	rec, err := loadCatalogRecord(pg, item.Kind, item.ID)
	if err != nil {
		return source, "", preview, nil, nil, nil, err
	}
	h, _, local, affected, err := s.catalogLocal(pg, item.Kind, item.ID)
	if err != nil {
		return source, "", preview, nil, nil, nil, err
	}
	preview = catalogPreview{Item: *item, Commit: commit, SourceHash: catalog.Digest([]byte(sourceJSON)), LocalHash: h, Changes: []string{}, AffectedAgents: affected, MissingDependencies: []string{}, Contents: map[string]string{}, LocalContents: map[string]string{}}
	preview.Conflict = h != "absent" && (rec == nil || rec.Hash != h || rec.Source != source)
	for _, f := range catalog.SortedFiles(files) {
		preview.Contents[f] = string(files[f])
		if old, ok := local[f]; !ok {
			preview.Changes = append(preview.Changes, "新增 "+f)
		} else if !bytes.Equal(old, files[f]) {
			preview.Changes = append(preview.Changes, "更新 "+f)
		}
	}
	for _, f := range catalog.SortedFiles(local) {
		preview.LocalContents[f] = string(local[f])
		if _, ok := files[f]; !ok {
			preview.Changes = append(preview.Changes, "删除 "+f)
		}
	}
	if item.Kind == "agent" {
		for _, key := range item.Tools {
			t, e := pg.GetTool(key)
			if e != nil {
				return source, "", preview, nil, nil, nil, e
			}
			if t == nil {
				preview.MissingDependencies = append(preview.MissingDependencies, "tool:"+key)
			}
		}
		for _, name := range item.Skills {
			info, e := os.Lstat(filepath.Join(s.skillDir, name, "SKILL.md"))
			if e != nil || !info.Mode().IsRegular() {
				preview.MissingDependencies = append(preview.MissingDependencies, "skill:"+name)
			}
		}
	}
	return source, sourceJSON, preview, files, payload, rec, nil
}

func decodeCatalogRequest(w http.ResponseWriter, r *http.Request) (catalogRequest, error) {
	var req catalogRequest
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	err := decode(r, &req)
	if err == nil && (req.Kind == "" || req.ID == "") {
		err = fmt.Errorf("缺少kind/id")
	}
	return req, err
}

func (s *Server) catalogPreview(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	req, err := decodeCatalogRequest(w, r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	_, _, p, _, _, _, err := s.catalogPrepare(r.Context(), pg, req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) catalogInstall(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	req, err := decodeCatalogRequest(w, r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Commit == "" || req.ExpectedLocalHash == "" || req.ExpectedSourceHash == "" {
		writeErr(w, 400, "请先预览，安装必须提供提交SHA、本地摘要和源摘要")
		return
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	skillMutationMu.Lock()
	defer skillMutationMu.Unlock()
	source, sourceJSON, p, files, payload, old, err := s.catalogPrepare(r.Context(), pg, req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.ExpectedSourceHash != p.SourceHash || req.ExpectedLocalHash != p.LocalHash || p.Conflict || len(p.MissingDependencies) > 0 {
		writeErr(w, 409, "源或本地内容已变化、存在本地修改或依赖缺失，请重新预览")
		return
	}
	rec := catalogRecord{Source: source, Commit: p.Commit, Item: p.Item, Payload: payload, InstalledAt: time.Now().UTC()}
	if old != nil && p.LocalHash != "absent" {
		copy := *old
		copy.Previous = nil
		rec.Previous = &copy
	}
	if p.Item.Kind == "skill" {
		rec.Hash = catalog.FileHash(files)
		err = catalog.ReplaceTree(filepath.Join(s.skillDir, p.Item.ID), files, func(backup string) error {
			rec.Backup = backup
			b, _ := json.Marshal(rec)
			return pg.SetSetting(catalogRecordKey(p.Item.Kind, p.Item.ID), string(b))
		})
	} else {
		if p.Item.Kind == "tool" {
			var t db.CatalogTool
			_ = json.Unmarshal(payload, &t)
			rec.Hash = catalog.Digest(db.CatalogToolPayload(&db.Tool{Key: t.Key, Description: t.Description, Kind: t.Kind, Schema: t.Schema, Exec: t.Exec, Deferred: t.Deferred}))
		} else {
			rec.Hash = catalog.Digest(payload)
		}
		b, _ := json.Marshal(rec)
		err = pg.CatalogApply(r.Context(), p.Item.Kind, p.Item.ID, p.LocalHash, payload, p.Item.Tools, p.Item.Skills, catalogRecordKey(p.Item.Kind, p.Item.ID), string(b), sourceJSON)
	}
	if err != nil {
		writeErr(w, 409, "安装失败，已保留或恢复旧版本："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "kind": p.Item.Kind, "id": p.Item.ID, "version": p.Item.Version, "local_hash": rec.Hash})
}

func (s *Server) catalogDownload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	req := catalogRequest{Kind: q.Get("kind"), ID: q.Get("id"), Commit: q.Get("commit")}
	source, _, err := catalogSource(pg)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m, commit, err := catalogClient.Manifest(r.Context(), source, req.Commit)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	var item *catalog.Item
	for _, i := range m.Items {
		if i.Kind == req.Kind && i.ID == req.ID {
			v := i
			item = &v
			break
		}
	}
	if item == nil {
		writeErr(w, 404, "条目不存在")
		return
	}
	files, err := catalogClient.Files(r.Context(), source, commit, *item)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range catalog.SortedFiles(files) {
		out, e := zw.Create(item.ID + "/" + f)
		if e == nil {
			_, e = out.Write(files[f])
		}
		if e != nil {
			writeErr(w, 500, e.Error())
			return
		}
	}
	if err = zw.Close(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, item.Kind, item.ID))
	w.Header().Set("X-Catalog-Commit", commit)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) catalogRollback(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	req, err := decodeCatalogRequest(w, r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	skillMutationMu.Lock()
	defer skillMutationMu.Unlock()
	rec, err := loadCatalogRecord(pg, req.Kind, req.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rec == nil || rec.Previous == nil {
		writeErr(w, 409, "没有可回滚的旧版本")
		return
	}
	h, _, _, _, err := s.catalogLocal(pg, req.Kind, req.ID)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	if h != req.ExpectedLocalHash || h != rec.Hash {
		writeErr(w, 409, "本地内容已修改，拒绝覆盖")
		return
	}
	prev := *rec.Previous
	prev.Previous = nil
	prev.InstalledAt = time.Now().UTC()
	if req.Kind == "skill" {
		if rec.Backup == "" || filepath.Dir(rec.Backup) != filepath.Join(s.skillDir, ".catalog-state") || !strings.HasPrefix(filepath.Base(rec.Backup), "backup-") {
			writeErr(w, 409, "技能备份路径无效")
			return
		}
		files, e := catalog.ReadTree(rec.Backup)
		if e != nil || catalog.FileHash(files) != prev.Hash {
			writeErr(w, 409, "技能备份缺失或摘要不匹配")
			return
		}
		err = catalog.ReplaceTree(filepath.Join(s.skillDir, req.ID), files, func(backup string) error {
			prev.Backup = backup
			b, _ := json.Marshal(prev)
			return pg.SetSetting(catalogRecordKey(req.Kind, req.ID), string(b))
		})
	} else {
		_, sourceJSON, e := catalogSource(pg)
		if e != nil {
			writeErr(w, 500, e.Error())
			return
		}
		b, _ := json.Marshal(prev)
		err = pg.CatalogApply(r.Context(), req.Kind, req.ID, h, prev.Payload, nil, nil, catalogRecordKey(req.Kind, req.ID), string(b), sourceJSON)
	}
	if err != nil {
		writeErr(w, 409, "回滚失败，当前版本已保留："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "version": prev.Item.Version})
}

// replaceSkillDirectory 保留旧目录直到新目录安装成功，避免上传覆盖时丢失原技能。
func replaceSkillDirectory(stage, target string, overwrite bool) error {
	backup := ""
	if info, err := os.Lstat(target); err == nil {
		if !overwrite {
			return fmt.Errorf("技能已存在")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("技能目标不是普通目录")
		}
		var e error
		stateDir, e := catalog.StateDir(filepath.Dir(target))
		if e != nil {
			return e
		}
		backup, e = os.MkdirTemp(stateDir, "backup-")
		if e != nil {
			return e
		}
		if e = os.Remove(backup); e != nil {
			return e
		}
		if e = os.Rename(target, backup); e != nil {
			return e
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		if backup != "" {
			if e := os.Rename(backup, target); e != nil {
				return fmt.Errorf("安装失败：%v；恢复失败：%w", err, e)
			}
		}
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func newSkillUploadStage(skillDir string) (string, error) {
	dir, err := catalog.StateDir(skillDir)
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(dir, "upload-")
}
