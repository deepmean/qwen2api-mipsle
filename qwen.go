package main

// Upstream Qwen web client.
//
// Ported from Qwen2API src/utils/request.js, src/utils/header-profile.js,
// src/utils/token-manager.js and src/models/models-map.js. Four calls make up
// the whole core surface:
//
//	POST /api/v1/auths/signin          -> JWT (password sent as sha256 hex)
//	POST /api/v2/chats/new             -> chat_id
//	POST /api/v2/chat/completions?chat_id=<id>  -> SSE stream (always streamed)
//	GET  /api/models                   -> model catalogue
//
// The browser headers are not decoration: Qwen's WAF fingerprints the request
// (UA / sec-ch-ua / sec-fetch-* / timezone / ssxmod cookies). header-profile.js
// keeps them consistent per account, and so does buildHeaders below.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// chromeVersions mirrors header-profile.js CHROME_VERSIONS.
var chromeVersions = []string{"149.0.0.0", "148.0.0.0", "147.0.0.0", "146.0.0.0"}

// frontendVersion is the `version` header the web client sends (FE 0.2.81).
const frontendVersion = "0.2.81"

// defaultChatBase is upstream's origin, used when the configuration did not
// supply one (also keeps the header builders usable in isolation).
const defaultChatBase = "https://chat.qwen.ai"

// chatBase returns the configured upstream origin, trimmed of a trailing slash.
func chatBase() string {
	base := currentConfig.QwenChatBase
	if base == "" {
		base = defaultChatBase
	}
	return strings.TrimRight(base, "/")
}

func pickChromeVersion(fp *fingerprint) string {
	if fp == nil || len(fp.hash) < 32 {
		return chromeVersions[0]
	}
	idx, err := strconv.ParseInt(fp.hash[30:32], 16, 32)
	if err != nil {
		return chromeVersions[0]
	}
	return chromeVersions[int(idx)%len(chromeVersions)]
}

func platformToSecChUA(platform string) string {
	switch {
	case platform == "MacIntel":
		return `"macOS"`
	case platform == "Win32":
		return `"Windows"`
	case strings.HasPrefix(platform, "Linux"):
		return `"Linux"`
	default:
		return `"Windows"`
	}
}

func buildUserAgent(fp *fingerprint) string {
	ver := pickChromeVersion(fp)
	var platform string
	if fp != nil && len(fp.raw) > 10 {
		platform = fp.raw[10]
	}
	switch {
	case platform == "MacIntel":
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + " Safari/537.36"
	case strings.HasPrefix(platform, "Linux"):
		return "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + " Safari/537.36"
	default:
		return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + " Safari/537.36"
	}
}

func buildSecChUA(fp *fingerprint) string {
	major := strings.SplitN(pickChromeVersion(fp), ".", 2)[0]
	return `"Google Chrome";v="` + major + `", "Chromium";v="` + major + `", "Not)A;Brand";v="24"`
}

// timezoneHeader renders the `timezone` request header. header-profile.js derives
// this from the fingerprint's UTC offset (e.g. offset -480 -> "UTC+08") rather
// than from the host clock, so the header stays consistent with the locale baked
// into the fingerprint. Deriving it from the router's own clock would depend on
// the container carrying tzdata, which OpenWrt does not ship.
func timezoneHeader(fp *fingerprint) string {
	offset := -480 // zh-CN default, matching LANGUAGE_PRESETS
	if fp != nil && len(fp.raw) > 6 {
		if v, err := strconv.Atoi(strings.TrimSpace(fp.raw[6])); err == nil {
			offset = v
		}
	}
	sign := "+"
	if offset > 0 {
		sign = "-"
	}
	hours := offset / 60
	if hours < 0 {
		hours = -hours
	}
	return fmt.Sprintf("UTC%s%02d", sign, hours)
}

// uuidv4 generates a random RFC 4122 v4 identifier.
func uuidv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable in any useful way; fall back to
		// a timestamp-derived value so the request still carries an id.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildHeaders assembles the browser profile for an upstream call. accept is the
// per-call Accept value; refererPath is appended to the base origin.
func buildHeaders(acct *account, token string, accept, refererPath string) map[string]string {
	fp := acct.fp

	ua := buildUserAgent(fp)
	secChUA := buildSecChUA(fp)
	var secChPlatform, language string
	if fp != nil && len(fp.raw) > 10 {
		secChPlatform = platformToSecChUA(fp.raw[10])
		language = fp.raw[5]
	} else {
		secChPlatform, language = `"Windows"`, "zh-CN"
	}

	acceptLanguage := language
	if strings.Contains(language, "-") {
		acceptLanguage = language + "," + strings.SplitN(language, "-", 2)[0] + ";q=0.9"
	}

	base := chatBase()
	if refererPath == "" {
		refererPath = "/"
	}

	ssx := fp.generateCookies()

	h := map[string]string{
		"sec-ch-ua-platform": secChPlatform,
		"accept-language":    acceptLanguage,
		"sec-ch-ua":          secChUA,
		"sec-ch-ua-mobile":   "?0",
		"user-agent":         ua,
		"content-type":       "application/json",
		"accept":             accept,
		"accept-encoding":    "gzip, deflate",
		"source":             "web",
		"version":            frontendVersion,
		"timezone":           timezoneHeader(fp),
		"connection":         "keep-alive",
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
		"referer":            base + refererPath,
		"origin":             base,
		"x-request-id":       uuidv4(),
		"cookie":             "token=" + token + ";ssxmod_itna=" + ssx.Itna + ";ssxmod_itna2=" + ssx.Itna2,
	}
	return h
}

// upstreamError carries a non-200 reply in a form the handlers can log and map
// back to the client.
type upstreamError struct {
	Status int
	Body   string
	URL    string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream %s returned %d: %s", e.URL, e.Status, truncateForLog(e.Body, 300))
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// signIn exchanges email+password for a JWT. The password is sent as its SHA-256
// hex digest, matching token-manager.js.
func (c *qwenClient) signIn(ctx context.Context, acct *account) (string, int64, error) {
	sum := sha256.Sum256([]byte(acct.Pass))
	payload, _ := json.Marshal(map[string]string{
		"email":    acct.Email,
		"password": hex.EncodeToString(sum[:]),
	})

	url := chatBase() + "/api/v1/auths/signin"
	// The signin call only carries a UA upstream; sending the full browser
	// profile here is harmless and keeps the egress fingerprint uniform.
	headers := map[string]string{
		"user-agent":      buildUserAgent(acct.fp),
		"content-type":    "application/json",
		"accept":          "application/json",
		"accept-language": "zh-CN,zh;q=0.9",
	}

	resp, err := c.hc.do(ctx, "POST", url, headers, payload, 20*time.Second)
	if err != nil {
		return "", 0, err
	}
	defer resp.Close()

	body, err := readAllLimited(resp.Body, 64<<10)
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode != 200 {
		return "", 0, &upstreamError{Status: resp.StatusCode, Body: string(body), URL: url}
	}

	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("signin: bad json: %w", err)
	}
	if parsed.Token == "" {
		return "", 0, fmt.Errorf("signin: response carried no token")
	}
	return parsed.Token, jwtExpiry(parsed.Token), nil
}

// jwtExpiry reads the `exp` claim without verifying the signature — the token
// comes from Qwen over TLS, so this is a freshness check, not an auth check.
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.Exp
}

// createChatID opens a new conversation and returns its id.
func (c *qwenClient) createChatID(ctx context.Context, acct *account, model, chatType string) (string, error) {
	if chatType == "" {
		chatType = "t2t"
	}
	payload, _ := json.Marshal(map[string]any{
		"chatId":     "",
		"models":     []string{model},
		"project_id": "",
		"timestamp":  time.Now().UnixMilli(),
		"chat_type":  chatType,
		"chat_mode":  "normal",
	})

	url := chatBase() + "/api/v2/chats/new"
	headers := buildHeaders(acct, acct.token, "application/json, text/plain, */*", "/c/new-chat")

	resp, err := c.hc.do(ctx, "POST", url, headers, payload, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Close()

	body, err := readAllLimited(resp.Body, 64<<10)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", &upstreamError{Status: resp.StatusCode, Body: string(body), URL: url}
	}

	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("chats/new: bad json: %w", err)
	}
	if parsed.Data.ID == "" {
		return "", fmt.Errorf("chats/new: response carried no chat id")
	}
	return parsed.Data.ID, nil
}

// chatStream opens the completions stream. The caller owns the returned
// response and must Close it.
func (c *qwenClient) chatStream(ctx context.Context, acct *account, chatID string, payload []byte) (*response, error) {
	url := chatBase() + "/api/v2/chat/completions?chat_id=" + chatID
	headers := buildHeaders(acct, acct.token, "application/json", "/c/"+chatID)
	headers["x-accel-buffering"] = "no"
	return c.hc.do(ctx, "POST", url, headers, payload, time.Duration(currentConfig.RequestTimeoutSec)*time.Second)
}

// listModels fetches the catalogue. A failure is not fatal: the handler falls
// back to a static list so clients can still start.
func (c *qwenClient) listModels(ctx context.Context, acct *account) ([]modelInfo, error) {
	url := chatBase() + "/api/models"
	headers := buildHeaders(acct, acct.token, "*/*", "/")

	resp, err := c.hc.do(ctx, "GET", url, headers, nil, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	body, err := readAllLimited(resp.Body, 512<<10)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &upstreamError{Status: resp.StatusCode, Body: string(body), URL: url}
	}

	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Info struct {
				Meta struct {
					ChatType []string `json:"chat_type"`
				} `json:"meta"`
			} `json:"info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("models: bad json: %w", err)
	}

	out := make([]modelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, modelInfo{ID: m.ID, Name: name, ChatTypes: m.Info.Meta.ChatType})
	}
	return out, nil
}

type modelInfo struct {
	ID        string
	Name      string
	ChatTypes []string
}

// fallbackModels is served when the catalogue cannot be fetched, so a client
// that only wants to send one chat request is never blocked by discovery.
var fallbackModels = []modelInfo{
	{ID: "qwen3.6-plus", Name: "qwen3.6-plus", ChatTypes: []string{"t2t"}},
	{ID: "qwen3.5-plus", Name: "qwen3.5-plus", ChatTypes: []string{"t2t"}},
	{ID: "qwen3.6-flash", Name: "qwen3.6-flash", ChatTypes: []string{"t2t"}},
	{ID: "coder-model", Name: "coder-model", ChatTypes: []string{"t2t"}},
}
