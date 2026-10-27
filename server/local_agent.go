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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	localagent "github.com/Autumn-27/artex/integrations/local-agent"
)

const localAgentBodyLimit = maxSkillZipBytes + 64*1024

var localAgentScopes = map[string]bool{
	"tasks.read": true, "tasks.create": true,
	"skills.read": true, "skills.upload": true,
}

type localAgentDevice struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	PublicKey   string     `json:"public_key"`
	Fingerprint string     `json:"fingerprint"`
	Status      string     `json:"status"`
	Scopes      []string   `json:"scopes"`
	CreatedAt   time.Time  `json:"created_at"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
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
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("设备公钥无效")
	}
	timestamp := r.Header.Get("X-ARTEX-Timestamp")
	when, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(when, 0)) > 5*time.Minute || time.Until(time.Unix(when, 0)) > 5*time.Minute {
		return errors.New("签名时间无效")
	}
	nonce := r.Header.Get("X-ARTEX-Nonce")
	if len(nonce) < 16 || len(nonce) > 128 {
		return errors.New("nonce 长度无效")
	}
	for _, c := range nonce {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return errors.New("nonce 字符无效")
		}
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-ARTEX-Signature"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("签名格式无效")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return errors.New("请求体过大或不可读取")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	digest := sha256.Sum256(body)
	message := r.Method + "\n" + r.URL.RequestURI() + "\n" + timestamp + "\n" + nonce + "\n" + hex.EncodeToString(digest[:])
	if !ed25519.Verify(ed25519.PublicKey(key), []byte(message), sig) {
		return errors.New("设备签名无效")
	}
	return nil
}

func localAgentDeviceRow(row *sql.Row) (d localAgentDevice, err error) {
	var scopes []byte
	err = row.Scan(&d.ID, &d.Name, &d.PublicKey, &d.Status, &scopes, &d.CreatedAt, &d.ApprovedAt, &d.LastUsedAt)
	if err == nil {
		err = json.Unmarshal(scopes, &d.Scopes)
		d.Fingerprint = localAgentFingerprint(d.PublicKey)
	}
	return
}

const localAgentSelect = `SELECT id,name,public_key,status,to_json(scopes),created_at,approved_at,last_used_at FROM local_agent_devices`

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
		RETURNING id,name,public_key,status,to_json(scopes),created_at,approved_at,last_used_at`, req.Name, req.PublicKey))
	if err != nil {
		writeErr(w, 503, "保存配对申请失败")
		return
	}
	writeJSON(w, 200, map[string]any{"id": d.ID, "status": d.Status})
}

func (s *Server) localAgentListDevices(w http.ResponseWriter, r *http.Request) {
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
		if err := rows.Scan(&d.ID, &d.Name, &d.PublicKey, &d.Status, &scopes, &d.CreatedAt, &d.ApprovedAt, &d.LastUsedAt); err != nil {
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
	writeJSON(w, 200, map[string]any{"devices": devices})
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
		if err := localAgentVerifySignature(r, d.PublicKey, localAgentBodyLimit); err != nil {
			writeErr(w, 401, err.Error())
			return
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
		_, _ = pg.Exec(`UPDATE local_agent_devices SET last_used_at=now() WHERE id=$1`, id)
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
	writeJSON(w, 200, map[string]any{"id": d.ID, "name": d.Name, "status": d.Status, "scopes": d.Scopes})
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
