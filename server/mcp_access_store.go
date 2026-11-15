package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type mcpAccessKey struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type mcpAccessCall struct {
	ID          int64      `json:"id"`
	KeyID       int64      `json:"key_id"`
	Tool        string     `json:"tool"`
	RequestID   string     `json:"request_id"`
	Status      string     `json:"status"`
	StatusCode  int        `json:"status_code"`
	DurationMS  int64      `json:"duration_ms"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

func newMCPAccessToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token := "artex_mcp_" + base64.RawURLEncoding.EncodeToString(b)
	return token, mcpAccessTokenHash(token), nil
}

func mcpAccessTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func validateMCPAccessScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("至少选择一项权限")
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if !localAgentScopes[scope] || seen[scope] {
			return errors.New("权限无效或重复")
		}
		seen[scope] = true
	}
	return nil
}

func (k mcpAccessKey) allows(scope string) bool {
	if scope == "" {
		return true
	}
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type mcpRowScanner interface{ Scan(...any) error }

const mcpKeyColumns = `id,name,prefix,to_json(scopes),created_at,expires_at,revoked_at,last_used_at`
const mcpCallColumns = `id,key_id,tool,COALESCE(request_id,''),status,status_code,duration_ms,created_at,completed_at`

func scanMCPKey(row mcpRowScanner) (k mcpAccessKey, err error) {
	var scopes []byte
	err = row.Scan(&k.ID, &k.Name, &k.Prefix, &scopes, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt)
	if err == nil {
		err = json.Unmarshal(scopes, &k.Scopes)
	}
	return
}

func scanMCPCall(row mcpRowScanner) (c mcpAccessCall, err error) {
	err = row.Scan(&c.ID, &c.KeyID, &c.Tool, &c.RequestID, &c.Status, &c.StatusCode, &c.DurationMS, &c.CreatedAt, &c.CompletedAt)
	return
}

func (s *Server) authenticateMCPAccess(ctx context.Context, token string) (mcpAccessKey, error) {
	if s.m == nil || s.m.pg == nil {
		return mcpAccessKey{}, errors.New("数据源不可用")
	}
	return scanMCPKey(s.m.pg.QueryRowContext(ctx, `UPDATE mcp_access_keys SET last_used_at=now()
 WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>now() RETURNING `+mcpKeyColumns, mcpAccessTokenHash(token)))
}
