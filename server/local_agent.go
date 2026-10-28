package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	localagent "github.com/Autumn-27/artex/integrations/local-agent"
)

const localAgentBodyLimit = maxSkillZipBytes + 64*1024

var localAgentScopes = map[string]bool{
	"tasks.read": true, "tasks.create": true, "tasks.control": true, "tasks.message": true,
	"skills.read": true, "skills.upload": true, "skills.write": true,
	"assets.read": true, "assets.write": true, "assets.delete": true,
	"workspace.read": true, "workspace.write": true, "workspace.upload": true, "workspace.download": true, "workspace.delete": true,
	"conversations.read": true, "conversations.create": true, "conversations.message": true, "conversations.stop": true,
	"discovery.read": true, "logs.read": true,
	"llm.read": true, "llm.write": true, "llm.test": true,
	"tools.read": true, "tools.write": true,
	"mcp.read": true, "mcp.write": true,
}

type localAgentDevice struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	PublicKey       string     `json:"public_key"`
	Fingerprint     string     `json:"fingerprint"`
	Status          string     `json:"status"`
	Scopes          []string   `json:"scopes"`
	CreatedAt       time.Time  `json:"created_at"`
	ApprovedAt      *time.Time `json:"approved_at,omitempty"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	SkillVersion    string     `json:"skill_version"`
	SkillReportedAt *time.Time `json:"skill_reported_at,omitempty"`
}

func (s *Server) registerLocalAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/local-agent/skill", s.localAgentDownload)
	mux.HandleFunc("POST /api/local-agent/pair", s.localAgentPair)
	mux.HandleFunc("GET /api/local-agent/devices", s.localAgentListDevices)
	mux.HandleFunc("POST /api/local-agent/devices/{id}/approve", s.localAgentApprove)
	mux.HandleFunc("POST /api/local-agent/devices/{id}/revoke", s.localAgentRevoke)
	mux.HandleFunc("GET /api/local-agent/status", s.localAgentWithScope("", s.localAgentStatus))
	mux.HandleFunc("GET /api/local-agent/tasks", s.localAgentWithScope("tasks.read", s.listTasks))
	mux.HandleFunc("GET /api/local-agent/tasks/{id}", s.localAgentWithScope("tasks.read", s.getTask))
	mux.HandleFunc("POST /api/local-agent/tasks", s.localAgentWithScope("tasks.create", s.createTask))
	mux.HandleFunc("GET /api/local-agent/skills", s.localAgentWithScope("skills.read", s.fsListSkills))
	mux.HandleFunc("POST /api/local-agent/skills/upload", s.localAgentWithScope("skills.upload", s.fsUploadSkill))
	mux.HandleFunc("GET /api/local-agent/skills/{name}/download", s.localAgentWithScope("skills.read", s.fsDownloadSkill))
	mux.HandleFunc("DELETE /api/local-agent/skills/{name}", s.localAgentWithScope("skills.write", s.fsDeleteSkill))
	mux.HandleFunc("GET /api/local-agent/skills/{name}/files", s.localAgentWithScope("skills.read", s.fsListFiles))
	mux.HandleFunc("GET /api/local-agent/skills/{name}/files/{file...}", s.localAgentWithScope("skills.read", s.fsReadFile))
	mux.HandleFunc("PUT /api/local-agent/skills/{name}/files/{file...}", s.localAgentWithScope("skills.write", s.fsWriteFile))
	mux.HandleFunc("DELETE /api/local-agent/skills/{name}/files/{file...}", s.localAgentWithScope("skills.write", s.fsDeletePath))

	// 任务及主 Agent 对话。路径变量与原 API 保持一致，handler 可直接复用。
	mux.HandleFunc("POST /api/local-agent/tasks/{id}/control", s.localAgentWithScope("tasks.control", s.control))
	mux.HandleFunc("POST /api/local-agent/tasks/{id}/intents/{iid}/messages", s.localAgentWithScope("tasks.message", s.sendWorkerMessage))
	mux.HandleFunc("POST /api/local-agent/tasks/{id}/chat", s.localAgentWithScope("tasks.message", s.localAgentTaskChat))
	mux.HandleFunc("POST /api/local-agent/tasks/{id}/chat/session", s.localAgentWithScope("tasks.message", s.localAgentNewTaskChatSession))
	mux.HandleFunc("POST /api/local-agent/chat/upload", s.localAgentWithScopeBody("tasks.message", maxWorkspaceUpload, s.chatUpload))
	mux.HandleFunc("GET /api/local-agent/tasks/{id}/chat/status", s.localAgentWithScope("tasks.read", s.taskChatStatus))
	mux.HandleFunc("POST /api/local-agent/tasks/{id}/chat/stop", s.localAgentWithScope("tasks.message", s.stopChat))
	mux.HandleFunc("GET /api/local-agent/tasks/{id}/llm/resolution", s.localAgentWithScope("llm.read", s.taskLLMResolutionHandler))
	mux.HandleFunc("PUT /api/local-agent/tasks/{id}/llm", s.localAgentWithScope("llm.write", s.updateTaskLLMProfiles))
	mux.HandleFunc("GET /api/local-agent/tasks/{id}/coverage", s.localAgentWithScope("discovery.read", s.taskCoverage))
	mux.HandleFunc("GET /api/local-agent/tasks/{id}/coverage-graph", s.localAgentWithScope("discovery.read", s.taskCoverageGraph))
	mux.HandleFunc("GET /api/local-agent/tasks/{id}/asset-refs", s.localAgentWithScope("discovery.read", s.taskAssetRefs))

	// 资产。
	mux.HandleFunc("GET /api/local-agent/assets", s.localAgentWithScope("assets.read", s.listAssets))
	mux.HandleFunc("GET /api/local-agent/assets/counts", s.localAgentWithScope("assets.read", s.assetCounts))
	mux.HandleFunc("POST /api/local-agent/assets", s.localAgentWithScope("assets.write", s.insertAssets))
	mux.HandleFunc("DELETE /api/local-agent/assets", s.localAgentWithScope("assets.delete", s.deleteAssets))

	// 工作空间。上传单独使用 512 MiB 的签名读取上限。
	mux.HandleFunc("GET /api/local-agent/workspace/list", s.localAgentWithScope("workspace.read", s.wsList))
	mux.HandleFunc("GET /api/local-agent/workspace/read", s.localAgentWithScope("workspace.read", s.wsRead))
	mux.HandleFunc("POST /api/local-agent/workspace/write", s.localAgentWithScope("workspace.write", s.wsWrite))
	mux.HandleFunc("POST /api/local-agent/workspace/mkdir", s.localAgentWithScope("workspace.write", s.wsMkdir))
	mux.HandleFunc("DELETE /api/local-agent/workspace/delete", s.localAgentWithScope("workspace.delete", s.wsDelete))
	mux.HandleFunc("GET /api/local-agent/workspace/download", s.localAgentWithScope("workspace.download", s.wsDownload))
	mux.HandleFunc("POST /api/local-agent/workspace/upload", s.localAgentWithScopeBody("workspace.upload", maxWorkspaceUpload, s.wsUpload))

	// 对话：创建、读取消息、发送和停止分别授权。
	mux.HandleFunc("GET /api/local-agent/conversations", s.localAgentWithScope("conversations.read", s.pgListConversations))
	mux.HandleFunc("POST /api/local-agent/conversations", s.localAgentWithScope("conversations.create", s.pgCreateConversation))
	mux.HandleFunc("GET /api/local-agent/conversations/{id}/messages", s.localAgentWithScope("conversations.read", s.pgConversationMessages))
	mux.HandleFunc("GET /api/local-agent/conversations/{id}/messages/{seq}", s.localAgentWithScope("conversations.read", s.pgConversationMsgDetail))
	mux.HandleFunc("POST /api/local-agent/conversations/{id}/messages", s.localAgentWithScope("conversations.message", s.pgSendConversationMessage))
	mux.HandleFunc("POST /api/local-agent/conversations/{id}/stop", s.localAgentWithScope("conversations.stop", s.pgStopConversation))
	mux.HandleFunc("PATCH /api/local-agent/conversations/{id}", s.localAgentWithScope("conversations.message", s.pgRenameConversation))
	mux.HandleFunc("PATCH /api/local-agent/conversations/{id}/profile", s.localAgentWithScope("llm.write", s.pgUpdateConversation))

	// 发现页只读数据。
	mux.HandleFunc("GET /api/local-agent/discovery/frontier", s.localAgentWithScope("discovery.read", s.frontier))
	mux.HandleFunc("GET /api/local-agent/discovery/findings", s.localAgentWithScope("discovery.read", s.findings))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/groups", s.localAgentWithScope("discovery.read", s.findingGroups))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/asset-tree", s.localAgentWithScope("discovery.read", s.findingAssetTree))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/stats", s.localAgentWithScope("discovery.read", s.findingStats))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/{id}", s.localAgentWithScope("discovery.read", s.getFinding))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/{id}/lineage", s.localAgentWithScope("discovery.read", s.findingLineage))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/{id}/retests", s.localAgentWithScope("discovery.read", s.listFindingRetests))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/retests/active", s.localAgentWithScope("discovery.read", s.listActiveFindingRetests))
	mux.HandleFunc("GET /api/local-agent/discovery/findings/export", s.localAgentWithScope("discovery.read", s.findingsExport))
	mux.HandleFunc("GET /api/local-agent/discovery/graph", s.localAgentWithScope("discovery.read", s.explorationGraph))
	mux.HandleFunc("GET /api/local-agent/discovery/nodes", s.localAgentWithScope("discovery.read", s.explorationNodes))
	mux.HandleFunc("GET /api/local-agent/discovery/activity", s.localAgentWithScope("discovery.read", s.activity))
	mux.HandleFunc("GET /api/local-agent/discovery/activity/history", s.localAgentWithScope("discovery.read", s.activityHistory))
	mux.HandleFunc("GET /api/local-agent/discovery/activity/stream", s.localAgentWithScope("discovery.read", s.streamActivity))
	mux.HandleFunc("GET /api/local-agent/discovery/activity/{seq}", s.localAgentWithScope("discovery.read", s.activityDetail))
	mux.HandleFunc("GET /api/local-agent/discovery/intents", s.localAgentWithScope("discovery.read", s.intents))
	mux.HandleFunc("GET /api/local-agent/discovery/main-sessions", s.localAgentWithScope("discovery.read", s.mainSessions))
	mux.HandleFunc("GET /api/local-agent/discovery/tokens", s.localAgentWithScope("discovery.read", s.tokenStats))
	mux.HandleFunc("GET /api/local-agent/discovery/stats", s.localAgentWithScope("discovery.read", s.findingStats))

	// 运行日志及系统统计。
	mux.HandleFunc("GET /api/local-agent/logs", s.localAgentWithScope("logs.read", s.getLogs))
	mux.HandleFunc("GET /api/local-agent/logs/history", s.localAgentWithScope("logs.read", s.getLogsHistory))
	mux.HandleFunc("GET /api/local-agent/logs/stream", s.localAgentWithScope("logs.read", s.streamLogs))

	// LLM 配置和渠道；读取 handler 已经确保不会返回 API key。
	mux.HandleFunc("GET /api/local-agent/llm", s.localAgentWithScope("llm.read", s.getLLM))
	mux.HandleFunc("POST /api/local-agent/llm", s.localAgentWithScope("llm.write", s.setLLM))
	mux.HandleFunc("POST /api/local-agent/llm/test", s.localAgentWithScope("llm.test", s.testLLM))
	mux.HandleFunc("GET /api/local-agent/llm/profiles", s.localAgentWithScope("llm.read", s.pgListProfiles))
	mux.HandleFunc("POST /api/local-agent/llm/profiles", s.localAgentWithScope("llm.write", s.pgSaveProfile))
	mux.HandleFunc("DELETE /api/local-agent/llm/profiles/{id}", s.localAgentWithScope("llm.write", s.pgDeleteProfile))
	mux.HandleFunc("POST /api/local-agent/llm/profiles/active", s.localAgentWithScope("llm.write", s.pgActivateProfile))
	mux.HandleFunc("GET /api/local-agent/llm/retry-policy", s.localAgentWithScope("llm.read", s.pgGetLLMRetryPolicy))
	mux.HandleFunc("POST /api/local-agent/llm/retry-policy", s.localAgentWithScope("llm.write", s.pgSaveLLMRetryPolicy))
	mux.HandleFunc("GET /api/local-agent/llm/pool", s.localAgentWithScope("llm.read", s.pgLLMPoolStatus))
	mux.HandleFunc("POST /api/local-agent/llm/pool/reset", s.localAgentWithScope("llm.write", s.pgLLMPoolReset))
	mux.HandleFunc("GET /api/local-agent/llm/profiles/{id}/models", s.localAgentWithScope("llm.read", s.pgGetLLMModels))
	mux.HandleFunc("PUT /api/local-agent/llm/profiles/{id}/models", s.localAgentWithScope("llm.write", s.pgPutLLMModels))
	mux.HandleFunc("GET /api/local-agent/llm/pool/routes", s.localAgentWithScope("llm.read", s.pgGetLLMPoolModelRoutes))
	mux.HandleFunc("PUT /api/local-agent/llm/pool/routes/{id}", s.localAgentWithScope("llm.write", s.pgPutLLMPoolModelRoute))
	mux.HandleFunc("GET /api/local-agent/llm/records", s.localAgentWithScope("llm.read", s.pgListLLMRecords))
	mux.HandleFunc("GET /api/local-agent/llm/records/{id}", s.localAgentWithScope("llm.read", s.pgGetLLMRecord))
	mux.HandleFunc("GET /api/local-agent/llm/records/tasks", s.localAgentWithScope("llm.read", s.pgLLMTasks))
	mux.HandleFunc("GET /api/local-agent/llm/records/by-model", s.localAgentWithScope("llm.read", s.pgTokenByModel))

	// 工具目录及自定义工具。
	mux.HandleFunc("GET /api/local-agent/tools", s.localAgentWithScope("tools.read", s.pgListTools))
	mux.HandleFunc("PUT /api/local-agent/tools/{key}", s.localAgentWithScope("tools.write", s.pgUpdateTool))
	mux.HandleFunc("POST /api/local-agent/tools/custom", s.localAgentWithScope("tools.write", s.pgCreateCustomTool))
	mux.HandleFunc("POST /api/local-agent/tools/custom/test", s.localAgentWithScope("tools.write", s.pgTestCustomTool))
	mux.HandleFunc("PUT /api/local-agent/tools/custom/{key}", s.localAgentWithScope("tools.write", s.pgUpdateCustomTool))
	mux.HandleFunc("DELETE /api/local-agent/tools/custom/{key}", s.localAgentWithScope("tools.write", s.pgDeleteCustomTool))
	mux.HandleFunc("POST /api/local-agent/tools/{key}/reset", s.localAgentWithScope("tools.write", s.pgResetTool))

	// MCP 服务器及其工具目录。
	mux.HandleFunc("GET /api/local-agent/mcp", s.localAgentWithScope("mcp.read", s.pgListMCP))
	mux.HandleFunc("POST /api/local-agent/mcp", s.localAgentWithScope("mcp.write", s.pgSaveMCP))
	mux.HandleFunc("DELETE /api/local-agent/mcp/{id}", s.localAgentWithScope("mcp.write", s.pgDeleteMCP))
	mux.HandleFunc("GET /api/local-agent/mcp/{id}/tools", s.localAgentWithScope("mcp.read", s.pgMCPTools))
	mux.HandleFunc("POST /api/local-agent/mcp/{id}/refresh", s.localAgentWithScope("mcp.write", s.pgRefreshMCP))
}

func (s *Server) localAgentTaskChat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("task", r.PathValue("id"))
	r.URL.RawQuery = q.Encode()
	s.chat(w, r)
}

func (s *Server) localAgentNewTaskChatSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("task", r.PathValue("id"))
	r.URL.RawQuery = q.Encode()
	s.newMainSession(w, r)
}

func (s *Server) localAgentDownload(w http.ResponseWriter, r *http.Request) {
	archive, err := localagent.Archive()
	if err != nil {
		writeErr(w, 500, "生成本地 Agent Skill 包失败")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="artex-agent.zip"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "artex-agent.zip", time.Time{}, bytes.NewReader(archive))
}

// 签名覆盖方法、完整 RequestURI、时间、一次性 nonce 和请求体摘要。
func localAgentVerifySignature(r *http.Request, publicKey string, maxBody int64) error {
	_, err := localAgentVerifySignatureWithCleanup(r, publicKey, maxBody)
	return err
}

func localAgentVerifySignatureWithCleanup(r *http.Request, publicKey string, maxBody int64) (func(), error) {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("设备公钥无效")
	}
	timestamp := r.Header.Get("X-ARTEX-Timestamp")
	when, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(when, 0)) > 5*time.Minute || time.Until(time.Unix(when, 0)) > 5*time.Minute {
		return nil, errors.New("签名时间无效")
	}
	nonce := r.Header.Get("X-ARTEX-Nonce")
	if len(nonce) < 16 || len(nonce) > 128 {
		return nil, errors.New("nonce 长度无效")
	}
	for _, c := range nonce {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return nil, errors.New("nonce 字符无效")
		}
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-ARTEX-Signature"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("签名格式无效")
	}
	var digest [sha256.Size]byte
	var cleanup func()
	if maxBody > 32<<20 {
		file, err := os.CreateTemp("", "artex-local-agent-body-*")
		if err != nil {
			return nil, errors.New("无法暂存签名请求体")
		}
		cleanup = func() { _ = file.Close(); _ = os.Remove(file.Name()) }
		h := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, h), io.LimitReader(r.Body, maxBody+1))
		if copyErr != nil || written > maxBody {
			cleanup()
			return nil, errors.New("请求体过大或不可读取")
		}
		copy(digest[:], h.Sum(nil))
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			cleanup()
			return nil, errors.New("无法重放签名请求体")
		}
		r.Body = file
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || int64(len(body)) > maxBody {
			return nil, errors.New("请求体过大或不可读取")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		digest = sha256.Sum256(body)
	}
	message := r.Method + "\n" + r.URL.RequestURI() + "\n" + timestamp + "\n" + nonce + "\n" + hex.EncodeToString(digest[:])
	if !ed25519.Verify(ed25519.PublicKey(key), []byte(message), sig) {
		if cleanup != nil {
			cleanup()
		}
		return nil, errors.New("设备签名无效")
	}
	return cleanup, nil
}

func localAgentDeviceRow(row *sql.Row) (d localAgentDevice, err error) {
	var scopes []byte
	err = row.Scan(&d.ID, &d.Name, &d.PublicKey, &d.Status, &scopes, &d.CreatedAt, &d.ApprovedAt, &d.LastUsedAt, &d.SkillVersion, &d.SkillReportedAt)
	if err == nil {
		err = json.Unmarshal(scopes, &d.Scopes)
		d.Fingerprint = localAgentFingerprint(d.PublicKey)
	}
	return
}

const localAgentSelect = `SELECT id,name,public_key,status,to_json(scopes),created_at,approved_at,last_used_at,skill_version,skill_reported_at FROM local_agent_devices`

func localAgentFingerprint(publicKey string) string {
	key, _ := base64.StdEncoding.DecodeString(publicKey)
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:])
}

func (s *Server) localAgentPair(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "配对请求格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 80 || len(req.Name) > 240 {
		writeErr(w, 400, "设备名称无效")
		return
	}
	// 配对时证明客户端持有对应私钥；审批前不得授予业务权限。
	key, err := base64.StdEncoding.DecodeString(req.PublicKey)
	proof, proofErr := base64.StdEncoding.DecodeString(r.Header.Get("X-ARTEX-Pair-Proof"))
	if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != req.PublicKey || proofErr != nil ||
		!ed25519.Verify(ed25519.PublicKey(key), []byte("ARTEX local-agent pair v1\n"+req.Name+"\n"+req.PublicKey), proof) {
		writeErr(w, 401, "设备公钥证明无效")
		return
	}
	d, err := localAgentDeviceRow(pg.QueryRow(`INSERT INTO local_agent_devices(name,public_key) VALUES($1,$2)
		ON CONFLICT (public_key) DO UPDATE SET name=CASE WHEN local_agent_devices.status='pending' THEN EXCLUDED.name ELSE local_agent_devices.name END
		RETURNING id,name,public_key,status,to_json(scopes),created_at,approved_at,last_used_at,skill_version,skill_reported_at`, req.Name, req.PublicKey))
	if err != nil {
		writeErr(w, 503, "保存配对申请失败")
		return
	}
	writeJSON(w, 200, map[string]any{"id": d.ID, "status": d.Status})
}

func (s *Server) localAgentListDevices(w http.ResponseWriter, r *http.Request) {
	skill, err := localagent.Metadata()
	if err != nil {
		writeErr(w, 500, "读取本地 Agent Skill 版本失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rows, err := pg.Query(localAgentSelect + ` ORDER BY id DESC LIMIT 500`)
	if err != nil {
		writeErr(w, 503, "读取设备失败")
		return
	}
	defer rows.Close()
	devices := []localAgentDevice{}
	for rows.Next() {
		var d localAgentDevice
		var scopes []byte
		if err := rows.Scan(&d.ID, &d.Name, &d.PublicKey, &d.Status, &scopes, &d.CreatedAt, &d.ApprovedAt, &d.LastUsedAt, &d.SkillVersion, &d.SkillReportedAt); err != nil {
			writeErr(w, 503, "读取设备失败")
			return
		}
		if err := json.Unmarshal(scopes, &d.Scopes); err != nil {
			writeErr(w, 503, "读取设备权限失败")
			return
		}
		d.Fingerprint = localAgentFingerprint(d.PublicKey)
		devices = append(devices, d)
	}
	if rows.Err() != nil {
		writeErr(w, 503, "读取设备失败")
		return
	}
	writeJSON(w, 200, map[string]any{"devices": devices, "skill": skill})
}

func (s *Server) localAgentApprove(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "设备 id 无效")
		return
	}
	var req struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, 400, "授权请求格式错误")
		return
	}
	if len(req.Scopes) == 0 || len(req.Scopes) > len(localAgentScopes) {
		writeErr(w, 400, "请指定有效权限")
		return
	}
	seen := map[string]bool{}
	for _, scope := range req.Scopes {
		if !localAgentScopes[scope] || seen[scope] {
			writeErr(w, 400, "权限无效或重复")
			return
		}
		seen[scope] = true
	}
	res, err := pg.Exec(`UPDATE local_agent_devices SET status='approved', scopes=$2, approved_at=now()
		WHERE id=$1 AND status IN ('pending','approved')`, id, req.Scopes)
	if err != nil {
		writeErr(w, 503, "授权失败")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, 404, "待审批设备不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) localAgentRevoke(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "设备 id 无效")
		return
	}
	res, err := pg.Exec(`UPDATE local_agent_devices SET status='revoked',scopes='{}' WHERE id=$1`, id)
	if err != nil {
		writeErr(w, 503, "撤销失败")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, 404, "设备不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) localAgentWithScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return s.localAgentWithScopeBody(scope, localAgentBodyLimit, next)
}

func (s *Server) localAgentWithScopeBody(scope string, maxBody int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pg := s.pg(w)
		if pg == nil {
			return
		}
		id, err := strconv.ParseInt(r.Header.Get("X-ARTEX-Device-ID"), 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, 401, "设备身份无效")
			return
		}
		d, err := localAgentDeviceRow(pg.QueryRow(localAgentSelect+` WHERE id=$1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 401, "设备不存在")
			return
		}
		if err != nil {
			writeErr(w, 503, "读取设备失败")
			return
		}
		cleanupBody, err := localAgentVerifySignatureWithCleanup(r, d.PublicKey, maxBody)
		if err != nil {
			writeErr(w, 401, err.Error())
			return
		}
		if cleanupBody != nil {
			defer cleanupBody()
		}
		if scope != "" && d.Status != "approved" {
			writeErr(w, 403, "设备未获授权或已撤销")
			return
		}
		if scope != "" {
			allowed := false
			for _, granted := range d.Scopes {
				if granted == scope {
					allowed = true
					break
				}
			}
			if !allowed {
				writeErr(w, 403, "设备缺少权限："+scope)
				return
			}
		}
		// 仅清理当前设备的过期 nonce，不扫描所有设备。
		_, _ = pg.Exec(`DELETE FROM local_agent_nonces WHERE public_key=$1 AND created_at < now()-interval '10 minutes'`, d.PublicKey)
		res, err := pg.Exec(`INSERT INTO local_agent_nonces(public_key,nonce) VALUES($1,$2) ON CONFLICT DO NOTHING`, d.PublicKey, r.Header.Get("X-ARTEX-Nonce"))
		if err != nil {
			writeErr(w, 503, "签名重放检查失败")
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			writeErr(w, 409, "签名 nonce 已使用")
			return
		}
		// 版本来自已经验证且未重放的签名请求，不接受未签名的展示头。
		version, err := localAgentConsumeSkillVersion(r)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// 旧客户端再次使用同一身份时清为未知，避免沿用其他安装上报的版本。
		if _, err := pg.Exec(`UPDATE local_agent_devices SET last_used_at=now(),skill_version=$2,
			skill_reported_at=CASE WHEN $2='' THEN NULL ELSE now() END WHERE id=$1`, id, version); err != nil {
			writeErr(w, 503, "保存设备技能版本失败，未执行操作")
			return
		}
		if scope == "" {
			s.localAgentStatus(w, r)
			return
		}
		// 写操作先持久化受理记录；不记录任务内容、Skill 正文或认证材料。
		if r.Method != http.MethodGet {
			var auditID int64
			if err := pg.QueryRow(`INSERT INTO local_agent_audit(device_id,method,path) VALUES($1,$2,$3) RETURNING id`, id, r.Method, r.URL.Path).Scan(&auditID); err != nil {
				writeErr(w, 503, "记录设备操作失败，未执行")
				return
			}
			recorder := &localAgentResponse{ResponseWriter: w, status: 200}
			next(recorder, r)
			// 只有处理器正常返回才登记完成；异常退出保留待核对记录。
			if _, err := pg.Exec(`UPDATE local_agent_audit SET status_code=$2,completed_at=now() WHERE id=$1`, auditID, recorder.status); err != nil {
				log.Printf("[local-agent] 操作审计结果写入失败，记录 #%d", auditID)
			}
			return
		}
		next(w, r)
	}
}

// 验签后才移除内部参数，防止影响复用的业务处理器和转发请求。
func localAgentConsumeSkillVersion(r *http.Request) (string, error) {
	query := r.URL.Query()
	values, present := query["_skill_version"]
	if !present {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) != sha256.Size*2 {
		return "", errors.New("技能版本格式无效")
	}
	version := values[0]
	if _, err := hex.DecodeString(version); err != nil || version != strings.ToLower(version) {
		return "", errors.New("技能版本格式无效")
	}
	query.Del("_skill_version")
	r.URL.RawQuery = query.Encode()
	return version, nil
}

func (s *Server) localAgentStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := strconv.ParseInt(r.Header.Get("X-ARTEX-Device-ID"), 10, 64)
	d, err := localAgentDeviceRow(pg.QueryRow(localAgentSelect+` WHERE id=$1`, id))
	if err != nil {
		writeErr(w, 503, "读取设备失败")
		return
	}
	skill, err := localagent.Metadata()
	if err != nil {
		writeErr(w, 500, "读取本地 Agent Skill 版本失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"id": d.ID, "name": d.Name, "status": d.Status, "scopes": d.Scopes,
		"skill": skill, "skill_version": d.SkillVersion, "skill_reported_at": d.SkillReportedAt,
		"skill_update_available": d.SkillVersion != "" && d.SkillVersion != skill.Version})
}

type localAgentResponse struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *localAgentResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status, w.wroteHeader = status, true
	w.ResponseWriter.WriteHeader(status)
}

func (w *localAgentResponse) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// Flush 保留 SSE handler 所需的 http.Flusher 能力，同时让审计包装器记录
// 首次写出的响应状态。
func (w *localAgentResponse) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
