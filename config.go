package main

// Configuration and account loading.
//
// Mirrors the subset of Qwen2API's config/index.js + data-persistence.js that the
// core chat path needs. Accounts are read from a plain text file, one per line:
//
//     email:password
//
// A `#`-prefixed line or blank line is ignored, so the file can carry comments.
// The password may itself contain ':' (upstream splits on the first one).

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

type config struct {
	// ListenPort is the TCP port the proxy serves on.
	ListenPort int
	// APIKeys are the accepted inbound bearer tokens; the first is the admin key.
	APIKeys []string
	// AdminKey is the first entry of APIKeys.
	AdminKey string
	// AccountsFile holds `email:password` lines.
	AccountsFile string
	// QwenChatBase is the upstream web API origin.
	QwenChatBase string
	// RequestTimeoutSec bounds a single upstream chat request (thinking models
	// routinely exceed 60s before the first answer token).
	RequestTimeoutSec int
	// MaxConcurrent bounds in-flight chat requests. The device has ~80MB free
	// RAM; each streamed answer costs a few hundred KB of buffers.
	MaxConcurrent int
	// PrefixCacheTTLSec bounds how long an uploaded history attachment may be
	// reused. An expired Qwen file_id does not error — the model just answers
	// without it — so reuse is capped by the document's own lifetime.
	PrefixCacheTTLSec int
	// PrefixCacheMaxEntries caps how many sessions' attachments are remembered.
	PrefixCacheMaxEntries int
	// LogLevel is one of DEBUG/INFO/WARN/ERROR.
	LogLevel string
}

func defaultConfig() config {
	return config{
		ListenPort:            3000,
		QwenChatBase:          "https://chat.qwen.ai",
		RequestTimeoutSec:     600,
		MaxConcurrent:         8,
		PrefixCacheTTLSec:     1800,
		PrefixCacheMaxEntries: 8,
		LogLevel:              "INFO",
	}
}

// loadConfig applies environment overrides on top of the defaults, matching the
// env names the upstream project documents where they overlap.
func loadConfig() (config, error) {
	c := defaultConfig()

	if v := os.Getenv("SERVICE_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 && p < 65536 {
			c.ListenPort = p
		}
	}
	if v := os.Getenv("API_KEY"); v != "" {
		for _, k := range strings.Split(v, ",") {
			k = strings.TrimSpace(k)
			if k == "" {
				return c, fmt.Errorf("API_KEY contains an empty slot (check that every variable expanded)")
			}
			c.APIKeys = append(c.APIKeys, k)
		}
		c.AdminKey = c.APIKeys[0]
	}
	if v := os.Getenv("QWEN_CHAT_PROXY_URL"); v != "" {
		c.QwenChatBase = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("ACCOUNTS_FILE"); v != "" {
		c.AccountsFile = v
	}
	if v := os.Getenv("MAX_CONCURRENT"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("AGENT_CONTEXT_PREFIX_TTL_SECONDS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			c.PrefixCacheTTLSec = n
		}
	}
	if v := os.Getenv("AGENT_CONTEXT_PREFIX_MAX_ENTRIES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.PrefixCacheMaxEntries = n
		}
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		c.LogLevel = strings.ToUpper(v)
	}
	return c, nil
}

// account is one Qwen credential plus the identity derived from it.
type account struct {
	Email string
	Pass  string

	// token is the JWT from /api/v1/auths/signin.
	token string
	// expires is the JWT `exp` claim (unix seconds), 0 when unknown.
	expires int64
	// fp is the deterministic browser identity bound to this email.
	fp *fingerprint
}

// loadAccounts parses the credentials file. Missing file is not fatal: the
// service starts and reports "no accounts" through /health, so credentials can
// be dropped in later without a rebuild.
func loadAccounts(path string) ([]*account, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return parseAccounts(f)
}

// parseAccounts reads `email:password` lines from r.
func parseAccounts(r io.Reader) ([]*account, error) {
	var out []*account
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon <= 0 {
			return nil, fmt.Errorf("line %d: expected email:password", lineNo)
		}
		email := strings.TrimSpace(line[:colon])
		pass := strings.TrimSpace(line[colon+1:])
		if email == "" || pass == "" {
			return nil, fmt.Errorf("line %d: empty email or password", lineNo)
		}
		out = append(out, &account{
			Email: email,
			Pass:  pass,
			fp:    generateDeterministicFingerprint(email),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
