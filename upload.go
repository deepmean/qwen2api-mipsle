package main

// Agent long-context externalization: upload a conversation to Qwen as a document
// attachment and reference it from a small inline prompt.
//
// Ported from Qwen2API src/utils/upload.js. Qwen's edge (Aliyun WAF) rejects a
// request body at roughly 128 KiB, and an agent client like Claude Code resends
// its whole conversation every turn — so the body crosses that line within a few
// exchanges. Upstream avoids the ceiling by putting the history in a Qwen
// document instead of the request body:
//
//	POST /api/v1/files/getstsToken   -> temporary Aliyun OSS credentials + a key
//	PUT  https://<bucket>.<endpoint>/<key>  (signed with those credentials)
//	POST /api/v2/files/parse         -> ask Qwen to parse the uploaded document
//	POST /api/v2/files/parse/status  -> poll until parsing finishes
//
// The parsed document is then referenced from message.files, and the model reads
// it as context. Only the parse calls are rate-limited by the WAF (they are
// counted per egress IP), which is why attachment.go caches the uploaded prefix
// and reuses it across turns.

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// maxUploadBytes mirrors upstream UPLOAD_CONFIG.maxFileSize.
const maxUploadBytes = 100 * 1024 * 1024

// stsResponse is the getstsToken reply. The endpoint wraps it as {success,data}
// in current FE builds (0.3.12) and returns the fields directly in older builds,
// so both shapes are accepted.
type stsResponse struct {
	Success         *bool        `json:"success"`
	AccessKeyID     string       `json:"access_key_id"`
	AccessKeySecret string       `json:"access_key_secret"`
	SecurityToken   string       `json:"security_token"`
	FileURL         string       `json:"file_url"`
	FilePath        string       `json:"file_path"`
	BucketName      string       `json:"bucketname"`
	Endpoint        string       `json:"endpoint"`
	Region          string       `json:"region"`
	FileID          string       `json:"file_id"`
	Data            *stsResponse `json:"data"`
}

// unwrap returns the inner payload when the reply used the {success,data} envelope.
func (s *stsResponse) unwrap() *stsResponse {
	if s.Data != nil && s.Data.AccessKeyID != "" {
		return s.Data
	}
	return s
}

// uploadAgentContext uploads text to Qwen's OSS, asks Qwen to parse it, and
// returns the message.files descriptor that references the parsed document.
func uploadAgentContext(ctx context.Context, c *qwenClient, acct *account, text, filename string) (map[string]any, error) {
	content := []byte(text)
	if len(content) == 0 {
		return nil, fmt.Errorf("agent context is empty")
	}
	if len(content) > maxUploadBytes {
		return nil, fmt.Errorf("agent context exceeds %d bytes", maxUploadBytes)
	}
	if filename == "" {
		filename = fmt.Sprintf("QWEN2API_AGENT_HISTORY_%d.txt", time.Now().UnixMilli())
	}

	sts, err := c.requestSTSToken(ctx, acct, filename, len(content), "file")
	if err != nil {
		return nil, err
	}
	if err := c.putObject(ctx, sts, content); err != nil {
		return nil, err
	}
	if err := c.parseUploadedFile(ctx, acct, sts.FileID); err != nil {
		return nil, err
	}
	return buildChatFileDescriptor(sts.FileID, sts.FileURL, filename, len(content)), nil
}

// uploadMedia uploads binary media — the input picture of an image-edit turn —
// to Qwen's OSS and returns the message.files entry that references it.
//
// Two differences from uploadAgentContext:
//
//   - NO parse step. A document must be parsed before the model can read it; an
//     image is fetched from the stored object directly, and the upstream has no
//     parse path for it.
//   - The descriptor is the short {type,url} pair. That is the only shape the
//     upstream accepts for an image in files[] (verified upstream: it is what the
//     Qwen web client's image_edit path pushes).
//
// The caller must supply the real contentType: it is signed into the OSS
// request and determines the stored object's type.
func uploadMedia(ctx context.Context, c *qwenClient, acct *account, data []byte, filename, contentType string) (map[string]any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("media is empty")
	}
	if len(data) > maxUploadBytes {
		return nil, fmt.Errorf("media exceeds %d bytes", maxUploadBytes)
	}
	if filename == "" {
		filename = uuidv4() + ".png"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	sts, err := c.requestSTSToken(ctx, acct, filename, len(data), simpleFileType(contentType))
	if err != nil {
		return nil, err
	}
	if err := c.putObjectTyped(ctx, sts, data, contentType); err != nil {
		return nil, err
	}
	if sts.FileURL == "" {
		return nil, fmt.Errorf("media upload returned no file url")
	}
	return map[string]any{"type": "image", "url": sts.FileURL}, nil
}

// simpleFileType maps a MIME type onto the simple type getstsToken expects.
// Mirrors upstream getSimpleFileType: the main type when it is a type the
// endpoint knows, otherwise "file".
func simpleFileType(contentType string) string {
	main := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, "/", 2)[0]))
	switch main {
	case "image", "video", "audio":
		return main
	default:
		return "file"
	}
}

// requestSTSToken asks Qwen for temporary OSS credentials scoped to one object.
//
// filetype is the *simple* type the upstream expects ("file", "image", "video",
// ...), which it uses to size the upload allowance. It defaults to "file", which
// is what the document path has always sent.
func (c *qwenClient) requestSTSToken(ctx context.Context, acct *account, filename string, filesize int, filetype string) (*stsResponse, error) {
	if filetype == "" {
		filetype = "file"
	}
	payload, _ := json.Marshal(map[string]any{
		"filename": filename,
		"filesize": filesize,
		"filetype": filetype,
	})

	url := chatBase() + "/api/v1/files/getstsToken"
	headers := buildHeaders(acct, acct.token, "application/json, text/plain, */*", "/")
	headers["authorization"] = "Bearer " + acct.token

	resp, err := c.hc.do(ctx, "POST", url, headers, payload, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	body, err := readAllLimited(resp.Body, 64<<10)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &upstreamError{Status: resp.StatusCode, Body: string(body), URL: url}
	}

	var parsed stsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("getstsToken: bad json: %w", err)
	}
	s := parsed.unwrap()
	if s.Success != nil && !*s.Success {
		return nil, fmt.Errorf("getstsToken: upstream refused the request")
	}
	if s.AccessKeyID == "" || s.AccessKeySecret == "" || s.SecurityToken == "" ||
		s.FilePath == "" || s.BucketName == "" || s.Endpoint == "" || s.FileID == "" {
		return nil, fmt.Errorf("getstsToken: incomplete credentials in reply")
	}
	return s, nil
}

// putObject uploads a document to Aliyun OSS as text/plain.
func (c *qwenClient) putObject(ctx context.Context, sts *stsResponse, content []byte) error {
	return c.putObjectTyped(ctx, sts, content, "text/plain")
}

// putObjectTyped uploads bytes to Aliyun OSS with a Signature-V1 Authorization
// header, which is what the STS credentials and the upstream ali-oss client use.
//
// The canonical string is:
//
//	PUT\n
//	<Content-MD5>\n
//	<Content-Type>\n
//	<Date>\n
//	<CanonicalizedOSSHeaders>/<bucket>/<key>
//
// contentType is part of the signature, so it must be the value actually sent:
// an image uploaded as text/plain would be signed correctly but stored with the
// wrong type, and Qwen reads the stored type when the model fetches it.
func (c *qwenClient) putObjectTyped(ctx context.Context, sts *stsResponse, content []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	host := sts.BucketName + "." + sts.Endpoint
	url := "https://" + host + "/" + sts.FilePath

	date := time.Now().UTC().Format(httpTimeFormat)

	canonical := "PUT\n\n" + contentType + "\n" + date + "\n" +
		"x-oss-security-token:" + sts.SecurityToken + "\n" +
		"/" + sts.BucketName + "/" + sts.FilePath
	mac := hmac.New(sha1.New, []byte(sts.AccessKeySecret))
	mac.Write([]byte(canonical))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	headers := map[string]string{
		"Date":                 date,
		"Content-Type":         contentType,
		"x-oss-security-token": sts.SecurityToken,
		"Authorization":        "OSS " + sts.AccessKeyID + ":" + signature,
	}

	resp, err := c.hc.do(ctx, "PUT", url, headers, content, 60*time.Second)
	if err != nil {
		return fmt.Errorf("oss put: %w", err)
	}
	defer resp.Close()
	if resp.StatusCode != 200 {
		body, _ := readAllLimited(resp.Body, 4<<10)
		return fmt.Errorf("oss put: status %d: %s", resp.StatusCode, truncateForLog(string(body), 300))
	}
	return nil
}

// parseUploadedFile asks Qwen to parse the document and waits for it to finish.
// The parse POST is the call the WAF rate-limits, so callers should cache the
// resulting descriptor rather than re-parsing every turn.
func (c *qwenClient) parseUploadedFile(ctx context.Context, acct *account, fileID string) error {
	if _, err := c.postParse(ctx, acct, "/api/v2/files/parse", map[string]any{"file_id": fileID}); err != nil {
		return err
	}

	// Upstream polls 30 times at 500 ms. The first poll often already reports
	// success for small documents, so a shorter overall budget is usually enough.
	const maxAttempts = 30
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, err := c.postParse(ctx, acct, "/api/v2/files/parse/status",
			map[string]any{"file_id_list": []string{fileID}})
		if err != nil {
			return err
		}
		status := parseStatusOf(body, fileID)
		switch status {
		case "success", "completed", "done":
			return nil
		case "failed", "error":
			return fmt.Errorf("qwen document parse failed for %s", fileID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("qwen document parse timed out for %s", fileID)
}

// postParse issues one of the document-parse calls and returns the decoded body.
// A non-JSON body means the WAF answered with its captcha page, which must be
// surfaced as such rather than as a generic parse failure.
func (c *qwenClient) postParse(ctx context.Context, acct *account, path string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	url := chatBase() + path
	headers := buildHeaders(acct, acct.token, "application/json, text/plain, */*", "/")
	headers["authorization"] = "Bearer " + acct.token

	resp, err := c.hc.do(ctx, "POST", url, headers, body, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	raw, err := readAllLimited(resp.Body, 64<<10)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &upstreamError{Status: resp.StatusCode, Body: string(raw), URL: url}
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// The parse endpoints answer a challenged request with an HTML captcha
		// page under HTTP 200, so a decode failure here is the WAF, not the
		// document. Report it distinctly so the caller can back off.
		if looksLikeWAFPage(raw) {
			return nil, fmt.Errorf("%w: document parse", errWAFChallenge)
		}
		return nil, fmt.Errorf("%s: bad json: %w", path, err)
	}
	if success, ok := parsed["success"].(bool); ok && !success {
		code := ""
		if data, ok := parsed["data"].(map[string]any); ok {
			code, _ = data["code"].(string)
		}
		return nil, fmt.Errorf("%s: upstream reported failure (%s)", path, code)
	}
	return parsed, nil
}

// parseStatusOf extracts the per-file parse status from a status reply. The
// payload is a list under `data`, but older builds return a bare object.
func parseStatusOf(body map[string]any, fileID string) string {
	records, _ := body["data"].([]any)
	for _, item := range records {
		record, _ := item.(map[string]any)
		if record == nil {
			continue
		}
		if id, _ := record["file_id"].(string); id == fileID {
			s, _ := record["status"].(string)
			return strings.ToLower(strings.TrimSpace(s))
		}
	}
	if data, ok := body["data"].(map[string]any); ok {
		s, _ := data["status"].(string)
		return strings.ToLower(strings.TrimSpace(s))
	}
	return ""
}

// looksLikeWAFPage reports whether a non-JSON body is an Aliyun WAF captcha page.
func looksLikeWAFPage(body []byte) bool {
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := strings.ToLower(string(head))
	return strings.Contains(lower, "aliyun_waf") ||
		strings.Contains(lower, "aliyuncaptcha") ||
		strings.Contains(lower, "<!doctype html") ||
		strings.Contains(lower, "<html")
}

// buildChatFileDescriptor builds the message.files entry that references a
// parsed document, matching Qwen Web 0.3.12 (upload.js#buildChatFileDescriptor).
func buildChatFileDescriptor(fileID, fileURL, filename string, size int) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"type": "file",
		"file": map[string]any{
			"created_at": now,
			"data":       map[string]any{},
			"filename":   filename,
			"hash":       nil,
			"id":         fileID,
			"meta": map[string]any{
				"name":         filename,
				"size":         size,
				"content_type": "text/plain",
				"parse_meta":   map[string]any{"parse_status": "success"},
			},
			"update_at": now,
			"name":      filename,
			"size":      size,
			"type":      "text/plain",
		},
		"id":              fileID,
		"url":             fileURL,
		"name":            filename,
		"collection_name": "",
		"status":          "uploaded",
		"progress":        100,
		"greenNet":        "success",
		"size":            size,
		"error":           "",
		"itemId":          fileID,
		"file_type":       "text/plain",
		"showType":        "file",
		"file_class":      "document",
		"context":         "full",
	}
}

// httpTimeFormat is RFC 1123 in GMT, which is what OSS expects in the Date
// header and includes verbatim in the signature.
const httpTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"

// errWAFChallenge marks a request the WAF answered with a challenge instead of
// the expected payload.
var errWAFChallenge = fmt.Errorf("upstream WAF challenge")
