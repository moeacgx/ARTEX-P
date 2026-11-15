package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Server) registerMCPAccessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/mcp-access", s.mcpAccessOverview)
	mux.HandleFunc("POST /api/mcp-access/keys", s.mcpAccessCreateKey)
	mux.HandleFunc("POST /api/mcp-access/keys/{id}/revoke", s.mcpAccessRevokeKey)
	mux.HandleFunc("GET /api/mcp-access/calls", s.mcpAccessListCalls)
}

func (s *Server) mcpAccessOverview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rows, err := pg.QueryContext(r.Context(), `SELECT `+mcpKeyColumns+` FROM mcp_access_keys ORDER BY id DESC`)
	if err != nil {
		writeErr(w, 503, "读取密钥失败")
		return
	}
	defer rows.Close()
	keys := []mcpAccessKey{}
	for rows.Next() {
		k, err := scanMCPKey(rows)
		if err != nil {
			writeErr(w, 503, "读取密钥失败")
			return
		}
		keys = append(keys, k)
	}
	if rows.Err() != nil {
		writeErr(w, 503, "读取密钥失败")
		return
	}
	scopes := []string{}
	for scope := range localAgentScopes {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	tools := []map[string]any{}
	for _, t := range s.mcpAccessTools() {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "scope": t.Scope, "write": t.Method != "GET", "input_schema": t.schema()})
	}
	writeJSON(w, 200, map[string]any{"keys": keys, "scopes": scopes, "tools": tools, "endpoint": "/mcp"})
}

func (s *Server) mcpAccessCreateKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Days   int      `json:"days"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 80 || req.Days < 1 || req.Days > 365 {
		writeErr(w, 400, "名称必填且不超过 80 字；有效期为 1–365 天")
		return
	}
	if err := validateMCPAccessScopes(req.Scopes); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	token, hash, err := newMCPAccessToken()
	if err != nil {
		writeErr(w, 500, "生成密钥失败")
		return
	}
	k, err := scanMCPKey(pg.QueryRowContext(r.Context(), `INSERT INTO mcp_access_keys(name,token_hash,prefix,scopes,expires_at) VALUES($1,$2,$3,$4,$5) RETURNING `+mcpKeyColumns, req.Name, hash, token[:16], req.Scopes, time.Now().Add(time.Duration(req.Days)*24*time.Hour)))
	if err != nil {
		writeErr(w, 503, "保存密钥失败")
		return
	}
	writeJSON(w, 201, map[string]any{"key": k, "token": token})
}

func (s *Server) mcpAccessRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "密钥 ID 无效")
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	result, err := pg.ExecContext(r.Context(), `UPDATE mcp_access_keys SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, id)
	if err != nil {
		writeErr(w, 503, "撤销失败")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeErr(w, 404, "密钥不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) mcpAccessListCalls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		writeErr(w, 400, "页码过大")
		return
	}
	keyID := int64(0)
	if q.Get("key_id") != "" {
		var err error
		keyID, err = strconv.ParseInt(q.Get("key_id"), 10, 64)
		if err != nil || keyID < 1 {
			writeErr(w, 400, "密钥 ID 无效")
			return
		}
	}
	var from, until any
	for name, dst := range map[string]*any{"from": &from, "until": &until} {
		if q.Get(name) != "" {
			t, err := time.Parse(time.RFC3339, q.Get(name))
			if err != nil {
				writeErr(w, 400, "时间格式无效")
				return
			}
			*dst = t
		}
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	where := ` WHERE ($1::bigint=0 OR key_id=$1) AND ($2='' OR tool=$2) AND ($3='' OR status=$3) AND ($4::timestamptz IS NULL OR created_at >= $4) AND ($5::timestamptz IS NULL OR created_at <= $5)`
	args := []any{keyID, q.Get("tool"), q.Get("status"), from, until}
	var total int
	if err := pg.QueryRowContext(r.Context(), `SELECT count(*) FROM mcp_access_calls`+where, args...).Scan(&total); err != nil {
		writeErr(w, 503, "读取记录失败")
		return
	}
	args = append(args, (page-1)*50)
	rows, err := pg.QueryContext(r.Context(), `SELECT `+mcpCallColumns+` FROM mcp_access_calls`+where+` ORDER BY id DESC LIMIT 50 OFFSET $6`, args...)
	if err != nil {
		writeErr(w, 503, "读取记录失败")
		return
	}
	defer rows.Close()
	calls := []mcpAccessCall{}
	for rows.Next() {
		c, err := scanMCPCall(rows)
		if err != nil {
			writeErr(w, 503, "读取记录失败")
			return
		}
		calls = append(calls, c)
	}
	if rows.Err() != nil {
		writeErr(w, 503, "读取记录失败")
		return
	}
	writeJSON(w, 200, map[string]any{"calls": calls, "total": total, "page": page, "page_size": 50})
}
