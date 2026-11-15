package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

const mcpFileLimit = 8 << 20

func mcpAccessRequest(ctx context.Context, t mcpAccessTool, raw json.RawMessage) (*http.Request, error) {
	var args map[string]json.RawMessage
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return nil, errors.New("参数必须是 JSON 对象")
	}
	allowed := map[string]bool{"query": true, "request_id": true}
	if t.Method != "GET" && !t.Upload {
		allowed["body"] = true
	}
	if t.Upload {
		allowed["filename"] = true
		allowed["content_base64"] = true
	}
	path := t.Path
	values := map[string]string{}
	for _, m := range mcpPathParam.FindAllStringSubmatch(t.Path, -1) {
		allowed[m[1]] = true
		var v string
		if json.Unmarshal(args[m[1]], &v) != nil || v == "" || strings.ContainsAny(v, "\\\x00\r\n") {
			return nil, fmt.Errorf("路径参数 %s 无效", m[1])
		}
		segments := strings.Split(v, "/")
		if m[2] == "" && len(segments) != 1 {
			return nil, errors.New("路径参数不能包含目录分隔符")
		}
		for _, segment := range segments {
			if segment == "" || segment == "." || segment == ".." {
				return nil, errors.New("路径参数不能跨越目录")
			}
		}
		encoded := make([]string, len(segments))
		for i, segment := range segments {
			encoded[i] = url.PathEscape(segment)
		}
		path = strings.ReplaceAll(path, m[0], strings.Join(encoded, "/"))
		values[m[1]] = v
	}
	for name := range args {
		if !allowed[name] {
			return nil, fmt.Errorf("未知参数 %s", name)
		}
	}
	query := url.Values{}
	if v, ok := args["query"]; ok {
		var q map[string]string
		if json.Unmarshal(v, &q) != nil || q == nil {
			return nil, errors.New("query 必须是字符串字典")
		}
		for k, v := range q {
			query.Set(k, v)
		}
	}
	if t.EnsureFindingsPagination && query.Get("task") == "" && query.Get("page") == "" && query.Get("limit") == "" {
		query.Set("limit", "20")
	}
	var body io.Reader
	contentType := "application/json"
	if t.Upload {
		var filename, encoded string
		if json.Unmarshal(args["filename"], &filename) != nil || filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\\x00\r\n\"") {
			return nil, errors.New("文件名无效")
		}
		if json.Unmarshal(args["content_base64"], &encoded) != nil {
			return nil, errors.New("缺少 base64 内容")
		}
		if len(encoded) > base64.StdEncoding.EncodedLen(mcpFileLimit) {
			return nil, errors.New("文件超过 8 MiB")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) > mcpFileLimit {
			return nil, errors.New("base64 内容无效或文件过大")
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			return nil, err
		}
		if _, err = part.Write(data); err != nil {
			return nil, err
		}
		if err = mw.Close(); err != nil {
			return nil, err
		}
		body = &buf
		contentType = mw.FormDataContentType()
	} else if v, ok := args["body"]; ok {
		// Some existing APIs accept arrays (assets, model lists), so preserve JSON values.
		if !json.Valid(v) {
			return nil, errors.New("body 不是合法 JSON")
		}
		if t.InjectRequestID {
			var body map[string]json.RawMessage
			if json.Unmarshal(v, &body) != nil || body == nil {
				return nil, errors.New("此工具 body 必须是 JSON 对象")
			}
			var outerID string
			if json.Unmarshal(args["request_id"], &outerID) != nil || outerID == "" {
				return nil, errors.New("request_id 无效")
			}
			if nested, ok := body["request_id"]; ok {
				var nestedID string
				if json.Unmarshal(nested, &nestedID) != nil || nestedID != outerID {
					return nil, errors.New("body.request_id 必须与调用级 request_id 一致")
				}
			} else {
				body["request_id"], _ = json.Marshal(outerID)
			}
			v, _ = json.Marshal(body)
		}
		body = bytes.NewReader(v)
	} else if t.Method != "GET" {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, "http://mcp.internal"+path+"?"+query.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range values {
		req.SetPathValue(k, v)
	}
	return req, nil
}

// A bounded writer avoids buffering arbitrary workspace downloads or long logs.
type mcpResponseWriter struct {
	header   http.Header
	body     bytes.Buffer
	code     int
	overflow bool
}

func (w *mcpResponseWriter) Header() http.Header { return w.header }
func (w *mcpResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *mcpResponseWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	if w.body.Len()+len(p) > mcpFileLimit {
		w.overflow = true
		return 0, errors.New("MCP response exceeds 8 MiB")
	}
	return w.body.Write(p)
}

func invokeMCPAccessTool(ctx context.Context, t mcpAccessTool, raw json.RawMessage) (json.RawMessage, int, error) {
	req, err := mcpAccessRequest(ctx, t, raw)
	if err != nil {
		return nil, 400, err
	}
	defer func() {
		if req.MultipartForm != nil {
			_ = req.MultipartForm.RemoveAll()
		}
	}()
	w := &mcpResponseWriter{header: make(http.Header)}
	t.Handler(w, req)
	if w.code == 0 {
		w.code = 200
	}
	if w.overflow {
		return nil, 502, errors.New("响应超过 8 MiB，请缩小查询范围或通过网页下载")
	}
	if t.Download && w.code < 400 {
		data, err := json.Marshal(map[string]any{"content_base64": base64.StdEncoding.EncodeToString(w.body.Bytes()), "content_type": w.header.Get("Content-Type"), "content_disposition": w.header.Get("Content-Disposition")})
		return data, w.code, err
	}
	if json.Valid(w.body.Bytes()) {
		return append(json.RawMessage(nil), w.body.Bytes()...), w.code, nil
	}
	data, err := json.Marshal(map[string]any{"text": w.body.String()})
	return data, w.code, err
}
